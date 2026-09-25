package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The runner API lives under /runner/v1/; the unversioned paths are gone,
// not aliased.
func TestRunnerRoutesAreVersioned(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runner/v1/bootstrap.sh", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "#!") ||
		!strings.Contains(w.Body.String(), "$LUX_URL/runner/v1/bin/manifest") {
		t.Fatalf("GET /runner/v1/bootstrap.sh: %d %.200q", w.Code, w.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/runner/bootstrap.sh"},
		{"GET", "/runner/ws"},
		{"POST", "/runner/poll"},
		{"PUT", "/runner/blobs/b1"},
		{"GET", "/runner/blobs/b1"},
		{"GET", "/runner/bin/manifest"},
		{"GET", "/runner/bin/linux-arm64/lux-runner"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, w.Code)
		}
	}
	// Versioned routes exist: without a host token they refuse, not 404.
	for _, c := range []struct{ method, path string }{
		{"POST", "/runner/v1/poll"},
		{"GET", "/runner/v1/blobs/b1"},
		{"GET", "/runner/v1/bin/manifest"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401 (%q)", c.method, c.path, w.Code, w.Body)
		}
	}
}
