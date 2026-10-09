package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const ocPassword = "s3cr3t-opencode-pw"

// fullSink is an inputSink that also keeps every event, with its data,
// and everything written to stdout and stderr, to search for a secret;
// warnings gets the message of each lux.warning.
type fullSink struct {
	*inputSink
	mu       sync.Mutex
	all      []string
	warnings chan string
}

func (s *fullSink) keep(v string) {
	s.mu.Lock()
	s.all = append(s.all, v)
	s.mu.Unlock()
}

func (s *fullSink) Event(typ string, v any) {
	b, _ := json.Marshal(v)
	s.keep(typ + " " + string(b))
	if typ == proto.EvWarning {
		m, _ := v.(map[string]any)
		msg, _ := m["message"].(string)
		select {
		case s.warnings <- msg:
		default:
			panic("more lux.warning events than the test's sink holds")
		}
	}
	s.inputSink.Event(typ, v)
}
func (s *fullSink) Stdout(p []byte) { s.keep(string(p)) }
func (s *fullSink) Stderr(p []byte) { s.keep(string(p)) }

func newFullSink() *fullSink {
	return &fullSink{inputSink: &inputSink{}, warnings: make(chan string, 16)}
}

func (s *fullSink) nextWarning(t *testing.T) string {
	t.Helper()
	select {
	case m := <-s.warnings:
		return m
	case <-waitTimeout():
		t.Fatal("no lux.warning")
	}
	return ""
}

// noMoreWarnings fails if a lux.warning came that nextWarning did not
// take; call it after the adapter's Run has returned.
func (s *fullSink) noMoreWarnings(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.warnings:
		t.Fatalf("lux.warning %q", m)
	default:
	}
}

func (s *fullSink) noSecret(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range append(s.all, s.inputSink.lines()...) {
		if strings.Contains(l, ocPassword) {
			t.Fatalf("the password was reported: %q", l)
		}
	}
}

// jervasionCommand is a wrapper's argv in the shape of Jervasion's
// entrypoint: no bare "acp", a --port of its own the wrapper's opencode
// serves on, and another port that is not OpenCode's.
func jervasionCommand(port int) []string {
	return []string{"jervasion-agent-entrypoint", "--acp", "--port", strconv.Itoa(port), "--readiness-port", "4097", "--policy-oid", "0123abcd"}
}

// ocObserved starts an OpenCode adapter on a Jervasion-shaped command whose
// server is b, with env as the workload's environment, through the first
// prompt.
func ocObserved(t *testing.T, b *fakeBus, env []string) (*ACP, *agentWire, *fullSink, string) {
	t.Helper()
	return ocObservedOn(t, b.port(), env, nil)
}

// ocObservedOn is ocObserved on the server at 127.0.0.1:port; setup, if
// set, runs on the adapter after Command and before Run.
func ocObservedOn(t *testing.T, port int, env []string, setup func(*ACP)) (*ACP, *agentWire, *fullSink, string) {
	t.Helper()
	a := NewOpenCode()
	a.WorkloadEnv(env)
	cmd := jervasionCommand(port)
	if argv, err := a.Command(proto.ShimConfig{Command: cmd, Workdir: "/workspace"}); err != nil || strings.Join(argv, " ") != strings.Join(cmd, " ") {
		t.Fatalf("argv %q, %v", argv, err)
	}
	if setup != nil {
		setup(a)
	}
	sink := newFullSink()
	w, first := ocStartedOn(t, a, sink, sink.inputSink)
	return a, w, sink, first
}

// recServer is an HTTP server that records each request as "METHOD path",
// and the requests that carried an Authorization header.
type recServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	reqs, authed []string
}

func newRecServer(t *testing.T, h http.HandlerFunc) *recServer {
	s := &recServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			s.authed = append(s.authed, r.Method+" "+r.URL.Path)
		}
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *recServer) port() int { return s.srv.Listener.Addr().(*net.TCPAddr).Port }

func (s *recServer) snapshot() (reqs, authed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs), slices.Clone(s.authed)
}

// onlyBus fails unless every request in reqs is one of the two an observer
// may send.
func onlyBus(t *testing.T, what string, reqs []string) {
	t.Helper()
	for _, r := range reqs {
		if r != "GET /event" && r != "GET /session/status" {
			t.Fatalf("%s: request %q beyond GET /event and GET /session/status (all: %q)", what, r, reqs)
		}
	}
}

// serveStream answers an event stream: server.connected, then events, then
// nothing until the client leaves.
func serveStream(w http.ResponseWriter, r *http.Request, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
	}
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// serveBusy answers as an OpenCode server whose session is busy: its event
// stream says so, as does any other GET.
func serveBusy(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/event") {
		serveStream(w, r, ocStatus("busy"))
		return
	}
	fmt.Fprintf(w, `{"%s":{"type":"busy"}}`, ocSession)
}

// An observed server that redirects GET /event or GET /session/status, to
// another loopback server or to another path of its own: lux does not
// follow. The target gets no request, no Authorization goes anywhere but
// the two bus endpoints, a redirected stream is retried after a backoff, a
// redirected status read is an error, and nothing the targets say (busy)
// reaches the Run's activity.
func TestOpenCodeObserverDoesNotFollowRedirects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoint  string
		elsewhere bool
	}{
		{"event to another server", "/event", true},
		{"event to another path", "/event", false},
		{"status to another server", "/session/status", true},
		{"status to another path", "/session/status", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elsewhere := newRecServer(t, serveBusy)
			target := "/elsewhere" + tc.endpoint
			if tc.elsewhere {
				target = elsewhere.srv.URL + tc.endpoint
			}
			observed := newRecServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == tc.endpoint:
					http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				case r.URL.Path == "/event":
					serveStream(w, r)
				case r.URL.Path == "/session/status":
					fmt.Fprint(w, `{}`)
				default:
					serveBusy(w, r)
				}
			})
			paused := make(chan time.Duration, 1)
			statusErr := make(chan error, 64)
			_, w, sink, first := ocObservedOn(t, observed.port(), []string{"OPENCODE_SERVER_PASSWORD=" + ocPassword}, func(a *ACP) {
				a.bus.pause = func(_ context.Context, d time.Duration) bool {
					paused <- d
					return false // the test has seen the one attempt it needs
				}
				a.statusRead = func(err error) {
					select {
					case statusErr <- err:
					default:
					}
				}
			})
			if tc.endpoint == "/event" {
				select {
				case <-paused:
				case <-waitTimeout():
					t.Fatal("a redirected GET /event was not retried after a backoff")
				}
			} else {
				for done := false; !done; {
					select {
					case err := <-statusErr:
						if err == nil {
							t.Fatal("a redirected status read succeeded")
						}
						done = !errors.Is(err, context.Canceled)
					case <-waitTimeout():
						t.Fatal("no status read")
					}
				}
			}
			w.resolve(first, ocResult)
			checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
			if reqs, authed := elsewhere.snapshot(); len(reqs) != 0 || len(authed) != 0 {
				t.Fatalf("the redirect's target got %q (authorized: %q)", reqs, authed)
			}
			reqs, authed := observed.snapshot()
			onlyBus(t, "observed", reqs)
			onlyBus(t, "observed, authorized", authed)
			if !slices.Contains(reqs, "GET "+tc.endpoint) {
				t.Fatalf("observed: no GET %s in %q", tc.endpoint, reqs)
			}
			sink.noSecret(t)
		})
	}
}

// A command's own port is its last --port, in either form; a last --port
// with no value, or one that is not a TCP port, means none, whatever came
// before it. Other flags ending in "port" are not it.
func TestOwnPort(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		port int // 0: none
	}{
		{[]string{"w"}, 0},
		{[]string{"w", "--port", "4096"}, 4096},
		{[]string{"w", "--port=4096"}, 4096},
		{[]string{"w", "--port"}, 0},
		{[]string{"w", "--port", "4096", "--port"}, 0},
		{[]string{"w", "--port=4096", "--port"}, 0},
		{[]string{"w", "--port="}, 0},
		{[]string{"w", "--port", "4096", "--port="}, 0},
		{[]string{"w", "--port", "4096", "--port", "x"}, 0},
		{[]string{"w", "--port=4096", "--port=x"}, 0},
		{[]string{"w", "--port", "4096", "--port", ""}, 0},
		{[]string{"w", "--port", "4096", "--port", "5000"}, 5000},
		{[]string{"w", "--port", "4096", "--port=5000"}, 5000},
		{[]string{"w", "--port=4096", "--port", "5000"}, 5000},
		{[]string{"w", "--port=4096", "--port=5000"}, 5000},
		{[]string{"w", "--port", "x", "--port", "5000"}, 5000},
		{[]string{"w", "--port", "0"}, 0},
		{[]string{"w", "--port", "1"}, 1},
		{[]string{"w", "--port", "65535"}, 65535},
		{[]string{"w", "--port", "65536"}, 0},
		{[]string{"w", "--port", "-1"}, 0},
		{[]string{"w", "--port", "4096", "--port", "65536"}, 0},
		{[]string{"w", "--readiness-port", "4097"}, 0},
		{[]string{"w", "--readiness-port=4097"}, 0},
		{[]string{"w", "--port", "4096", "--readiness-port", "4097"}, 4096},
		{[]string{"w", "--portx", "4096"}, 0},
		{[]string{"w", "--port", "--readiness-port", "4097"}, 0},
	} {
		port, ok := ownPort(tc.argv)
		if ok != (tc.port != 0) || (ok && port != tc.port) {
			t.Errorf("ownPort(%q) = %d, %v; want %d", tc.argv, port, ok, tc.port)
		}
	}
}

// A command lux did not build is followed on its own port, and its own
// port only: a trailing --port with no valid value leaves it unobserved
// rather than falling back to an earlier one, so the server there gets no
// request.
func TestOpenCodeObservesOnlyTheLastPort(t *testing.T) {
	earlier := newRecServer(t, serveBusy)
	a := NewOpenCode()
	a.WorkloadEnv([]string{"OPENCODE_SERVER_PASSWORD=" + ocPassword})
	cmd := []string{"wrapper", "--acp", "--port", strconv.Itoa(earlier.port()), "--port"}
	argv, err := a.Command(proto.ShimConfig{Command: cmd, Workdir: "/workspace"})
	if err != nil || strings.Join(argv, " ") != strings.Join(cmd, " ") {
		t.Fatalf("argv %q, %v", argv, err)
	}
	sink := newFullSink()
	w, first := ocStartedOn(t, a, sink, sink.inputSink)
	w.resolve(first, ocResult)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
	if reqs, _ := earlier.snapshot(); len(reqs) != 0 {
		t.Fatalf("the earlier --port's server got %q", reqs)
	}
}

// A command lux did not build, with OpenCode's server on its own --port
// behind Basic auth: lux follows that server's bus, authenticated, for the
// Run's activity. A loop a client starts over HTTP shows the Run busy, then
// idle; lux's steers still go over ACP, without receipts, and lux sends
// nothing to the server but its bus and status reads. The password is in
// nothing lux reports.
func TestOpenCodeObservesServerItDidNotStart(t *testing.T) {
	b := newFakeBus(t)
	b.password = ocPassword
	b.setLoop(true) // the first prompt's loop
	a, w, sink, first := ocObserved(t, b, []string{"PATH=/bin", "OPENCODE_SERVER_PASSWORD=" + ocPassword})
	waitGen(t, a, 1)
	waitHandled(t, a)

	a.Deliver(proto.Input{RequestID: "steer", Text: "also check the tests"})
	second, p := w.next("session/prompt")
	if str(p, "sessionId") != ocSession {
		t.Fatalf("params %v", p)
	}
	sink.wait(t, "accepted steer next_step receipt=false")
	b.setLoop(false)
	w.resolve(first, ocResult)
	w.resolve(second, ocResult)
	sink.waitLast(t, "idle")

	// A review a client drives over OpenCode's HTTP API.
	b.setLoop(true)
	b.events <- ocStatus("busy")
	sink.waitLast(t, "busy")
	b.events <- b.answer("msg_0f274ed8a001VXpumPDla0AnsH")
	b.setLoop(false)
	gen := busGen(a)
	b.events <- ocStatus("idle")
	b.events <- ocIdle
	waitGen(t, a, gen+2)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false",
		"accepted steer next_step receipt=false", "turn_end", "idle", "busy", "idle")

	b.mu.Lock()
	authed, refused := b.authed, b.refused
	var reqs []string
	for r, n := range b.requests {
		for range n {
			reqs = append(reqs, r)
		}
	}
	b.mu.Unlock()
	if authed == 0 || refused != 0 {
		t.Fatalf("Basic auth: %d requests authenticated, %d refused", authed, refused)
	}
	onlyBus(t, "observed", reqs)
	sink.noSecret(t)
}

// The last OPENCODE_SERVER_USERNAME stands, and an empty one is the
// default user, as it is when it is the only one.
func TestOpenCodeObserverEmptyUsernameIsDefault(t *testing.T) {
	b := newFakeBus(t)
	b.password = ocPassword
	reads := make(statusReads, 64)
	_, w, sink, first := ocObservedOn(t, b.port(), []string{"OPENCODE_SERVER_USERNAME=alice", "OPENCODE_SERVER_PASSWORD=" + ocPassword, "OPENCODE_SERVER_USERNAME="}, func(a *ACP) {
		a.statusRead = reads.hook
	})
	if err := reads.next(t); err != nil {
		t.Fatalf("authenticated status read: %v", err)
	}
	w.resolve(first, ocResult)
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
	b.mu.Lock()
	authed, refused := b.authed, b.refused
	b.mu.Unlock()
	if authed == 0 || refused != 0 {
		t.Fatalf("Basic auth: %d requests authenticated, %d refused", authed, refused)
	}
}

// The server refuses lux's password on every GET /event: lux asks again
// after each backoff, refusalLimit times in all, then gives exactly one
// warning and sends nothing more. The Run's activity is lux's own turns',
// with no busy from OpenCode.
func TestOpenCodeObservedWrongPasswordWarnsOnce(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			b := newFakeBus(t)
			if code == http.StatusUnauthorized {
				b.password = "the-right-one"
			} else {
				b.deny = func(string) int { return code }
			}
			b.setLoop(true)
			var pauses int
			_, w, sink, first := ocObservedOn(t, b.port(), []string{"OPENCODE_SERVER_PASSWORD=" + ocPassword}, func(a *ACP) {
				a.bus.pause = func(context.Context, time.Duration) bool { pauses++; return true }
			})
			want := fmt.Sprintf("opencode: its server refused lux %d times in a row; lux no longer follows its activity: refused: GET /event: %d %s",
				refusalLimit, code, http.StatusText(code))
			if got := sink.nextWarning(t); got != want {
				t.Fatalf("warning %q, want %q", got, want)
			}
			atWarning := b.count("GET /event")
			w.resolve(first, ocResult)
			checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
			sink.noMoreWarnings(t)
			b.mu.Lock()
			refused, authed, kinds, reqs := b.refused, b.authed, len(b.requests), fmt.Sprint(b.requests)
			b.mu.Unlock()
			if atWarning != refusalLimit || refused != refusalLimit || authed != 0 || pauses != refusalLimit-1 || kinds != 1 {
				t.Fatalf("GET /event %d at the warning; %d refused, %d authenticated, %d backoffs; requests %s",
					atWarning, refused, authed, pauses, reqs)
			}
			sink.noSecret(t)
		})
	}
}

// A server that refuses lux for a while (still setting up its auth, or
// restarting) and then accepts it is followed: an accepted status read
// clears the count of refusals, so two runs of refusalLimit-1 refusals give
// no warning, and OpenCode's busy reaches the Run once it accepts.
func TestOpenCodeObserverRetriesRefusal(t *testing.T) {
	b := newFakeBus(t)
	connected := make(chan struct{}, 2)
	n := 0
	b.deny = func(req string) int {
		if req != "GET /event" {
			return 0
		}
		n++
		switch {
		case n == refusalLimit || n == 2*refusalLimit:
			connected <- struct{}{}
			return 0
		case n%2 == 0:
			return http.StatusForbidden
		}
		return http.StatusUnauthorized
	}
	pauses := 0
	reads := make(statusReads, 64)
	_, w, sink, first := ocObservedOn(t, b.port(), nil, func(a *ACP) {
		a.bus.pause = func(context.Context, time.Duration) bool { pauses++; return true }
		a.statusRead = reads.hook
	})
	w.resolve(first, ocResult)
	sink.waitLast(t, "idle")
	await(t, connected, "the first accepted GET /event")
	if err := reads.next(t); err != nil {
		t.Fatalf("status read on the first stream: %v", err)
	}
	b.drop <- struct{}{}
	await(t, connected, "the second accepted GET /event")
	if err := reads.next(t); err != nil {
		t.Fatalf("status read on the second stream: %v", err)
	}
	// Only the second stream is open: the event goes there.
	b.events <- ocStatus("busy")
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle", "busy")
	sink.noMoreWarnings(t)
	if got := b.count("GET /event"); got != 2*refusalLimit || pauses != 2*refusalLimit-1 {
		t.Fatalf("GET /event %d times, %d backoffs", got, pauses)
	}
}

// statusReads is a test's view of an adapter's status reads (statusRead).
type statusReads chan error

func (c statusReads) hook(err error) {
	select {
	case c <- err:
	default:
		panic("more status reads than the test's channel holds")
	}
}

// next is the error of the next status read that was not superseded or
// cancelled.
func (c statusReads) next(t *testing.T) error {
	t.Helper()
	for {
		select {
		case err := <-c:
			if !errors.Is(err, context.Canceled) {
				return err
			}
		case <-waitTimeout():
			t.Fatal("no status read")
		}
	}
}

// The server accepts lux's event stream but refuses its status reads: each
// refusal counts as the stream's do, and drops the stream, so lux backs
// off, reconnects and reads the status again. After refusalLimit refusals
// in a row: one warning, and no request after it. OpenCode's busy, which
// the stream says on every connect after the first refusal, never reaches
// the Run.
func TestOpenCodeObserverStatusRefusedGivesUp(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var mu sync.Mutex
			streams := 0
			srv := newRecServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/event" {
					http.Error(w, http.StatusText(code), code)
					return
				}
				mu.Lock()
				streams++
				n := streams
				mu.Unlock()
				switch n {
				case 1: // lux connects again once the session is known
					http.Error(w, "starting", http.StatusServiceUnavailable)
				case 2:
					serveStream(w, r)
				default:
					serveStream(w, r, ocStatus("busy"))
				}
			})
			gate := make(chan struct{})
			pauses := 0
			_, w, sink, first := ocObservedOn(t, srv.port(), []string{"OPENCODE_SERVER_PASSWORD=" + ocPassword}, func(a *ACP) {
				a.bus.pause = func(ctx context.Context, _ time.Duration) bool {
					if pauses++; pauses == 1 {
						select {
						case <-gate:
						case <-ctx.Done():
							return false
						}
					}
					return true
				}
			})
			close(gate)
			want := fmt.Sprintf("opencode: its server refused lux %d times in a row; lux no longer follows its activity: refused: GET /session/status: %d %s",
				refusalLimit, code, http.StatusText(code))
			if got := sink.nextWarning(t); got != want {
				t.Fatalf("warning %q, want %q", got, want)
			}
			atWarning, _ := srv.snapshot()
			w.resolve(first, ocResult)
			checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
			sink.noMoreWarnings(t)
			reqs, authed := srv.snapshot()
			onlyBus(t, "observed", reqs)
			events, statuses := 0, 0
			for _, r := range reqs {
				if r == "GET /event" {
					events++
				} else {
					statuses++
				}
			}
			if len(reqs) != len(atWarning) || len(authed) != len(reqs) || events != refusalLimit+1 ||
				statuses != refusalLimit || pauses != refusalLimit {
				t.Fatalf("%d requests at the warning, %d after (%d authorized); %d GET /event, %d GET /session/status, %d backoffs",
					len(atWarning), len(reqs), len(authed), events, statuses, pauses)
			}
			sink.noSecret(t)
		})
	}
}

// A refused status read after OpenCode's busy was shown: the Run is idle at
// once, and lux reconnects after a backoff. A busy event on the stream while
// the status is still refused is not shown; once the server accepts the
// status read again, its busy is.
func TestOpenCodeObserverStatusRefusalAfterActivity(t *testing.T) {
	b := newFakeBus(t)
	b.setLoop(true)
	paused, resume := make(chan struct{}), make(chan struct{})
	reads := make(statusReads, 64)
	handled := make(chan string, 64)
	_, w, sink, first := ocObservedOn(t, b.port(), nil, func(a *ACP) {
		a.bus.pause = func(ctx context.Context, _ time.Duration) bool {
			select {
			case paused <- struct{}{}:
			case <-ctx.Done():
				return false
			}
			select {
			case <-resume:
				return true
			case <-ctx.Done():
				return false
			}
		}
		a.statusRead = reads.hook
		a.busHandled = func(ev busEvent) { handled <- ev.Type }
	})
	if err := reads.next(t); err != nil {
		t.Fatalf("status read: %v", err)
	}
	b.setStatusCode(http.StatusForbidden)
	// Lux's turn ends while OpenCode's busy: the status is read again.
	w.resolve(first, ocResult)
	if err := reads.next(t); !errors.Is(err, errRefused) {
		t.Fatalf("status read after the turn: %v", err)
	}
	await(t, paused, "a backoff after the refused status read")
	sink.waitLast(t, "idle")

	b.holdStatus(true)
	resume <- struct{}{}
	held := b.nextStatus(t)
	b.events <- ocStatus("busy")
	for ev := ""; ev != "session.status"; {
		select {
		case ev = <-handled:
		case <-waitTimeout():
			t.Fatal("the busy event was not handled")
		}
	}
	b.holdStatus(false)
	close(held.release)
	if err := reads.next(t); !errors.Is(err, errRefused) {
		t.Fatalf("held status read: %v", err)
	}
	await(t, paused, "a backoff after the second refused status read")
	sink.waitLast(t, "idle")

	b.setStatusCode(0)
	resume <- struct{}{}
	if err := reads.next(t); err != nil {
		t.Fatalf("status read once accepted: %v", err)
	}
	checkLines(t, w, sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle", "busy")
	sink.noMoreWarnings(t)
}

func TestOpenCodeObserverRecoveryReconcilesMaskedStatus(t *testing.T) {
	for _, status := range []string{"busy", "idle"} {
		t.Run(status, func(t *testing.T) {
			o := newStepObserver(t, func(b *fakeBus) {
				b.setLoop(true)
				b.setStatusCode(http.StatusForbidden)
			})
			o.step()
			refused := o.nextGot(t)
			close(refused.release)
			if err := o.nextRead(t); !errors.Is(err, errRefused) {
				t.Fatalf("initial refusal: %v", err)
			}
			o.backoff(t, "refused stream cancellation")
			await(t, o.b.streamEnded, "the refused SSE handler exiting")
			o.b.setStatusCode(0)
			o.b.holdStatus(true)
			o.step()
			held := o.b.nextStatus(t)
			o.b.events <- ocStatus(status)
			o.statusHandled(t)
			close(held.release)
			recovery := o.nextGot(t)
			close(recovery.release)
			if err := o.nextRead(t); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			want := []string{"idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle"}
			if status == "busy" {
				want = append(want, "busy")
			}
			// statusRead is an application milestone: no event or reread
			// is needed to expose the latest observed status.
			if got := o.sink.lines(); !slices.Equal(got, want) {
				t.Fatalf("activity after recovery: %q, want %q", got, want)
			}
			o.b.holdStatus(false)
			o.unstep()
			o.w.exit()
			o.sink.noMoreWarnings(t)
			if got := o.b.count("GET /session/status"); got != 2 {
				t.Fatalf("status reads: %d, want 2", got)
			}
		})
	}
}

// stepObserver is an observer of a fakeBus the test drives step by step:
// each reconnect backoff waits for step, and each status read, once its
// response is in, waits for the test to release it (got), until unstep.
// Its first GET /event fails, Lux's turn is over and the Run idle.
type stepObserver struct {
	b                    *fakeBus
	w                    *agentWire
	sink                 *fullSink
	paused, resume, free chan struct{}
	got                  chan gotStatus
	reads                statusReads
	handled              chan string
	freeOnce             sync.Once
}

// gotStatus is a status read whose response is in, held until release
// closes.
type gotStatus struct {
	err     error
	release chan struct{}
}

func newStepObserver(t *testing.T, setup func(*fakeBus)) *stepObserver {
	t.Helper()
	o := &stepObserver{b: newFakeBus(t), paused: make(chan struct{}), resume: make(chan struct{}),
		free: make(chan struct{}), got: make(chan gotStatus), reads: make(statusReads, 256), handled: make(chan string, 1024)}
	t.Cleanup(o.unstep)
	o.b.streamEnded = make(chan struct{}, 256)
	o.b.setEventFail(true)
	if setup != nil {
		setup(o.b)
	}
	_, w, sink, first := ocObservedOn(t, o.b.port(), nil, func(a *ACP) {
		a.bus.pause = func(ctx context.Context, _ time.Duration) bool {
			select {
			case o.paused <- struct{}{}:
			case <-o.free:
				return true
			case <-ctx.Done():
				return false
			}
			select {
			case <-o.resume:
				return true
			case <-o.free:
				return true
			case <-ctx.Done():
				return false
			}
		}
		a.statusGot = func(err error) {
			h := gotStatus{err, make(chan struct{})}
			select {
			case o.got <- h:
			case <-o.free:
				return
			}
			select {
			case <-h.release:
			case <-o.free:
			}
		}
		a.statusRead = o.reads.hook
		a.busHandled = func(ev busEvent) { o.handled <- ev.Type }
	})
	o.w, o.sink = w, sink
	w.resolve(first, ocResult)
	sink.waitLast(t, "idle")
	await(t, o.paused, "a backoff after the failed GET /event")
	o.b.setEventFail(false)
	return o
}

// step ends the backoff the follower is in, so it connects again.
func (o *stepObserver) step() { o.resume <- struct{}{} }

func (o *stepObserver) backoff(t *testing.T, what string) {
	t.Helper()
	await(t, o.paused, what)
}

// nextGot is the next status read whose response is in, held.
func (o *stepObserver) nextGot(t *testing.T) gotStatus {
	t.Helper()
	select {
	case h := <-o.got:
		return h
	case <-waitTimeout():
		t.Fatal("no status response")
	}
	return gotStatus{}
}

// nextRead is the error of the next status read applied or discarded,
// cancelled or not.
func (o *stepObserver) nextRead(t *testing.T) error {
	t.Helper()
	select {
	case err := <-o.reads:
		return err
	case <-waitTimeout():
		t.Fatal("no status read")
	}
	return nil
}

// statusHandled waits until the adapter has handled a session.status event.
func (o *stepObserver) statusHandled(t *testing.T) {
	t.Helper()
	for ev := ""; ev != "session.status"; {
		select {
		case ev = <-o.handled:
		case <-waitTimeout():
			t.Fatal("the session.status event was not handled")
		}
	}
}

// unstep lets backoffs and status reads run without the test from now on.
func (o *stepObserver) unstep() { o.freeOnce.Do(func() { close(o.free) }) }

func (b *fakeBus) setStatusCode(code int) {
	b.mu.Lock()
	b.statusCode = code
	b.mu.Unlock()
}

// Status read A succeeds, but its stream drops before A is applied; the
// next stream's read B is refused. A, superseded, does not clear B's
// refusal: a busy event on the stream after it stays masked, and the count
// goes on from B's refusal, so lux gives up after refusalLimit refused
// reads, with one warning, and is idle throughout.
func TestOpenCodeObserverStaleSuccessKeepsNewerRefusal(t *testing.T) {
	o := newStepObserver(t, func(b *fakeBus) { b.setLoop(true) })
	o.step()
	readA := o.nextGot(t)
	if readA.err != nil {
		t.Fatalf("read A: %v", readA.err)
	}
	o.b.drop <- struct{}{}
	o.backoff(t, "a backoff after the first stream dropped")
	await(t, o.b.streamEnded, "the first SSE handler exiting")
	o.b.setStatusCode(http.StatusForbidden)
	o.step()
	readB := o.nextGot(t)
	if !errors.Is(readB.err, errRefused) {
		t.Fatalf("read B: %v", readB.err)
	}
	close(readB.release)
	if err := o.nextRead(t); !errors.Is(err, errRefused) {
		t.Fatalf("read B applied: %v", err)
	}
	o.backoff(t, "a backoff after read B's refusal")
	await(t, o.b.streamEnded, "the refused SSE handler exiting")
	close(readA.release)
	if err := o.nextRead(t); err != nil {
		t.Fatalf("read A applied: %v", err)
	}

	// The third stream: its read is held while a busy event arrives.
	o.b.holdStatus(true)
	o.step()
	held := o.b.nextStatus(t)
	o.b.events <- ocStatus("busy")
	o.statusHandled(t)
	o.b.holdStatus(false)
	o.unstep()
	close(held.release)
	want := fmt.Sprintf("opencode: its server refused lux %d times in a row; lux no longer follows its activity: refused: GET /session/status: 403 Forbidden",
		refusalLimit)
	if got := o.sink.nextWarning(t); got != want {
		t.Fatalf("warning %q, want %q", got, want)
	}
	statuses := o.b.count("GET /session/status")
	checkLines(t, o.w, o.sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle")
	o.sink.noMoreWarnings(t)
	// Read A, then refusalLimit refused reads.
	if statuses != 1+refusalLimit || o.b.count("GET /session/status") != statuses {
		t.Fatalf("GET /session/status %d times at the warning, %d in all", statuses, o.b.count("GET /session/status"))
	}
}

// Status read A is refused, but its stream drops before A is applied; the
// next stream's read B succeeds, busy. A, superseded, neither drops the
// recovered stream nor shows the Run idle: OpenCode's busy stands, and the
// stream goes on to deliver its events unmasked.
func TestOpenCodeObserverStaleRefusalKeepsRecovery(t *testing.T) {
	o := newStepObserver(t, func(b *fakeBus) {
		b.setLoop(true)
		b.setStatusCode(http.StatusForbidden)
	})
	o.step()
	readA := o.nextGot(t)
	if !errors.Is(readA.err, errRefused) {
		t.Fatalf("read A: %v", readA.err)
	}
	o.b.drop <- struct{}{}
	o.backoff(t, "a backoff after the first stream dropped")
	o.b.setStatusCode(0)
	o.step()
	readB := o.nextGot(t)
	if readB.err != nil {
		t.Fatalf("read B: %v", readB.err)
	}
	close(readB.release)
	if err := o.nextRead(t); err != nil {
		t.Fatalf("read B applied: %v", err)
	}
	o.sink.waitLast(t, "busy")
	close(readA.release)
	if err := o.nextRead(t); !errors.Is(err, errRefused) {
		t.Fatalf("read A applied: %v", err)
	}
	o.b.events <- ocStatus("idle")
	o.statusHandled(t)
	o.b.events <- ocStatus("busy")
	o.statusHandled(t)
	o.unstep()
	checkLines(t, o.w, o.sink.inputSink, "idle", "busy", "accepted prompt next_step receipt=false", "turn_end", "idle", "busy", "idle", "busy")
	o.sink.noMoreWarnings(t)
	// The failed one, the first stream and the recovered one.
	if n := o.b.count("GET /event"); n != 3 {
		t.Fatalf("GET /event %d times", n)
	}
}
