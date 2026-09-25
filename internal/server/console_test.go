package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console is the catch-all at /, but /v1/ and /runner/ keep their own
// 404s and 405s, and old /console links redirect to the same page at /.
func TestConsoleAtRootAPIKeepsPrecedence(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	h := s.Handler()
	isConsole := func(w *httptest.ResponseRecorder) bool {
		return strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") &&
			(strings.Contains(w.Body.String(), `id="root"`) || strings.Contains(w.Body.String(), "lux console"))
	}
	for _, c := range []struct {
		method, path string
		status       int
		console      bool
		ctype        string
	}{
		{"GET", "/", 200, true, ""},
		{"GET", "/runs/run_x", 200, true, ""},
		{"GET", "/hosts", 200, true, ""},
		{"HEAD", "/pools", 200, true, ""},
		{"GET", "/health", 200, false, "application/json"},
		{"GET", "/openapi.json", 200, false, "application/openapi+json"},
		{"GET", "/v1/runs", 401, false, "application/json"},
		{"GET", "/v1/nope", 404, false, ""},
		{"GET", "/v1", 404, false, ""},
		{"POST", "/v1/nope", 404, false, ""},
		{"GET", "/runner/nope", 404, false, ""},
		{"GET", "/runner", 404, false, ""},
		{"POST", "/runs", 404, false, ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.status {
			t.Errorf("%s %s: %d, want %d (%q)", c.method, c.path, w.Code, c.status, w.Body)
		}
		if c.method == "GET" && isConsole(w) != c.console {
			t.Errorf("%s %s: console served = %v, want %v (%q)", c.method, c.path, !c.console, c.console, w.Body)
		}
		if c.ctype != "" && w.Header().Get("Content-Type") != c.ctype {
			t.Errorf("%s %s: Content-Type %q, want %q", c.method, c.path, w.Header().Get("Content-Type"), c.ctype)
		}
	}

	for from, to := range map[string]string{
		"/console":                  "/",
		"/console/":                 "/",
		"/console/runs/run_x":       "/runs/run_x",
		"/console/runs?tenant=acme": "/runs?tenant=acme",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, from, nil))
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != to {
			t.Errorf("GET %s: %d Location %q, want 301 %q", from, w.Code, w.Header().Get("Location"), to)
		}
	}
}

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
