package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// api is the runner's plain-HTTP client for luxd: polling, and blob
// uploads and downloads. Authenticated with the host token.
type api struct {
	base  string
	token string
	host  string
	http  *http.Client
}

func newAPI(base, token, host string) *api {
	return &api{
		base:  strings.TrimRight(base, "/"),
		token: token,
		host:  host,
		http: &http.Client{
			Timeout: 0, // uploads can be long; callers use contexts
			// Redirects are followed by hand so the token never goes to
			// S3 with a presigned URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (a *api) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+a.token)
	return r, nil
}

func (a *api) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return a.doJSON(ctx, http.MethodPost, path, bytes.NewReader(b), out)
}

func (a *api) getJSON(ctx context.Context, path string, out any) error {
	return a.doJSON(ctx, http.MethodGet, path, nil, out)
}

func (a *api) doJSON(ctx context.Context, method, path string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := a.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// upload PUTs a blob through luxd, which streams it into S3: the path for
// a luxd without the direct upload routes.
func (a *api) upload(ctx context.Context, blobID string, body io.Reader, size int64) error {
	r, err := a.req(ctx, http.MethodPut, "/runner/v1/blobs/"+blobID+"?host="+a.host, body)
	if err != nil {
		return err
	}
	r.ContentLength = size
	r.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp, "upload "+blobID)
	}
	return nil
}

// errNoDirectUpload: luxd predates POST /runner/v1/blobs/{id}/upload.
var errNoDirectUpload = errors.New("luxd has no direct upload route")

// uploadDirect uploads the size bytes of f straight to S3 with the
// presigned URLs luxd hands out, then has luxd record it. f is read once to
// hash it, then once per part to send it.
func (a *api) uploadDirect(ctx context.Context, blobID string, f io.ReaderAt, size int64) error {
	part := func(i int) io.Reader { return io.NewSectionReader(f, int64(i)*proto.BlobPartSize, partLen(size, i)) }
	in := proto.BlobUploadRequest{Size: size}
	whole := sha256.New()
	for i := range proto.BlobParts(size) {
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(whole, h), part(i)); err != nil {
			return err
		}
		in.Parts = append(in.Parts, base64.StdEncoding.EncodeToString(h.Sum(nil)))
	}
	in.SHA256 = hex.EncodeToString(whole.Sum(nil))
	var out proto.BlobUploadResponse
	if err := a.blobPost(ctx, blobID, "upload", in, &out); err != nil {
		// A luxd without the route answers 405 or the mux's 404; a lux
		// not_found is the blob's, and keeps its meaning.
		var he *httpError
		if errors.As(err, &he) && (he.Status == http.StatusMethodNotAllowed || he.muxNotFound()) {
			return errNoDirectUpload
		}
		return err
	}
	var done proto.BlobCompleteRequest
	switch {
	case out.Uploaded:
		return nil
	case out.Single != nil:
		if _, err := putPart(ctx, *out.Single, func() io.Reader { return io.NewSectionReader(f, 0, size) }, size); err != nil {
			return fmt.Errorf("upload %s: %w", blobID, err)
		}
	default:
		if len(out.Parts) != len(in.Parts) {
			return fmt.Errorf("upload %s: luxd presigned %d parts, want %d", blobID, len(out.Parts), len(in.Parts))
		}
		done.UploadID = out.UploadID
		for i, p := range out.Parts {
			etag, err := putPart(ctx, p.PresignedRequest, func() io.Reader { return part(i) }, partLen(size, i))
			if err != nil {
				a.abortUpload(ctx, blobID, out.UploadID)
				return fmt.Errorf("upload %s part %d: %w", blobID, p.Number, err)
			}
			done.Parts = append(done.Parts, proto.UploadedPart{Number: p.Number, ETag: etag, SHA256: in.Parts[i]})
		}
	}
	complete := func() error { return a.blobPost(ctx, blobID, "complete", done, &struct{}{}) }
	var err error
	if done.UploadID == "" {
		err = complete()
	} else if err = withRetries(ctx, retryableComplete, complete); err != nil {
		a.abortUpload(ctx, blobID, done.UploadID)
	}
	// The mux's own 404 did not record the upload: a plain error, retried,
	// never the uploader's "deleted".
	var he *httpError
	if errors.As(err, &he) && he.muxNotFound() {
		return fmt.Errorf("complete %s: %s", blobID, he.Msg)
	}
	return err
}

// abortUpload has luxd drop the parts of an upload id given up on; best effort.
func (a *api) abortUpload(ctx context.Context, blobID, uploadID string) {
	_ = a.blobPost(ctx, blobID, "complete", proto.BlobCompleteRequest{UploadID: uploadID, Abort: true}, &struct{}{})
}

// retryableComplete: luxd did not answer, answered a 5xx (S3 unavailable),
// or the answer was the mux's own 404 rather than luxd's.
func retryableComplete(err error) bool {
	var he *httpError
	return !errors.As(err, &he) || he.Status/100 == 5 || he.muxNotFound()
}

// partLen is the length of part i (0-based) of a blob of size bytes.
func partLen(size int64, i int) int64 {
	return max(0, min(proto.BlobPartSize, size-int64(i)*proto.BlobPartSize))
}

// partRetryDelays precede a second and third attempt; a var so tests need not wait.
var partRetryDelays = []time.Duration{200 * time.Millisecond, time.Second}

// withRetries calls do until it succeeds, fails with an error retryable
// refuses, ctx ends, or partRetryDelays run out.
func withRetries(ctx context.Context, retryable func(error) bool, do func() error) error {
	err := do()
	for _, d := range partRetryDelays {
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(d):
		}
		err = do()
	}
	return err
}

// putPart is putPresigned with retries on a network error or an S3 5xx,
// each attempt with a fresh body from body(). A 4xx (an expired URL, a
// refused checksum) is final.
func putPart(ctx context.Context, p proto.PresignedRequest, body func() io.Reader, n int64) (string, error) {
	var etag string
	err := withRetries(ctx, func(err error) bool {
		var se *s3Error
		return !errors.As(err, &se) || se.status/100 != 4
	}, func() (err error) {
		etag, err = putPresigned(ctx, p, body(), n)
		return err
	})
	return etag, err
}

// s3Error is S3's error answer to a presigned request.
type s3Error struct {
	status int
	msg    string
}

func (e *s3Error) Error() string { return e.msg }

// putPresigned PUTs n bytes of body to a presigned S3 URL with exactly the
// headers signed into it, and never the host token. Returns the ETag. S3's
// errors are *s3Error, never *httpError: a 404 from S3 is not luxd
// deleting the blob.
func putPresigned(ctx context.Context, p proto.PresignedRequest, body io.Reader, n int64) (string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, body)
	if err != nil {
		return "", err
	}
	r.ContentLength = n
	if n == 0 {
		// Otherwise Go sends a non-nil body of length 0 chunked, without
		// the signed Content-Length.
		r.Body = http.NoBody
	}
	for k, v := range p.Headers {
		if http.CanonicalHeaderKey(k) == "Content-Length" {
			if v != strconv.FormatInt(n, 10) {
				return "", fmt.Errorf("presigned for %s bytes, part is %d", v, n)
			}
			continue // sent from r.ContentLength
		}
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", &s3Error{status: resp.StatusCode, msg: fmt.Sprintf("S3 PUT: %s: %s", resp.Status, bytes.TrimSpace(msg))}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("ETag"), nil
}

// blobPost POSTs in to /runner/v1/blobs/{id}/{op}; an error answer is an
// *httpError.
func (a *api) blobPost(ctx context.Context, blobID, op string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	// Completing a large multipart upload takes S3 a while.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	r, err := a.req(ctx, http.MethodPost, "/runner/v1/blobs/"+blobID+"/"+op+"?host="+a.host, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp, op+" "+blobID)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readHTTPError reads an error answer, with luxd's error code when its
// body is luxd's JSON error.
func readHTTPError(resp *http.Response, what string) *httpError {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var lux struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(msg, &lux)
	return &httpError{Status: resp.StatusCode, Code: lux.Error.Code, Msg: fmt.Sprintf("%s: %s: %s", what, resp.Status, bytes.TrimSpace(msg))}
}

// download returns a blob's body: luxd redirects to a presigned URL, which
// is fetched without the host token.
func (a *api) download(ctx context.Context, blobID string) (io.ReadCloser, error) {
	r, err := a.req(ctx, http.MethodGet, "/runner/v1/blobs/"+blobID+"?host="+a.host, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusTemporaryRedirect {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		r2, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		resp, err = http.DefaultClient.Do(r2)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpError{Status: resp.StatusCode, Msg: fmt.Sprintf("download %s: %s: %s", blobID, resp.Status, bytes.TrimSpace(msg))}
	}
	return resp.Body, nil
}

type httpError struct {
	Status int
	Code   string // luxd's error code; empty when the body was not luxd's JSON error
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }

// muxNotFound: a 404 without luxd's error body, from a mux (an older luxd, or a proxy).
func (e *httpError) muxNotFound() bool { return e.Status == http.StatusNotFound && e.Code == "" }
