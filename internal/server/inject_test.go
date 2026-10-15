package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const snip = `<script src="/w.js"></script>`

func injectAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(newInjectBody(io.NopCloser(r), []byte(snip)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInjectPlacement(t *testing.T) {
	big := strings.Repeat("x", injectWindow)
	for _, c := range []struct{ name, in, want string }{
		{"head", "<html><head><title>t</title>", "<html><head>" + snip + "<title>t</title>"},
		{"upper case with attribute", "<!DOCTYPE html><HTML><HEAD lang=x><meta>", "<!DOCTYPE html><HTML><HEAD lang=x>" + snip + "<meta>"},
		{"quoted >", `<head data-x="a>b" data-y='c>d'><title>`, `<head data-x="a>b" data-y='c>d'>` + snip + "<title>"},
		{"header before head", "<header>h</header><head>x", "<header>h</header><head>" + snip + "x"},
		{"head with newline", "<head\n>x", "<head\n>" + snip + "x"},
		{"no head, body", "<!doctype html><p>a<body class=x>b", "<!doctype html><p>a" + snip + "<body class=x>b"},
		{"no head, bodyguard is not body", "<!doctype html><bodyguard><body>b", "<!doctype html><bodyguard>" + snip + "<body>b"},
		{"doctype only", "\xef\xbb\xbf \n<!DOCTYPE html>\n<p>hi", "\xef\xbb\xbf \n<!DOCTYPE html>" + snip + "\n<p>hi"},
		{"no doctype, no tags", "just text", snip + "just text"},
		{"empty", "", snip},
		{"head after the window", big + "<head>z", snip + big + "<head>z"},
		{"head after the window, doctype", "<!doctype html>" + big + "<head>z", "<!doctype html>" + snip + big + "<head>z"},
		{"head unclosed in the window, body", "<body>" + "<head " + big, snip + "<body>" + "<head " + big},
		{"head straddles the window's end", big[:injectWindow-3] + "<head>z", snip + big[:injectWindow-3] + "<head>z"},
		{"body in a comment before head", "<!doctype html><!-- <body> --><html><head><title>", "<!doctype html><!-- <body> --><html><head>" + snip + "<title>"},
		{"head in a comment", "<!-- <head> --><head>x", "<!-- <head> --><head>" + snip + "x"},
		{"comment unclosed through the window", "<!doctype html><!-- <head> " + big + "--><head>z", "<!doctype html>" + snip + "<!-- <head> " + big + "--><head>z"},
		{"body before head", "<body>a<head>b", snip + "<body>a<head>b"},
		{"body inside head's attribute", `<head data-x="<body>">x`, `<head data-x="<body>">` + snip + "x"},
		// Documented limitation: a scan, not a parser, takes this <body for the tag.
		{"body inside an earlier tag's attribute", `<html data-x="<body>"><head>x`, `<html data-x="` + snip + `<body>"><head>x`},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, wrap := range []struct {
				name string
				fn   func(io.Reader) io.Reader
			}{
				{"whole", func(r io.Reader) io.Reader { return r }},
				{"one byte", iotest.OneByteReader},
				{"half", iotest.HalfReader},
				{"data err", iotest.DataErrReader},
			} {
				if got := injectAll(t, wrap.fn(strings.NewReader(c.in))); got != c.want {
					t.Errorf("%s: got %q, want %q", wrap.name, abbrev(got), abbrev(c.want))
				}
			}
		})
	}
}

func abbrev(s string) string {
	if len(s) > 200 {
		return s[:100] + "…" + s[len(s)-100:]
	}
	return s
}

// countingReader counts what has been read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// Without a <head in the window, no more than the window is read before
// the first bytes come out.
func TestInjectHoldsAtMostTheWindow(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("y", 10*injectWindow) + "<head>")}
	b := newInjectBody(io.NopCloser(iotest.HalfReader(src)), []byte(snip))
	p := make([]byte, 10)
	if _, err := io.ReadFull(b, p); err != nil {
		t.Fatal(err)
	}
	if string(p) != snip[:10] || src.n > injectWindow {
		t.Fatalf("first read %q after %d bytes of the source", p, src.n)
	}
}

// blockingReader gives its data, then blocks until released.
type blockingReader struct {
	data    []byte
	release chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		<-b.release
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// A page that has not ended streams what it has once the snippet is
// placed: nothing waits for its end.
func TestInjectStreamsBeforeEOF(t *testing.T) {
	for _, c := range []struct{ name, data, want string }{
		{"head", "<!doctype html><html><head><title>t</title>", "<!doctype html><html><head>" + snip + "<title>t</title>"},
		{"body, no head", "<!doctype html><body><p>step 1</p>", "<!doctype html>" + snip + "<body><p>step 1</p>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := &blockingReader{data: []byte(c.data), release: make(chan struct{})}
			defer close(src.release)
			b := newInjectBody(io.NopCloser(src), []byte(snip))
			got := make(chan []byte, 1)
			go func() {
				buf := make([]byte, len(c.want))
				n, _ := io.ReadFull(b, buf)
				got <- buf[:n]
			}()
			select {
			case g := <-got:
				if string(g) != c.want {
					t.Fatalf("got %q, want %q", g, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the first bytes waited for the end of the page")
			}
		})
	}
}

func TestIsNavigationHeaders(t *testing.T) {
	for _, c := range []struct {
		method string
		hdr    map[string]string
		want   bool
	}{
		{"GET", map[string]string{"Sec-Fetch-Dest": "document"}, true},
		{"GET", map[string]string{"Sec-Fetch-Dest": "iframe", "Accept": "*/*"}, true},
		{"GET", map[string]string{"Sec-Fetch-Dest": "script", "Accept": "text/html"}, false},
		{"GET", map[string]string{"Sec-Fetch-Dest": "image"}, false},
		{"GET", map[string]string{"Sec-Fetch-Dest": "empty", "Accept": "text/html"}, false},
		{"GET", map[string]string{"Accept": "text/html,application/xhtml+xml"}, true},
		{"GET", map[string]string{"Accept": "application/json"}, false},
		{"GET", map[string]string{"Sec-Fetch-Dest": "", "Accept": "text/html"}, false},
		{"GET", map[string]string{"Accept": "application/xhtml+xml"}, false},
		{"GET", map[string]string{"Accept": "TEXT/HTML"}, true},
		{"GET", nil, false},
		{"HEAD", map[string]string{"Sec-Fetch-Dest": "document"}, false},
		{"POST", map[string]string{"Sec-Fetch-Dest": "document"}, false},
		{"GET", map[string]string{"Sec-Fetch-Dest": "document", "Upgrade": "websocket"}, false},
	} {
		r := httptest.NewRequest(c.method, "https://web.lux.example.com/", nil)
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := isNavigation(r); got != c.want {
			t.Errorf("%s %v: %v, want %v", c.method, c.hdr, got, c.want)
		}
	}
}
