// Package blob is luxd's view of the blob store: S3.
//
// There is no control-plane disk tier and runners never hold S3
// credentials. A runner uploads straight to S3 with presigned PutObject or
// UploadPart URLs luxd signs for it, each carrying the body's SHA-256 so S3
// refuses any other bytes; luxd still accepts a streamed upload (Put) from
// runners that predate this. Downloads are presigned GET URLs for exactly
// the blobs a runner or client may read.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/marcioapm/lux/internal/proto"
)

type Config struct {
	Endpoint  string // empty for AWS
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PublicEndpoint, if set, is the endpoint presigned URLs are signed for.
	PublicEndpoint string
}

type Store struct {
	s3      *s3.Client
	presign *s3.PresignClient
	tm      *transfermanager.Client
	bucket  string
}

// New builds the store. Credentials come from the AWS default chain
// (environment, shared config, instance role); static keys in cfg, when
// set, take precedence over it.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob: bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	load := []func(*config.LoadOptions) error{config.WithRegion(cfg.Region)}
	if cfg.AccessKey != "" {
		load = append(load, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, load...)
	if err != nil {
		return nil, fmt.Errorf("blob: aws config: %w", err)
	}
	// Both clients share every option but the endpoint, so presigned URLs
	// are built exactly like the uploads they stand for.
	opts := func(endpoint string) func(*s3.Options) {
		return func(o *s3.Options) {
			// Path-style so MinIO and custom endpoints work without DNS tricks.
			o.UsePathStyle = cfg.Endpoint != ""
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
		}
	}
	client := s3.NewFromConfig(awsCfg, opts(cfg.Endpoint))
	pub := client
	if cfg.PublicEndpoint != "" {
		pub = s3.NewFromConfig(awsCfg, opts(cfg.PublicEndpoint))
	}
	return &Store{s3: client, presign: s3.NewPresignClient(pub), tm: transfermanager.New(client), bucket: cfg.Bucket}, nil
}

// Key is where a blob lives: scoped by tenant and Run.
func Key(tenantID, runID, blobID string) string {
	return fmt.Sprintf("tenants/%s/runs/%s/%s", url.PathEscape(tenantID), url.PathEscape(runID), url.PathEscape(blobID))
}

// Put streams body to key.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, sha256hex string) error {
	// The body is a stream from the runner, not seekable: the transfer
	// manager uploads it in parts without buffering the whole blob.
	_, err := s.tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: map[string]string{"sha256": sha256hex},
	})
	return err
}

// Get opens a blob for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, 0, err
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

// PresignGet returns a URL valid for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// presigned is a presigned request: its URL and every header the client
// must send unchanged for the signature to hold.
func presigned(url string, signed http.Header) proto.PresignedRequest {
	h := map[string]string{}
	for k, v := range signed {
		// Host is the URL's; Go's client sends it from there.
		if !strings.EqualFold(k, "Host") && len(v) > 0 {
			h[http.CanonicalHeaderKey(k)] = strings.Join(v, ",")
		}
	}
	return proto.PresignedRequest{URL: url, Headers: h}
}

// PresignPut presigns a PutObject of exactly size bytes whose SHA-256 is
// sha256hex (sha256b64 the same digest in base64): the checksum, the
// sha256 metadata and the length are signed, so S3 refuses any other body.
func (s *Store) PresignPut(ctx context.Context, key string, size int64, sha256hex, sha256b64 string, ttl time.Duration) (proto.PresignedRequest, error) {
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(sha256b64),
		Metadata:       map[string]string{"sha256": sha256hex},
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return proto.PresignedRequest{}, err
	}
	return presigned(req.URL, req.SignedHeader), nil
}

// CreateMultipart starts a multipart upload whose parts each carry a
// SHA-256 checksum. Returns its upload id.
func (s *Store) CreateMultipart(ctx context.Context, key, sha256hex string) (string, error) {
	out, err := s.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Metadata:          map[string]string{"sha256": sha256hex},
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.UploadId), nil
}

// PresignUploadPart presigns part number (1-based) of a multipart upload:
// size bytes whose SHA-256 is sha256b64 (base64), both signed.
func (s *Store) PresignUploadPart(ctx context.Context, key, uploadID string, number int32, size int64, sha256b64 string, ttl time.Duration) (proto.PresignedRequest, error) {
	req, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		UploadId:       aws.String(uploadID),
		PartNumber:     aws.Int32(number),
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(sha256b64),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return proto.PresignedRequest{}, err
	}
	return presigned(req.URL, req.SignedHeader), nil
}

// Part is an uploaded part of a multipart upload.
type Part struct {
	Number int32
	ETag   string
	SHA256 string // base64
}

// CompleteMultipart assembles the parts, each checked against its checksum.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	done := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		done[i] = types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag), ChecksumSHA256: aws.String(p.SHA256)}
	}
	_, err := s.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done},
	})
	return err
}

func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID)})
	return err
}

// Size is the stored object's length.
func (s *Store) Size(ctx context.Context, key string) (int64, error) {
	out, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(out.ContentLength), nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

// Check verifies the bucket is reachable, so luxd fails at startup rather
// than on the first upload.
func (s *Store) Check(ctx context.Context) error {
	_, err := s.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return fmt.Errorf("bucket %s does not exist", s.bucket)
	}
	return err
}
