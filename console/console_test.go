package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesAssetsAndFallsBackToIndex(t *testing.T) {
	files := fstest.MapFS{
		"index.html":         {Data: []byte(`<div id="root"></div>`)},
		"main-abc123.js":     {Data: []byte("console.log(1)")},
		"main-abc123.css":    {Data: []byte("body{}")},
		"logo.svg":           {Data: []byte("<svg/>")},
		"chunk-9f8e.js.map":  {Data: []byte("{}")},
		"nested/icon-11.png": {Data: []byte("\x89PNG\r\n\x1a\n")},
	}
	h := handler(files)
	for _, c := range []struct {
		path, status, ctype, body, cache string
	}{
		{"/", "200", "text/html; charset=utf-8", `<div id="root">`, "no-cache"},
		{"/runs/run_x", "200", "text/html; charset=utf-8", `<div id="root">`, "no-cache"},
		{"/hosts?tenant=t", "200", "text/html; charset=utf-8", `<div id="root">`, "no-cache"},
		{"/index.html", "200", "text/html; charset=utf-8", `<div id="root">`, "no-cache"},
		{"/main-abc123.js", "200", "text/javascript; charset=utf-8", "console.log(1)", "public, max-age=31536000, immutable"},
		{"/main-abc123.css", "200", "text/css; charset=utf-8", "body{}", "public, max-age=31536000, immutable"},
		{"/logo.svg", "200", "image/svg+xml", "<svg/>", ""},
		{"/nested/icon-11.png", "200", "image/png", "PNG", "public, max-age=31536000, immutable"},
		{"/chunk-9f8e.js.map", "200", "text/plain; charset=utf-8", "{}", "public, max-age=31536000, immutable"},
		{"/main-stale.js", "404", "text/plain; charset=utf-8", "not found", ""},
		{"/gone.css", "404", "text/plain; charset=utf-8", "not found", ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := w.Result().Status[:3]; got != c.status {
			t.Errorf("%s: status %s, want %s", c.path, got, c.status)
		}
		if got := w.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ctype)
		}
		if !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: body %q, want it to contain %q", c.path, w.Body, c.body)
		}
		if got := w.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.cache)
		}
	}
}

func TestHandlerWithoutABuildSaysHowToMakeOne(t *testing.T) {
	w := httptest.NewRecorder()
	handler(fstest.MapFS{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "built without the console") {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
}
