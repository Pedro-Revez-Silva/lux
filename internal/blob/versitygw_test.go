package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/marcioapm/lux/internal/proto"
)

// versitygwStore is a Store on the e2e harness's S3 (versitygw on
// LUX_TEST_S3_PORT, its fixed keys) with a bucket of its own, deleted at
// the end. Skips without LUX_TEST_S3_PORT.
func versitygwStore(t *testing.T) *Store {
	t.Helper()
	port := os.Getenv("LUX_TEST_S3_PORT")
	if port == "" {
		t.Skip("LUX_TEST_S3_PORT not set: no harness S3")
	}
	isolateAWS(t, "luxs3", "luxs3-secret")
	ctx := context.Background()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	bucket := "lux-ds3-" + hex.EncodeToString(suffix)
	s, err := New(ctx, Config{Endpoint: "http://127.0.0.1:" + port, Region: "us-east-1", Bucket: bucket, AccessKey: "luxs3", SecretKey: "luxs3-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ups, _ := s.s3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if ups != nil {
			for _, u := range ups.Uploads {
				_ = s.AbortMultipart(ctx, aws.ToString(u.Key), aws.ToString(u.UploadId))
			}
		}
		objs, _ := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if objs != nil {
			for _, o := range objs.Contents {
				_ = s.Delete(ctx, aws.ToString(o.Key))
			}
		}
		if _, err := s.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Errorf("delete bucket %s: %v", bucket, err)
		}
	})
	return s
}

// putPresigned PUTs body with exactly the presigned headers, as the runner
// does. Returns the status, ETag and S3's error body.
func putPresigned(t *testing.T, p proto.PresignedRequest, body []byte) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		if k == "Content-Length" {
			if v != strconv.Itoa(len(body)) {
				t.Fatalf("presigned Content-Length %s, body %d bytes", v, len(body))
			}
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), string(msg)
}

func b64sum(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

// A 2-part upload through presigned UploadPart URLs against a real S3
// implementation: a part 2 body other than the one presigned is refused;
// the right one completes with the per-part checksums, at the right size.
func TestMultipartAgainstVersitygw(t *testing.T) {
	s := versitygwStore(t)
	ctx := context.Background()
	const minPart = 5 << 20 // S3's least non-final part
	content := make([]byte, minPart+1000)
	_, _ = rand.Read(content)
	p1, p2 := content[:minPart], content[minPart:]
	whole := sha256.Sum256(content)
	key := "tenants/t1/runs/r1/b1"

	id, err := s.CreateMultipart(ctx, key, hex.EncodeToString(whole[:]))
	if err != nil {
		t.Fatal(err)
	}
	u1, err := s.PresignUploadPart(ctx, key, id, 1, int64(len(p1)), b64sum(p1), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := s.PresignUploadPart(ctx, key, id, 2, int64(len(p2)), b64sum(p2), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, etag1, msg := putPresigned(t, u1, p1)
	if code != http.StatusOK {
		t.Fatalf("part 1: %d %s", code, msg)
	}
	wrong := bytes.Clone(p2)
	wrong[0] ^= 0xff
	if code, _, msg := putPresigned(t, u2, wrong); code/100 == 2 {
		t.Fatalf("part 2 with another body: %d %s, want refused", code, msg)
	}
	code, etag2, msg := putPresigned(t, u2, p2)
	if code != http.StatusOK {
		t.Fatalf("part 2: %d %s", code, msg)
	}
	err = s.CompleteMultipart(ctx, key, id, []Part{{1, etag1, b64sum(p1)}, {2, etag2, b64sum(p2)}})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	size, err := s.Size(ctx, key)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("size %d %v, want %d", size, err, len(content))
	}
	body, _, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, content) {
		t.Fatalf("object differs: %d bytes", len(got))
	}
}
