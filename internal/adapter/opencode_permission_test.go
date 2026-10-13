package adapter

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// permServer is OpenCode's server as far as answering a permission goes:
// POST /permission/{id}/reply answers code with body, after hold if set.
type permServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	code int
	body string
	hold chan struct{}
}

func newPermServer(t *testing.T) *permServer {
	s := &permServer{code: http.StatusOK, body: "true"}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("x-opencode-directory")+" "+string(b))
		code, body, hold := s.code, s.body, s.hold
		s.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *permServer) port() int { return s.srv.Listener.Addr().(*net.TCPAddr).Port }

func (s *permServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

const askedChild = `{"type":"permission.asked","properties":{"id":"per_1","sessionID":"ses_kid","permission":"external_directory","patterns":["/etc/*"],"metadata":{},"always":["/etc/*"],"tool":{"messageID":"msg_1","callID":"toolu_1"}}}`

// A child's permission request, which OpenCode's ACP forwards to nobody,
// is allowed once over OpenCode's server; opencode.permission says so,
// and a lux.warning names the session, the permission and its patterns.
// The same request again is not answered again.
func TestOpenCodeChildPermissionAllowed(t *testing.T) {
	srv := newPermServer(t)
	a, sink := childRun(t, ocSession, srv.port(), nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, askedChild)
	onBus(t, a, askedChild)
	got := joined(a, sink)
	onBus(t, a, askedChild)
	wantRecords(t, joined(a, sink), got)
	wantRecords(t, got, []string{
		`opencode.session {"id":"ses_kid","parentID":"` + ocSession + `","title":"t","createdAt":1}`,
		`opencode.permission {"sessionID":"ses_kid","permissionID":"per_1","permission":"external_directory","patterns":["/etc/*"],"callID":"toolu_1","answered":"allow"}`,
		`lux.warning {"message":"opencode: a subagent session ses_kid asked for external_directory on /etc/*; lux allowed it"}`,
	})
	if want := []string{`POST /permission/per_1/reply application/json /workspace {"reply":"once"}`}; !slices.Equal(srv.requests(), want) {
		t.Fatalf("requests %q, want %q", srv.requests(), want)
	}
}

// The capture of a child asking (testdata/...-children-permission): the
// one permission.asked is the child's, answered over HTTP.
func TestOpenCodeChildPermissionCapture(t *testing.T) {
	srv := newPermServer(t)
	a, sink := childRun(t, "ses_ecc2f11cfffeaAFEEcNfwHJCeC", srv.port(), nil)
	replay(t, a, "opencode-1.18.21-children-permission.ndjson")
	perms := slices.DeleteFunc(joined(a, sink), func(r string) bool {
		return !strings.HasPrefix(r, "opencode.permission ") && !strings.HasPrefix(r, "lux.warning ")
	})
	wantRecords(t, perms, []string{
		`opencode.permission {"sessionID":"ses_ecc2f05bcffeO26eP2Q6hCnlL5","permissionID":"per_133d0ff15001tnvIm3JG5dUtyj","permission":"external_directory","patterns":["/etc/*"],"callID":"toolu_01FzAZzB2tNThBUBquzNwAiY","answered":"allow"}`,
		`lux.warning {"message":"opencode: a subagent session ses_ecc2f05bcffeO26eP2Q6hCnlL5 asked for external_directory on /etc/*; lux allowed it"}`,
	})
	if got := srv.requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /permission/per_133d0ff15001tnvIm3JG5dUtyj/reply ") {
		t.Fatalf("requests %q", got)
	}
}

// The Run's own session's request is answered over ACP (handleRequest):
// recorded as allowed, nothing sent over HTTP, no warning. A request of a
// session not of the Run is neither answered nor recorded.
func TestOpenCodeOwnPermissionRecordedOnly(t *testing.T) {
	srv := newPermServer(t)
	a, sink := childRun(t, ocSession, srv.port(), nil)
	onBus(t, a, strings.Replace(askedChild, "ses_kid", ocSession, 1))
	onBus(t, a, strings.Replace(strings.Replace(askedChild, "per_1", "per_2", 1), "ses_kid", "ses_stranger", 1))
	onBus(t, a, strings.Replace(askedChild, "per_1", "per_3", 1)) // ses_kid, before its session.created
	wantRecords(t, joined(a, sink), []string{
		`opencode.permission {"sessionID":"` + ocSession + `","permissionID":"per_1","permission":"external_directory","patterns":["/etc/*"],"callID":"toolu_1","answered":"allow"}`,
	})
	if got := srv.requests(); len(got) != 0 {
		t.Fatalf("requests %q", got)
	}
}

// OpenCode refusing the answer, answering with anything but true (as its
// web app answers 200 and HTML on a path it does not route), or not at
// all: opencode.permission says none, with the error, and the warning says
// lux could not answer it.
func TestOpenCodeChildPermissionFails(t *testing.T) {
	for _, c := range []struct {
		name, body, err string
		code            int
	}{
		{"not found", `{"_tag":"PermissionNotFoundError"}`, `POST /permission/per_1/reply: 404 Not Found {\"_tag\":\"PermissionNotFoundError\"}`, 404},
		{"html", "<!doctype html>", `POST /permission/per_1/reply: 200 OK \u003c!doctype html\u003e`, 200},
		{"false", "false", `POST /permission/per_1/reply: 200 OK false`, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newPermServer(t)
			srv.code, srv.body = c.code, c.body
			a, sink := childRun(t, ocSession, srv.port(), nil)
			onBus(t, a, ocSessionCreated("ses_kid", ocSession))
			onBus(t, a, askedChild)
			got := joined(a, sink)[1:]
			wantRecords(t, got, []string{
				`opencode.permission {"sessionID":"ses_kid","permissionID":"per_1","permission":"external_directory","patterns":["/etc/*"],"callID":"toolu_1","answered":"none","error":"` + c.err + `"}`,
				`lux.warning {"message":"opencode: a subagent session ses_kid asked for external_directory on /etc/*; lux could not answer it, and its task call may wait for it: ` + c.err + `"}`,
			})
		})
	}
}

// No server listening, and a server that does not answer within
// permissionTimeout: none, with the error; the bus goroutine is not held.
func TestOpenCodeChildPermissionUnreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	a, sink := childRun(t, ocSession, port, nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, askedChild)
	var p proto.OpenCodePermission
	_ = json.Unmarshal([]byte(strings.TrimPrefix(joined(a, sink)[1], "opencode.permission ")), &p)
	if p.Answered != proto.PermissionNone || !strings.Contains(p.Error, "connection refused") {
		t.Fatalf("unreachable: %+v", p)
	}

	srv := newPermServer(t)
	srv.hold = make(chan struct{})
	defer close(srv.hold)
	a, sink = childRun(t, ocSession, srv.port(), nil)
	a.permissionTimeout = 50 * time.Millisecond
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	start := time.Now()
	onBus(t, a, askedChild)
	if d := time.Since(start); d > 40*time.Millisecond {
		t.Fatalf("onBus waited %v for the answer", d)
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(joined(a, sink)[1], "opencode.permission ")), &p)
	if p.Answered != proto.PermissionNone || !strings.Contains(p.Error, "context deadline exceeded") {
		t.Fatalf("held: %+v", p)
	}
}

// A server lux did not start (an observer) is sent nothing: a child's
// request is recorded as not answered.
func TestOpenCodeChildPermissionObserverSendsNothing(t *testing.T) {
	srv := newPermServer(t)
	a, sink := childRun(t, ocSession, srv.port(), nil)
	a.bus = newOpencodeObserver(srv.port(), "/workspace", "opencode", "")
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	onBus(t, a, askedChild)
	got := joined(a, sink)
	if len(got) != 3 || !strings.Contains(got[1], `"answered":"none","error":"lux sends nothing but reads to a server it did not start"`) {
		t.Fatalf("records:\n%s", strings.Join(got, "\n"))
	}
	if r := srv.requests(); len(r) != 0 {
		t.Fatalf("requests %q", r)
	}
}

// A request that comes as the Run ends (nothing can be spawned) is
// recorded unanswered, at once.
func TestOpenCodeChildPermissionAfterRunEnded(t *testing.T) {
	srv := newPermServer(t)
	a, sink := childRun(t, ocSession, srv.port(), nil)
	onBus(t, a, ocSessionCreated("ses_kid", ocSession))
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	onBus(t, a, askedChild)
	got := sink.records()
	if len(got) != 3 || !strings.Contains(got[1], `"answered":"none","error":"the Run ended before lux could answer it"`) {
		t.Fatalf("records:\n%s", strings.Join(got, "\n"))
	}
	if r := srv.requests(); len(r) != 0 {
		t.Fatalf("requests %q", r)
	}
}
