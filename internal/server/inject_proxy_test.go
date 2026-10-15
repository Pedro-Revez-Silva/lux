package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const injectPage = "<!doctype html><html><head><title>t</title></head><body>hi</body></html>"

// injectProxyFixture: two ready servers of r1 behind the preview listener,
// web (inject testSnippet) and plain (no inject), both proxied to one app
// (at appURL); seen returns the headers of the app's last request.
func injectProxyFixture(t *testing.T) (appURL string, s *Server, do func(host, method, path string, hdr http.Header) *httptest.ResponseRecorder, seen func() http.Header) {
	t.Helper()
	s = testServer(t)
	s.cfg.PublicURL = "https://luxd.example.com"
	s.cfg.Preview = PreviewConfig{Domain: "lux.example.com", Auth: "ticket", HoldFor: 300 * time.Millisecond}
	ctx := context.Background()
	serversFixture(t, s, ctx)
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var last http.Header
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(injectPage))
	_ = zw.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		h := w.Header()
		h.Set("ETag", `"v1"`)
		h.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		h.Set("Cache-Control", "max-age=60")
		h.Set("Content-Security-Policy", "script-src 'self'")
		switch r.URL.Path {
		case "/gz":
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
		case "/missing":
			h.Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(injectPage))
		case "/api":
			h.Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"head":"<head>"}`))
		case "/app.js":
			h.Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(`document.write("<head>")`))
		case "/stream":
			h.Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><head><title>s</title>"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		case "/xhtml":
			h.Set("Content-Type", "application/xhtml+xml")
			_, _ = w.Write([]byte(injectPage))
		case "/identity":
			h.Set("Content-Type", "text/html")
			h.Set("Content-Encoding", "identity")
			_, _ = w.Write([]byte(injectPage))
		default:
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Content-MD5", "Q2hlY2sgSW50ZWdyaXR5IQ==")
			h.Set("Digest", "sha-256=X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=")
			h.Set("Vary", "Origin")
			_, _ = w.Write([]byte(injectPage))
		}
	}))
	t.Cleanup(app.Close)
	fakeRunner(t, s, strings.TrimPrefix(app.URL, "http://"))
	execSQL(t, s, ctx, `INSERT INTO run_servers (id, host, tenant_id, run_id, name, port, state, ready_since, inject) VALUES
		('srv_aaaaaaaaaaaaaaaa', 'web', 't1', $1, 'web', 3000, 'ready', now(), $2),
		('srv_bbbbbbbbbbbbbbbb', 'plain', 't1', $1, 'plain', 3001, 'ready', now(), '')`, r1, testSnippet)
	cookies := map[string]string{"web": cookieFor(s, "srv_aaaaaaaaaaaaaaaa"), "plain": cookieFor(s, "srv_bbbbbbbbbbbbbbbb")}
	do = func(host, method, path string, hdr http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://"+host+".lux.example.com"+path, nil)
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("Cookie", previewCookie+"="+cookies[host])
		w := httptest.NewRecorder()
		s.preview.ServeHTTP(w, req)
		return w
	}
	seen = func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	return app.URL, s, do, seen
}

// A page that keeps streaming reaches the browser as it comes, snippet
// included, before the server ends it.
func TestPreviewInjectStreams(t *testing.T) {
	_, s, _, _ := injectProxyFixture(t)
	front := httptest.NewServer(s.preview)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/stream", nil)
	req.Host = "web.lux.example.com"
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Cookie", previewCookie+"="+cookieFor(s, "srv_aaaaaaaaaaaaaaaa"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := "<html><head>" + testSnippet + "<title>s</title>"
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != want {
		t.Fatalf("streamed: %q, %v", buf, err)
	}
}

// What a browser's navigation sends.
func navHeaders() http.Header {
	return http.Header{"Sec-Fetch-Dest": {"document"}, "Accept": {"text/html,*/*"}, "Accept-Encoding": {"gzip, br"},
		"If-None-Match": {`"v0"`}, "If-Modified-Since": {"Mon, 02 Jan 2006 15:04:05 GMT"}}
}

func TestPreviewInjectNavigation(t *testing.T) {
	_, _, do, seen := injectProxyFixture(t)
	w := do("web", http.MethodGet, "/", navHeaders())
	want := strings.Replace(injectPage, "<head>", "<head>"+testSnippet, 1)
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("navigation: %d %q", w.Code, w.Body)
	}
	h := w.Header()
	for _, k := range []string{"Content-Length", "ETag", "Last-Modified", "Content-MD5", "Digest"} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s kept: %q", k, v)
		}
	}
	if h.Get("Cache-Control") != "max-age=60" || h.Get("Content-Security-Policy") != "script-src 'self'" {
		t.Errorf("cache-control or CSP changed: %v", h)
	}
	if v := h.Values("Vary"); !slices.Equal(v, []string{"Origin", "Sec-Fetch-Dest", "Accept"}) {
		t.Errorf("Vary: %q", v)
	}
	up := seen()
	if up.Get("Accept-Encoding") != "identity" || up.Get("If-None-Match") != "" || up.Get("If-Modified-Since") != "" {
		t.Fatalf("upstream saw: %v", up)
	}
	// An old browser: no Sec-Fetch-Dest, Accept asks for HTML.
	if w := do("web", http.MethodGet, "/", http.Header{"Accept": {"text/html"}}); w.Body.String() != want {
		t.Fatalf("navigation without Sec-Fetch-Dest: %q", w.Body)
	}
	// An iframe's document.
	if w := do("web", http.MethodGet, "/", http.Header{"Sec-Fetch-Dest": {"iframe"}}); w.Body.String() != want {
		t.Fatalf("iframe: %q", w.Body)
	}
	// Content-Encoding: identity is no encoding.
	if w := do("web", http.MethodGet, "/identity", navHeaders()); w.Body.String() != want {
		t.Fatalf("identity: %q", w.Body)
	}
}

// Everything that is not an HTML 200 for a document navigation of a server
// with an inject passes as it came: body, headers, and what the server saw.
func TestPreviewInjectPassThrough(t *testing.T) {
	appURL, _, do, seen := injectProxyFixture(t)
	nav := navHeaders()
	for _, c := range []struct {
		name, host, method, path string
		hdr                      http.Header
		// asSent: the server saw the client's encoding and validators.
		asSent bool
	}{
		{"script", "web", "GET", "/app.js", http.Header{"Sec-Fetch-Dest": {"script"}, "Accept": {"text/html"}, "Accept-Encoding": {"gzip"}, "If-None-Match": {`"v0"`}}, true},
		{"image", "web", "GET", "/", http.Header{"Sec-Fetch-Dest": {"image"}, "Accept-Encoding": {"gzip"}, "If-None-Match": {`"v0"`}}, true},
		{"fetch", "web", "GET", "/", http.Header{"Sec-Fetch-Dest": {"empty"}, "Accept": {"text/html"}, "Accept-Encoding": {"gzip"}, "If-None-Match": {`"v0"`}}, true},
		{"head", "web", "HEAD", "/", nav, true},
		{"404", "web", "GET", "/missing", nav, false},
		{"json", "web", "GET", "/api", nav, false},
		{"gzip anyway", "web", "GET", "/gz", nav, false},
		{"xhtml", "web", "GET", "/xhtml", nav, false},
		{"lux path", "web", "GET", "/.lux/page", nav, true},
		{"no inject", "plain", "GET", "/", nav, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(c.host, c.method, c.path, c.hdr)
			if c.asSent {
				up := seen()
				if up.Get("Accept-Encoding") != c.hdr.Get("Accept-Encoding") || up.Get("If-None-Match") != c.hdr.Get("If-None-Match") ||
					up.Get("If-Modified-Since") != c.hdr.Get("If-Modified-Since") {
					t.Fatalf("upstream saw: %v", up)
				}
			}
			direct := directAnswer(t, appURL, c.method, c.path, c.hdr)
			if w.Code != direct.StatusCode || !bytes.Equal(w.Body.Bytes(), direct.body) {
				t.Fatalf("body changed: %d %q, want %d %q", w.Code, w.Body, direct.StatusCode, direct.body)
			}
			for _, k := range []string{"Content-Length", "ETag", "Last-Modified", "Content-Type", "Content-Encoding", "Cache-Control", "Content-Security-Policy", "Vary", "Content-MD5", "Digest"} {
				if got, want := w.Header().Values(k), direct.Header.Values(k); !slices.Equal(got, want) {
					t.Errorf("%s: %q, want %q", k, got, want)
				}
			}
		})
	}
}

type answer struct {
	*http.Response
	body []byte
}

// directAnswer is the app's own answer to a request, as lux relayed it
// before servers had an inject: the baseline of a pass-through.
func directAnswer(t *testing.T, appURL, method, path string, hdr http.Header) answer {
	t.Helper()
	req, _ := http.NewRequest(method, appURL+path, nil)
	req.Header = hdr.Clone()
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{resp, b}
}
