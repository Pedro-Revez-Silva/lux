package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/blob"
)

// fakeS3 is an S3 endpoint that records object deletes, answering each
// with 204, or with 403 for keys under failPrefix. A PUT is accepted
// (its body drained) after onPut, if set, returns.
type fakeS3 struct {
	mu         sync.Mutex
	deleted    []string
	failPrefix string
	onPut      func(key string)
}

func (f *fakeS3) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.deleted)
	slices.Sort(out)
	return out
}

// useFakeS3 points s's blob store at a fakeS3 for bucket b.
func useFakeS3(t *testing.T, s *Server) *fakeS3 {
	t.Helper()
	f := &fakeS3{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/b/")
		if r.Method == http.MethodPut {
			_, _ = io.Copy(io.Discard, r.Body)
			if f.onPut != nil {
				f.onPut(key)
			}
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		f.mu.Lock()
		fail := f.failPrefix != "" && strings.HasPrefix(key, f.failPrefix)
		if !fail {
			f.deleted = append(f.deleted, key)
		}
		f.mu.Unlock()
		// 403: not retried by the SDK, so a failing delete fails at once.
		if fail {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	bs, err := blob.New(context.Background(), blob.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = bs
	return f
}

// captureLog sends s's log to a buffer.
func captureLog(s *Server) *syncBuffer {
	b := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(b, nil))
	return b
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
