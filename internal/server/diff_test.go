package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// diffFixture: tenant t1's running Run r1 with repositories app and lib
// (push: false), cloned at base-app and base-lib, its placement at epoch 2
// running on host h1; tenant t2; keys for both and an operator.
func diffFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	sp := `{"git": {"repositories": [
		{"name": "app", "url": "https://git.example/app.git", "path": "/workspace/repos/app"},
		{"name": "lib", "url": "https://git.example/lib.git", "path": "/workspace/repos/lib", "push": false}]}}`
	execSQL(t, s, ctx, `UPDATE runs SET spec = $1, state = 'running', current_epoch = 2 WHERE id = 'r1'`, sp)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'running')`)
	// lib's base: its latest clone (an earlier one was replaced).
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 1, 'git.clone', '{"repo": "app", "status": "cloned", "commit": "base-app"}'),
		('t1', 'r1', 1, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "old-lib"}'),
		('t1', 'r1', 2, 'git.clone', '{"repo": "lib", "status": "failed"}'),
		('t1', 'r1', 2, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "base-lib"}')`)
	return s, keys
}

func setRun(t *testing.T, s *Server, runState, plState string) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'r1'`, runState)
	execSQL(t, s, ctx, `UPDATE placements SET state = $1 WHERE id = 'p2'`, plState)
}

func getDiff(t *testing.T, s *Server, key, path, accept string) (int, http.Header, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, w.Header(), w.Body.Bytes()
}

func decodeDiff(t *testing.T, b []byte) RunDiff {
	t.Helper()
	var d RunDiff
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return d
}

// fakeConn stands in for h1's runner connection (with the diff
// capability): it answers each diff request with answer's frames, and
// sends each diff.cancel's subId to cancels (if not nil). It returns the
// requests seen so far, when called.
func fakeConn(t *testing.T, s *Server, answer func(proto.DiffRequest) []proto.Frame, cancels chan<- string) func() []proto.DiffRequest {
	t.Helper()
	c := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 16), notify: make(chan struct{}, 1), done: make(chan struct{}), caps: []string{proto.CapDiff}}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = c
	s.hub.mu.Unlock()
	var mu sync.Mutex
	var reqs []proto.DiffRequest
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case f := <-c.send:
				if f.Type == proto.MsgDiffCancel && cancels != nil {
					var end proto.DiffEnd
					json.Unmarshal(f.Data, &end)
					cancels <- end.SubID
				}
				if f.Type != proto.MsgDiffRequest {
					continue
				}
				var req proto.DiffRequest
				json.Unmarshal(f.Data, &req)
				mu.Lock()
				reqs = append(reqs, req)
				mu.Unlock()
				for _, a := range answer(req) {
					s.hub.route(req.SubID, a)
				}
			}
		}
	}()
	return func() []proto.DiffRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reqs)
	}
}

func liveFrames(req proto.DiffRequest) []proto.Frame {
	var out []proto.Frame
	for _, r := range req.Repos {
		res := proto.DiffResult{SubID: req.SubID, Stat: proto.DiffStat{Repo: r.Name, Kind: req.Kind, Base: r.Base, Head: "live-head", Files: 1,
			NormalizedPaths: []string{"crlf.txt"}}}
		if !req.StatOnly {
			res.Patch = []byte("live " + r.Name + "\n")
		}
		if r.Name == "lib" {
			res.Stat.Error, res.Stat.ErrorCode = "gone", proto.DiffBaseUnreachable
		}
		out = append(out, proto.Frame{Type: proto.MsgDiffResult, Data: proto.Marshal(res)})
	}
	return append(out, proto.Frame{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID})})
}

func TestDiffLiveWhileTheContainerRuns(t *testing.T) {
	s, keys := diffFixture(t)
	reqs := fakeConn(t, s, liveFrames, nil)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	d := decodeDiff(t, body)
	if len(d.Repos) != 2 || d.Repos[0].Patch != "live app\n" || d.Repos[0].Head != "live-head" || !d.Repos[0].Push || d.Repos[1].Push ||
		d.Repos[1].ErrorCode != "base_unreachable" || !slices.Equal(d.Repos[0].NormalizedPaths, []string{"crlf.txt"}) {
		t.Fatalf("live: %s", body)
	}
	// The runner was told each repository's path and clone base.
	if r := reqs()[0]; r.Kind != "clone" || len(r.Repos) != 2 || r.Repos[0].Base != "base-app" || r.Repos[1].Base != "base-lib" ||
		r.Repos[1].Path != "/workspace/repos/lib" {
		t.Errorf("request: %+v", r)
	}
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?base=head&stat=true&repo=app", "")
	if r := reqs()[1]; code != http.StatusOK || r.Kind != "head" || !r.StatOnly || len(r.Repos) != 1 || decodeDiff(t, body).Repos[0].Patch != "" {
		t.Errorf("head stat: %d %+v %s", code, r, body)
	}
	// The patches alone; a failed repository is named in a header.
	code, hdr, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "text/x-diff")
	if code != http.StatusOK || string(body) != "live app\nlive lib\n" || hdr.Get("X-Lux-Diff-Errors") != "lib" ||
		!strings.HasPrefix(hdr.Get("Content-Type"), "text/x-diff") {
		t.Errorf("text/x-diff: %d %v %q", code, hdr, body)
	}
	// Stopping: still asked while its container is there.
	setRun(t, s, StateStopping, "stopping")
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusOK {
		t.Errorf("stopping: %d %s", code, body)
	}
	// Scoped like the Run: another tenant, or an unknown repository.
	if code, _, _ := getDiff(t, s, keys["t2"], "/v1/runs/r1/diff", ""); code != http.StatusNotFound {
		t.Errorf("another tenant: %d", code)
	}
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=nope", ""); code != http.StatusNotFound || !strings.Contains(string(body), "not_found") {
		t.Errorf("unknown repo: %d %s", code, body)
	}
	if code, _, _ := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?base=x", ""); code/100 != 4 {
		t.Errorf("bad base: %d", code)
	}
}

// A Run whose container is not running has no diff: 409 run_not_running,
// saying whether it is not up yet or has stopped (and how to keep a
// patch), without asking the host. A container the database thinks is up
// but the runner finds exited is the same answer.
func TestDiffOfARunNotRunning(t *testing.T) {
	s, keys := diffFixture(t)
	reqs := fakeConn(t, s, func(req proto.DiffRequest) []proto.Frame {
		return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, NotRunning: true})}}
	}, nil)
	for _, c := range []struct {
		run, pl string
		want    string
	}{
		{StateStopped, "exited", "the Run is stopped: its diff is available only while the Run is running; resume it, or save a patch at stop with workload.beforeStop (git add -N . && git diff --binary <base> > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts"},
		{StateFailed, "exited", "the Run is failed: its diff is available only while the Run is running; resume it"},
		{StateSucceeded, "exited", "the Run is succeeded: its diff"},
		{StateLost, "lost", "the Run is lost: its diff"},
		{StateScheduled, "assigned", "the Run is scheduled: its container is not up yet; its diff is available once it runs"},
		{StateStarting, "starting", "the Run is starting: its container is not up yet"},
		{StateResuming, "exited", "the Run is resuming: its container is not up yet"},
	} {
		setRun(t, s, c.run, c.pl)
		code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
		var e errorBody
		json.Unmarshal(body, &e)
		if code != http.StatusConflict || e.Error.Code != "run_not_running" || !strings.HasPrefix(e.Error.Message, c.want) {
			t.Errorf("%s/%s: %d %s", c.run, c.pl, code, body)
		}
	}
	if n := len(reqs()); n != 0 {
		t.Errorf("the host was asked %d times for a Run that is not running", n)
	}
	setRun(t, s, StateStopping, "stopping")
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusConflict || !strings.Contains(string(body), "the Run is stopping and its container has exited") ||
		!strings.Contains(string(body), "workload.beforeStop") || len(reqs()) != 1 {
		t.Errorf("exited under a stopping Run: %d %s", code, body)
	}
	// A Run with no repositories: nothing to diff.
	execSQL(t, s, context.Background(), `UPDATE runs SET spec = '{}' WHERE id = 'r1'`)
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusNotFound || !strings.Contains(string(body), "no_diff") {
		t.Errorf("no repositories: %d %s", code, body)
	}
}

// A diff the runner could not compute is 502 with its error.
func TestDiffRunnerError(t *testing.T) {
	s, keys := diffFixture(t)
	fakeConn(t, s, func(req proto.DiffRequest) []proto.Frame {
		return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, Error: "boom"})}}
	}, nil)
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusBadGateway || !strings.Contains(string(body), "boom") {
		t.Errorf("runner error: %d %s", code, body)
	}
}

// A live request whose client goes away tells the runner to cancel it; one
// the runner finds busy (another live diff under way) is 429 diff_busy.
func TestLiveDiffCancelAndBusy(t *testing.T) {
	s, keys := diffFixture(t)
	cancels := make(chan string, 4)
	reqs := fakeConn(t, s, func(req proto.DiffRequest) []proto.Frame {
		if req.Kind == "head" {
			return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, Busy: true})}}
		}
		return nil // never answers: the client gives up
	}, cancels)

	rctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(rctx, http.MethodGet, "/v1/runs/r1/diff", nil)
	req.Header.Set("Authorization", "Bearer "+keys["t1"])
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	for deadline := time.Now().Add(5 * time.Second); len(reqs()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	select {
	case sub := <-cancels:
		if sub != reqs()[0].SubID {
			t.Errorf("cancelled %s, requested %s", sub, reqs()[0].SubID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was not told to cancel")
	}

	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?base=head", "")
	if code != http.StatusTooManyRequests || !strings.Contains(string(body), "diff_busy") {
		t.Errorf("busy: %d %s", code, body)
	}
	// An answered request is not cancelled.
	select {
	case sub := <-cancels:
		t.Errorf("a finished request was cancelled: %s", sub)
	case <-time.After(100 * time.Millisecond):
	}
}

// A runner without the diff capability (an older release) is never sent a
// live diff request: 503 diff_unsupported. The capability is the
// connection's, from the runner's hello.
func TestDiffCapability(t *testing.T) {
	s, keys := diffFixture(t)
	reqs := fakeConn(t, s, liveFrames, nil)
	s.hub.mu.Lock()
	s.hub.conns["h1"].caps = nil
	s.hub.mu.Unlock()
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "diff_unsupported") || len(reqs()) != 0 {
		t.Errorf("old runner: %d %s (%d requests)", code, body, len(reqs()))
	}
	s.hub.mu.Lock()
	s.hub.conns["h1"].caps = []string{proto.CapDiff}
	s.hub.mu.Unlock()
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusOK || len(reqs()) != 1 {
		t.Errorf("new runner: %d %s", code, body)
	}
	// No connection at all.
	s.hub.mu.Lock()
	delete(s.hub.conns, "h1")
	s.hub.mu.Unlock()
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusServiceUnavailable || !strings.Contains(string(body), "host_unreachable") {
		t.Errorf("no connection: %d %s", code, body)
	}
}

// Every assignment carries each repository's clone base, so a resumed
// placement (which does not clone) diffs from where the Run started.
func TestAssignCarriesGitBases(t *testing.T) {
	s, _ := diffFixture(t)
	ctx := context.Background()
	var a proto.Assign
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := s.assign(ctx, tx, pendingRun{ID: "r1", TenantID: "t1", State: StateResuming, Epoch: 2, Spec: spec.RunSpec{}}, &candidateHost{ID: "h1"}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT payload FROM host_messages WHERE host_id = 'h1' AND type = $1`, proto.MsgAssign).Scan(&a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Epoch != 3 || a.GitBases["app"] != "base-app" || a.GitBases["lib"] != "base-lib" || len(a.GitBases) != 2 {
		t.Errorf("assign: epoch %d, bases %v", a.Epoch, a.GitBases)
	}
}
