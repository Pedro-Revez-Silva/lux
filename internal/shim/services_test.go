package shim

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

func TestServiceProxy(t *testing.T) {
	var got *http.Request
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/base/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				io.WriteString(w, "data: "+string(rune('a'+i))+"\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
			return
		}
		io.WriteString(w, "ok")
	}))
	defer up.Close()

	svc := spec.Service{Name: "tools", URL: up.URL + "/base", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "TOK"}}}
	h, err := newServiceProxy(svc, map[string]string{"TOK": "Bearer s3cret"}, NewRedactor(map[string]string{"TOK": "Bearer s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	// The workload's own Authorization is replaced; path and query joined.
	req, _ := http.NewRequest("POST", proxy.URL+"/items?x=1", strings.NewReader(`{"a":1}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Header.Get("Authorization") != "Bearer s3cret" || len(got.Header.Values("Authorization")) != 1 {
		t.Fatalf("auth %q", got.Header.Values("Authorization"))
	}
	if got.URL.Path != "/base/items" || got.URL.RawQuery != "x=1" || got.Method != "POST" || gotBody != `{"a":1}` {
		t.Fatalf("forwarded %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, gotBody)
	}

	// An absolute or //host target still goes to url's host, with the
	// header; ".." is refused; trailing slashes and %2F are kept.
	send := func(target string) string {
		c, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", target)
		b, _ := io.ReadAll(c)
		status, _, _ := strings.Cut(string(b), "\r\n")
		return status
	}
	for _, target := range []string{"//evil.example/etc", "http://evil.example/etc"} {
		send(target)
		if !strings.HasPrefix(got.URL.Path, "/base/") || got.Header.Get("Authorization") != "Bearer s3cret" {
			t.Fatalf("%s forwarded to %s", target, got.URL.Path)
		}
	}
	got = nil
	if st := send("/../../etc"); !strings.Contains(st, "400") || got != nil {
		t.Fatalf(".. was forwarded: %s", st)
	}
	send("/projects/group%2Fproject/issues/")
	if got.URL.EscapedPath() != "/base/projects/group%2Fproject/issues/" {
		t.Fatalf("path changed: %s", got.URL.EscapedPath())
	}

	// Streamed: each event arrives before the next is sent.
	resp, err = http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	start, n := time.Now(), 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			n++
			if n == 1 && time.Since(start) > 80*time.Millisecond {
				t.Fatal("the first event was held back")
			}
		}
	}
	resp.Body.Close()
	if n != 3 {
		t.Fatalf("%d events", n)
	}
}

func TestServiceProxyUnreachable(t *testing.T) {
	svc := spec.Service{Name: "gone", URL: "http://127.0.0.1:1", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "TOK"}}}
	h, _ := newServiceProxy(svc, map[string]string{"TOK": "Bearer s3cret"}, NewRedactor(map[string]string{"TOK": "Bearer s3cret"}))
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "service gone unreachable") || strings.Contains(string(b), "s3cret") {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}

func TestServiceEnv(t *testing.T) {
	k, v := ServiceEnv("my-tools")
	if k != "LUX_SERVICE_MY_TOOLS" || v != "unix:/.lux/services/my-tools.sock" {
		t.Fatalf("%s=%s", k, v)
	}
}

func newTestServiceProxy(t *testing.T, svc spec.Service) *httptest.Server {
	t.Helper()
	h, err := newServiceProxy(svc, nil, NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h)
}

// The workload holds back the rest of its body until the response header.
// Draining would deadlock; closing would interrupt the upstream upload.
// Covers Content-Length and chunked framing.
func TestServiceProxyAnswersBeforeTheBodyIsSent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).EnableFullDuplex()
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "got ")
		w.(http.Flusher).Flush()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read: %v", err)
		}
		io.WriteString(w, string(b))
	}))
	defer up.Close()

	proxy := newTestServiceProxy(t, spec.Service{Name: "tools", URL: up.URL})
	defer proxy.Close()

	for _, size := range []int64{int64(len("hello world")), -1} {
		t.Run(fmt.Sprintf("content-length %d", size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pr, pw := io.Pipe()
			// Unblocks the writer below whichever way the test ends.
			defer pr.Close()
			req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/upload", pr)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = size
			gotHeader := make(chan struct{})
			go func() {
				io.WriteString(pw, "hello")
				select {
				case <-gotHeader:
				case <-ctx.Done():
				}
				io.WriteString(pw, " world")
				pw.Close()
			}()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			close(gotHeader)
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != "got hello world" {
				t.Fatalf("status %d body %q err %v", resp.StatusCode, body, err)
			}
		})
	}
}

// postThenGet sends a POST declaring n body bytes (or chunked, of unknown
// length) and only its first sent bytes, then reads the response. Unless that
// says close, it sends the rest of the body and a GET on the same connection
// and returns the GET's response; the connection is closed on return.
func postThenGet(t *testing.T, addr, path string, n, sent int, chunked bool) (post, get *http.Response, getErr error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if chunked {
		// One chunk of sent bytes, no terminator: the body never ends.
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", path, sent, strings.Repeat("x", sent))
	} else {
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", path, n, strings.Repeat("x", sent))
	}
	br := bufio.NewReader(c)
	post, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, post.Body)
	if post.Close {
		return post, nil, nil
	}
	// The server may stop reading once it has answered; writing in the
	// background keeps a blocked write from hiding the GET's outcome.
	go func() {
		if chunked {
			fmt.Fprintf(c, "%x\r\n%s\r\n0\r\n\r\n", n-sent, make([]byte, n-sent))
		} else {
			c.Write(make([]byte, n-sent))
		}
		fmt.Fprintf(c, "GET /next HTTP/1.1\r\nHost: x\r\n\r\n")
	}()
	get, getErr = http.ReadResponse(br, nil)
	if getErr == nil {
		io.Copy(io.Discard, get.Body)
	}
	return post, get, getErr
}

// An unread upload must close the connection; a fully read upload must
// keep it usable for the next request.
func TestServiceProxyEarlyAnswerClosesAnUnreadUpload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reject" {
			// Else net/http drains a chunked body of unknown size before the header.
			http.NewResponseController(w).EnableFullDuplex()
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	proxy := newTestServiceProxy(t, spec.Service{Name: "tools", URL: up.URL})
	defer proxy.Close()
	addr := proxy.Listener.Addr().String()

	const n = 1 << 20
	for _, chunked := range []bool{false, true} {
		post, get, err := postThenGet(t, addr, "/reject", n, 5, chunked)
		if post.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("post (chunked %v): status %d", chunked, post.StatusCode)
		}
		if !post.Close {
			t.Fatalf("upload unread (chunked %v): keep-alive promised, next request: %v %v", chunked, get, err)
		}
	}

	// The whole body reaches the upstream before it answers.
	post, get, err := postThenGet(t, addr, "/store", n, n, false)
	if post.Close || err != nil || get.StatusCode != http.StatusOK {
		t.Fatalf("upload sent in full: close %v, next request: %v %v", post.Close, get, err)
	}
}

// Over HTTP/2 the transport reuses one upstream connection, and from the
// second request on it hands ModifyResponse a copy of the request.
func TestServiceProxyEarlyAnswerClosesAnUnreadUploadOverHTTP2(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proto", r.Proto)
		http.Error(w, "too big", http.StatusRequestEntityTooLarge)
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	defer up.Close()
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	serviceRootCAs = roots
	defer func() { serviceRootCAs = nil }()
	proxy := newTestServiceProxy(t, spec.Service{Name: "tools", URL: up.URL})
	defer proxy.Close()

	for i := range 3 {
		post, get, err := postThenGet(t, proxy.Listener.Addr().String(), "/reject", 1<<20, 5, false)
		if post.StatusCode != http.StatusRequestEntityTooLarge || post.Header.Get("X-Proto") != "HTTP/2.0" {
			t.Fatalf("post %d: status %d over %q", i, post.StatusCode, post.Header.Get("X-Proto"))
		}
		if !post.Close {
			t.Fatalf("post %d: 1 MiB upload unread: keep-alive promised, next request: %v %v", i, get, err)
		}
	}
}

// An upstream may switch protocols before it has read the request body.
func TestServiceProxyUpgradeWithAnUnreadBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: foo\r\n\r\n")
		brw.Flush()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, brw)
	}))
	defer up.Close()
	proxy := newTestServiceProxy(t, spec.Service{Name: "tools", URL: up.URL})
	defer proxy.Close()

	c, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "POST /u HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: foo\r\nContent-Length: 1000\r\n\r\nhello")
	// A 101 needs none of the rest; a failed upgrade's 502 goes out only once
	// the body ends, so send it late rather than never.
	defer time.AfterFunc(time.Second, func() { c.Write(make([]byte, 995)) }).Stop()
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		t.Fatalf("status %d %q", res.StatusCode, b)
	}
	if res.Header.Get("Connection") != "Upgrade" || res.Header.Get("Upgrade") != "foo" {
		t.Fatalf("upgrade headers %v", res.Header)
	}
}

// An unreachable upstream also answers before the upload is read.
func TestServiceProxyUnreachableClosesAnUnreadUpload(t *testing.T) {
	proxy := newTestServiceProxy(t, spec.Service{Name: "gone", URL: "http://127.0.0.1:1"})
	defer proxy.Close()
	post, get, err := postThenGet(t, proxy.Listener.Addr().String(), "/up", 1<<20, 5, false)
	if post.StatusCode != http.StatusBadGateway {
		t.Fatalf("post: status %d", post.StatusCode)
	}
	if !post.Close {
		t.Fatalf("1 MiB upload unread: keep-alive promised, next request: %v %v", get, err)
	}
}

// The workload sends each chunked line only after reading the previous echo.
func TestServiceProxyStreamsBothWays(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).EnableFullDuplex()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			io.WriteString(w, sc.Text()+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()
	proxy := newTestServiceProxy(t, spec.Service{Name: "tools", URL: up.URL})
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	// The transport waits for the body before Do fails; end it on timeout.
	defer context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()) })()
	req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/chat", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	go io.WriteString(pw, "a\n")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for _, next := range []string{"b", "c"} {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("echo before %q: %q %v", next, line, err)
		}
		io.WriteString(pw, next+"\n")
	}
	pw.Close()
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "c\n" {
		t.Fatalf("last echo %q %v", rest, err)
	}
}
