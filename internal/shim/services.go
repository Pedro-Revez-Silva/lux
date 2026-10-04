package shim

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Services: HTTP services the workload calls through a unix socket each
// (/.lux/services/<name>.sock), with their headers added by the shim. The
// header values come from secrets and live only in this process's memory:
// never in a file, an environment or argv, and the shim is not dumpable
// (see Main), so a workload without CAP_SYS_PTRACE cannot read them from
// /proc either, even as container root.

// ServiceEnv names a service's socket for the workload:
// LUX_SERVICE_<NAME>=unix:/.lux/services/<name>.sock.
func ServiceEnv(name string) (string, string) {
	key := "LUX_SERVICE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return key, "unix:" + ServiceSocket(name)
}

func ServiceSocket(name string) string {
	return filepath.Join(proto.ShimServicesDir, name+".sock")
}

// serveServices starts a proxy per service, its socket owned by the
// workload user (mode 0600).
func (s *Shim) serveServices(secrets map[string]string) error {
	for i, svc := range s.cfg.Services {
		h, err := newServiceProxy(svc, secrets, s.red)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		path := ServiceSocket(svc.Name)
		os.Remove(path)
		ln, err := net.Listen("unix", path)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		if err := os.Chown(path, s.user.uid, s.user.gid); err != nil {
			return err
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		if svc.Loopback {
			// Bound before the workload starts, so nothing else holds the
			// port; the same port on every placement (spec.ServicePort).
			tcp, err := net.Listen("tcp", ServiceAddr(i))
			if err != nil {
				return fmt.Errorf("service %s on %s: %w", svc.Name, ServiceAddr(i), err)
			}
			go func() { _ = srv.Serve(tcp) }()
		}
	}
	return nil
}

// ServiceAddr is the loopback address of the i-th service.
func ServiceAddr(i int) string {
	return fmt.Sprintf("127.0.0.1:%d", spec.ServiceBasePort+i)
}

// serviceRootCAs verifies service upstreams; nil means the system roots.
// Tests set it to trust an httptest TLS server.
var serviceRootCAs *x509.CertPool

// newServiceProxy forwards every request to svc.URL (its path joined with
// the request's), with svc's headers set over the workload's own.
func newServiceProxy(svc spec.Service, secrets map[string]string, red *Redactor) (http.Handler, error) {
	target, err := url.Parse(svc.URL)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	for _, h := range svc.Headers {
		headers.Set(h.Name, secrets[h.Secret])
	}
	transport := &http.Transport{
		// Not the image's HTTP_PROXY: the request goes straight out, under
		// the Run's egress rules.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // streams may take their time
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{RootCAs: serviceRootCAs},
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			for k, v := range headers {
				r.Out.Header[k] = v
			}
		},
		Transport:     transport,
		FlushInterval: -1, // SSE and chunked responses as they come
		ModifyResponse: func(res *http.Response) error {
			// A 101 hands the connection over; its Connection: Upgrade must stay.
			if res.StatusCode != http.StatusSwitchingProtocols && bodyUnread(res.Request) {
				res.Header.Set("Connection", "close")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if bodyUnread(r) {
				w.Header().Set("Connection", "close")
			}
			reason := err.Error()
			if cerr := r.Context().Err(); cerr != nil {
				reason = cerr.Error()
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "lux: service %s unreachable: %s\n", svc.Name, red.Redact(reason))
		},
	}
	// url's path is a prefix the workload can't climb out of: a ".." segment
	// is refused, not cleaned (cleaning would also drop trailing slashes and
	// decode %2F, which APIs care about).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, seg := range strings.Split(r.URL.Path, "/") {
			if seg == ".." {
				http.Error(w, "lux: a service path may not contain ..", http.StatusBadRequest)
				return
			}
		}
		// The transport may still be sending the body after the upstream's
		// header goes out, which net/http supports only in full duplex. An
		// unread body is not drained because the response says
		// Connection: close (eofBody); full duplex covers any that do not.
		_ = http.NewResponseController(w).EnableFullDuplex()
		if r.ContentLength != 0 {
			b := &eofBody{ReadCloser: r.Body}
			r.Body = b
			r = r.WithContext(context.WithValue(r.Context(), eofBodyKey{}, b))
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

// eofBody records whether the request body was read to EOF. In full
// duplex, net/http decides to drop the connection only after the response
// has gone out (it cannot drain more than 256 KiB of unread body), so a
// response sent before the body was consumed must say Connection: close
// itself, or the workload reuses a connection the shim then closes. It is
// found through the request context (eofBodyKey): the transport may hand
// back a copy of the request with another Body, as HTTP/2 does.
type eofBody struct {
	io.ReadCloser
	eof atomic.Bool
}

type eofBodyKey struct{}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

// bodyUnread reports whether the inbound request r derives from has a body
// not yet read to EOF.
func bodyUnread(r *http.Request) bool {
	b, ok := r.Context().Value(eofBodyKey{}).(*eofBody)
	return ok && !b.eof.Load()
}
