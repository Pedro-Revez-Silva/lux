package blob

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// signedHeaders is the X-Amz-SignedHeaders list of a presigned URL.
func signedHeaders(t *testing.T, raw string) (*url.URL, []string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u, strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";")
}

func presignStore(t *testing.T) *Store {
	t.Helper()
	isolateAWS(t, "AKID", "secret")
	s, err := New(context.Background(), Config{Endpoint: "http://internal:9000", PublicEndpoint: "http://public.example:9000", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A presigned PUT signs the body's checksum, its sha256 metadata and its
// length, and hands back exactly those headers: S3 refuses another body.
func TestPresignPutSignsChecksum(t *testing.T) {
	s := presignStore(t)
	p, err := s.PresignPut(context.Background(), "k/v", 5, "abcd", "q83vEjRWeJA=", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, signed := signedHeaders(t, p.URL)
	if u.Host != "public.example:9000" || u.Path != "/b/k/v" {
		t.Fatalf("URL %s, want the public endpoint and /b/k/v", p.URL)
	}
	for _, h := range []string{"content-length", "x-amz-checksum-sha256", "x-amz-meta-sha256"} {
		if !slices.Contains(signed, h) {
			t.Errorf("%s not signed: %v", h, signed)
		}
	}
	want := map[string]string{"Content-Length": "5", "X-Amz-Checksum-Sha256": "q83vEjRWeJA=", "X-Amz-Meta-Sha256": "abcd"}
	if len(p.Headers) != len(want) {
		t.Fatalf("headers %v, want %v", p.Headers, want)
	}
	for k, v := range want {
		if p.Headers[k] != v {
			t.Errorf("header %s = %q, want %q", k, p.Headers[k], v)
		}
	}
	if e := u.Query().Get("X-Amz-Expires"); e != "3600" {
		t.Errorf("expires %s, want 3600", e)
	}
}

func TestPresignUploadPartSignsChecksum(t *testing.T) {
	s := presignStore(t)
	p, err := s.PresignUploadPart(context.Background(), "k/v", "up1", 2, 7, "cGFydA==", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, signed := signedHeaders(t, p.URL)
	if q := u.Query(); q.Get("uploadId") != "up1" || q.Get("partNumber") != "2" {
		t.Fatalf("URL %s, want uploadId up1 partNumber 2", p.URL)
	}
	for _, h := range []string{"content-length", "x-amz-checksum-sha256"} {
		if !slices.Contains(signed, h) {
			t.Errorf("%s not signed: %v", h, signed)
		}
	}
	if p.Headers["X-Amz-Checksum-Sha256"] != "cGFydA==" || p.Headers["Content-Length"] != "7" {
		t.Fatalf("headers %v", p.Headers)
	}
}

// multipartS3 answers the multipart calls and HeadObject, recording each
// request with its body.
type multipartS3 struct {
	mu   sync.Mutex
	reqs []string
}

func (f *multipartS3) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" algo="+r.Header.Get("X-Amz-Checksum-Algorithm")+
			" meta="+r.Header.Get("X-Amz-Meta-Sha256")+" "+string(body))
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "1234")
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMultipartCalls(t *testing.T) {
	isolateAWS(t, "AKID", "secret")
	var f multipartS3
	s, err := New(context.Background(), Config{Endpoint: f.start(t), Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := s.CreateMultipart(ctx, "k", "abcd")
	if err != nil || id != "up-1" {
		t.Fatalf("create: %q %v", id, err)
	}
	if err := s.CompleteMultipart(ctx, "k", id, []Part{{1, `"e1"`, "c3VtMQ=="}, {2, `"e2"`, "c3VtMg=="}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.AbortMultipart(ctx, "k", id); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if n, err := s.Size(ctx, "k"); err != nil || n != 1234 {
		t.Fatalf("size: %d %v", n, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 4 {
		t.Fatalf("requests %q", f.reqs)
	}
	if r := f.reqs[0]; !strings.HasPrefix(r, "POST /b/k?uploads") || !strings.Contains(r, "algo=SHA256") || !strings.Contains(r, "meta=abcd") {
		t.Errorf("create: %s", r)
	}
	c := f.reqs[1]
	if !strings.HasPrefix(c, "POST /b/k?uploadId=up-1") {
		t.Errorf("complete: %s", c)
	}
	for _, want := range []string{"<ChecksumSHA256>c3VtMQ==</ChecksumSHA256>", "<ChecksumSHA256>c3VtMg==</ChecksumSHA256>",
		"<PartNumber>2</PartNumber>", "<ETag>&#34;e1&#34;</ETag>"} {
		if !strings.Contains(c, want) {
			t.Errorf("complete body lacks %s: %s", want, c)
		}
	}
	if r := f.reqs[2]; !strings.HasPrefix(r, "DELETE /b/k?uploadId=up-1") {
		t.Errorf("abort: %s", r)
	}
	if r := f.reqs[3]; !strings.HasPrefix(r, "HEAD /b/k") {
		t.Errorf("head: %s", r)
	}
}
