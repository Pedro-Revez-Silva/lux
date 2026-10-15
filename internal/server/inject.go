package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// A server's inject: HTML the preview listener adds to the HTML documents
// it proxies for the server, right after the <head> tag. No parser and no
// full buffering: at most injectWindow bytes of the start of the body are
// held while looking for where the snippet goes, then the rest streams.

// injectWindow bounds how much of a page is held back to place the snippet.
const injectWindow = 32 << 10

// isNavigation: a request for a document a browser displays (a page or
// an iframe's). Without Sec-Fetch-Dest (older browsers), an Accept that
// asks for HTML. Upgrades (WebSockets) never are.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		return false
	}
	if _, ok := r.Header["Sec-Fetch-Dest"]; ok {
		dest := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
		return dest == "document" || dest == "iframe"
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// prepareInjectRequest asks the server for an uncompressed, unconditional
// answer: compressed, the snippet could not be placed without
// decompressing; a 304 would replay a page cached without it.
func prepareInjectRequest(out *http.Request) {
	out.Header.Set("Accept-Encoding", "identity")
	out.Header.Del("If-None-Match")
	out.Header.Del("If-Modified-Since")
}

// injectResponse wraps an HTML 200 that is not encoded; anything else
// (and an encoded page) passes untouched. The validators and length of
// the server's bytes no longer describe the page sent, so they go.
func injectResponse(resp *http.Response, snippet string) {
	if resp.StatusCode != http.StatusOK || !isHTML(resp.Header.Get("Content-Type")) {
		return
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return
	}
	resp.Body = newInjectBody(resp.Body, []byte(snippet))
	resp.ContentLength = -1
	for _, h := range []string{"Content-Length", "ETag", "Content-MD5", "Digest", "Last-Modified"} {
		resp.Header.Del(h)
	}
	// The same URL answers fetches without the snippet: caches must key on
	// what tells a navigation apart (isNavigation).
	resp.Header.Add("Vary", "Sec-Fetch-Dest")
	resp.Header.Add("Vary", "Accept")
}

func isHTML(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "text/html")
}

// injectBody is a response body with a snippet placed in its first
// injectWindow bytes (findInjectAt).
type injectBody struct {
	src     io.ReadCloser
	snippet []byte
	buf     []byte // held while undecided
	from    int    // where the scan (firstTag) resumes in buf
	decided bool
	pending io.Reader // decided bytes not yet returned
	err     error     // src's error, returned once pending is out
}

func newInjectBody(src io.ReadCloser, snippet []byte) *injectBody {
	return &injectBody{src: src, snippet: snippet, buf: make([]byte, 0, injectWindow)}
}

func (b *injectBody) Read(p []byte) (int, error) {
	for !b.decided {
		n, err := b.src.Read(b.buf[len(b.buf):cap(b.buf)])
		b.buf = b.buf[:len(b.buf)+n]
		final := err != nil || len(b.buf) == cap(b.buf)
		at, from, ok := findInjectAt(b.buf, b.from, final)
		if ok {
			b.pending = io.MultiReader(bytes.NewReader(b.buf[:at]), bytes.NewReader(b.snippet), bytes.NewReader(b.buf[at:]))
			b.buf, b.decided = nil, true
		}
		b.from, b.err = from, err
	}
	if b.pending != nil {
		n, err := b.pending.Read(p)
		if err != io.EOF {
			return n, err
		}
		b.pending = nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.src.Read(p)
}

func (b *injectBody) Close() error { return b.src.Close() }

// findInjectAt is where the snippet goes in the start of a page. The first
// complete <head…> or <body…> tag wins, as in the HTML parser (a <head>
// after <body> is ignored): right after a <head…> tag, right before a
// <body…> tag. Without either in the window, after a leading <!doctype …>
// (never before it: the page would render in quirks mode), else at the
// start. A <head or <body inside an earlier tag's attribute value or a
// script is taken for the tag: this is a scan, not an HTML parser.
// ok is false while it cannot tell yet; final says no more is coming. from
// is where the scan resumes (nothing before it can matter), returned for
// the next call.
func findInjectAt(buf []byte, from int, final bool) (at, next int, ok bool) {
	at, partial := firstTag(buf, from)
	switch {
	case at >= 0:
		return at, 0, true
	case final:
		return afterDoctype(buf), 0, true
	case partial >= 0:
		return 0, partial, false
	}
	return 0, len(buf), false
}

// firstTag scans buf from from for the first complete <head…> or <body…>
// tag (case-insensitive; the name ends with '>' or whitespace, so
// "<header>" is not one; '>' in quoted values skipped), stepping over
// <!-- … --> comments. at is the index after a <head…> tag's '>' or the '<'
// of a <body…> tag, else -1. partial is then the '<' of a tag or comment
// the buffer ends inside, else -1; everything after it is inside it, so no
// complete tag can follow.
func firstTag(buf []byte, from int) (at, partial int) {
	const open, n = "<!--", len("<head")
	for i := from; i < len(buf); i++ {
		if buf[i] != '<' {
			continue
		}
		rest := buf[i:]
		if bytes.HasPrefix(rest, []byte(open)) {
			j := bytes.Index(rest[len(open):], []byte("-->"))
			if j < 0 {
				return -1, i
			}
			i += len(open) + j + len("--")
			continue
		}
		if len(rest) <= n {
			if prefixFold(rest, open) || prefixFold(rest, "<head") || prefixFold(rest, "<body") {
				return -1, i
			}
			continue
		}
		name := strings.ToLower(string(rest[:n]))
		if (name != "<head" && name != "<body") || !tagBoundary(rest[n]) {
			continue
		}
		switch end := tagEnd(buf, i+n); {
		case end < 0:
			return -1, i
		case name == "<head":
			return end, -1
		default:
			return i, -1
		}
	}
	return -1, -1
}

// prefixFold: b is a case-insensitive prefix of s.
func prefixFold(b []byte, s string) bool {
	return len(b) <= len(s) && strings.EqualFold(string(b), s[:len(b)])
}

func tagBoundary(c byte) bool {
	return c == '>' || htmlSpace(c)
}

func htmlSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// tagEnd is the index after the '>' closing a tag whose attributes start at
// i, with '>' inside quoted values skipped; -1 if the buffer ends first.
func tagEnd(buf []byte, i int) int {
	var quote byte
	for ; i < len(buf); i++ {
		c := buf[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i + 1
		}
	}
	return -1
}

// afterDoctype is the index after a <!doctype …> that the page starts
// with (after a BOM and whitespace), else 0.
func afterDoctype(buf []byte) int {
	i := 0
	if bytes.HasPrefix(buf, []byte("\xef\xbb\xbf")) {
		i = 3
	}
	for i < len(buf) && htmlSpace(buf[i]) {
		i++
	}
	const dt = "<!doctype"
	if len(buf)-i < len(dt) || !strings.EqualFold(string(buf[i:i+len(dt)]), dt) {
		return 0
	}
	if j := bytes.IndexByte(buf[i:], '>'); j >= 0 {
		return i + j + 1
	}
	return 0
}
