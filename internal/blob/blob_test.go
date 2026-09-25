package blob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolateAWS points the default chain at the given environment keys only:
// no shared config files, no profile, no IMDS.
func isolateAWS(t *testing.T, accessKey, secretKey string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	t.Setenv("AWS_ACCESS_KEY_ID", accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secretKey)
}

// fakeS3 answers every request with 200 and records its Authorization
// header and path.
type fakeS3 struct {
	mu    sync.Mutex
	auth  []string
	paths []string
}

func (f *fakeS3) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeS3) lastAuth(t *testing.T) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auth) == 0 {
		t.Fatal("no request reached the fake S3")
	}
	return f.auth[len(f.auth)-1]
}

func TestDefaultChainCredentialsSignRequests(t *testing.T) {
	isolateAWS(t, "AKIDFROMENV", "env-secret")
	var f fakeS3
	s, err := New(context.Background(), Config{Endpoint: f.start(t), Region: "eu-north-1", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	auth := f.lastAuth(t)
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "Credential=AKIDFROMENV/") {
		t.Fatalf("request not signed with the environment's key: %q", auth)
	}
	if !strings.Contains(auth, "/eu-north-1/s3/aws4_request") {
		t.Fatalf("request not signed for the configured region: %q", auth)
	}
	// Path-style with a custom endpoint.
	if f.paths[len(f.paths)-1] != "/b" {
		t.Fatalf("path = %q, want /b", f.paths[len(f.paths)-1])
	}
}

func TestStaticKeysOverrideDefaultChain(t *testing.T) {
	isolateAWS(t, "AKIDFROMENV", "env-secret")
	var f fakeS3
	s, err := New(context.Background(), Config{
		Endpoint: f.start(t), Bucket: "b", AccessKey: "AKIDSTATIC", SecretKey: "static-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	auth := f.lastAuth(t)
	if !strings.Contains(auth, "Credential=AKIDSTATIC/") {
		t.Fatalf("request not signed with the static key: %q", auth)
	}
	// No region configured: us-east-1, as before.
	if !strings.Contains(auth, "/us-east-1/s3/aws4_request") {
		t.Fatalf("request not signed for us-east-1: %q", auth)
	}
}

func TestPresignUsesPublicEndpointAndDefaultChain(t *testing.T) {
	isolateAWS(t, "AKIDFROMENV", "env-secret")
	s, err := New(context.Background(), Config{
		Endpoint: "http://internal:9000", PublicEndpoint: "http://public.example:9000", Bucket: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.PresignGet(context.Background(), "k/v", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "public.example:9000" || u.Path != "/b/k/v" {
		t.Fatalf("presigned URL = %s, want host public.example:9000 path /b/k/v", raw)
	}
	if c := u.Query().Get("X-Amz-Credential"); !strings.HasPrefix(c, "AKIDFROMENV/") {
		t.Fatalf("presigned with credential %q, want the environment's key", c)
	}
}
