package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES ('s1', 't1', 'r1', 'p1', 1, '{}')`)
	// lib's base: its latest clone (an earlier one was replaced).
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 1, 'git.clone', '{"repo": "app", "status": "cloned", "commit": "base-app"}'),
		('t1', 'r1', 1, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "old-lib"}'),
		('t1', 'r1', 2, 'state', '{"state":"scheduled","snapshotId":"s1"}'),
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
	_, reqs := fakeConnWith(t, s, []string{proto.CapDiff}, answer, cancels)
	return reqs
}

// fakeConnWith is fakeConn with the given capabilities; it replaces h1's
// connection, and returns the new one too.
func fakeConnWith(t *testing.T, s *Server, caps []string, answer func(proto.DiffRequest) []proto.Frame, cancels chan<- string) (*runnerConn, func() []proto.DiffRequest) {
	t.Helper()
	c := newRunnerConn("h1", nil, caps)
	s.hub.install(c)
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
					s.hub.route(c, req.SubID, a)
				}
			}
		}
	}()
	return c, func() []proto.DiffRequest {
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

// Every assignment carries the bases known from the restored snapshot's
// lineage. A resumed placement that does not clone retains those bases.
func TestAssignCarriesGitBases(t *testing.T) {
	s, _ := diffFixture(t)
	ctx := context.Background()
	var a proto.Assign
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES ('s2', 't1', 'r1', 'p2', 2, '{}')`); err != nil {
			return err
		}
		sid := "s2"
		if err := s.assign(ctx, tx, pendingRun{ID: "r1", TenantID: "t1", State: StateResuming, Epoch: 2, Spec: spec.RunSpec{}, SnapshotID: &sid}, &candidateHost{ID: "h1"}); err != nil {
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

// A Run resumed from an older snapshot diffs from the clones that
// snapshot descends from, not from a later placement's. Epoch 1 cloned app
// at c1 and snapshotted s1; epoch 2, from s1, recloned app (its checkout
// replaced) at c2 and added lib at l2, then snapshotted s2; epoch 3 is
// resumed --from s1: app's base is c1 again, and lib (not in s1) has none
// from the Run's history. Epoch 4, from s3 (epoch 3's), keeps c1 and the
// lib it cloned in epoch 3.
func TestGitBasesFollowTheRestoredSnapshot(t *testing.T) {
	s, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('rs', 't1', '{}', 'stopped', 4)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('q1', 't1', 'rs', 'h1', 1, 'exited'), ('q2', 't1', 'rs', 'h1', 2, 'exited'), ('q3', 't1', 'rs', 'h1', 3, 'exited'), ('q4', 't1', 'rs', 'h1', 4, 'exited')`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES
		('rs-s1', 't1', 'rs', 'q1', 1, '{}'), ('rs-s2', 't1', 'rs', 'q2', 2, '{}'), ('rs-s3', 't1', 'rs', 'q3', 3, '{}')`)
	schedule := func(epoch int, snap any) {
		execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES ('t1', 'rs', $1, 'state', jsonb_build_object('state', 'scheduled', 'snapshotId', $2::text))`, epoch, snap)
	}
	clone := func(epoch int, repo, commit string) {
		execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES ('t1', 'rs', $1, 'git.clone', jsonb_build_object('repo', $2::text, 'status', 'cloned', 'commit', $3::text))`, epoch, repo, commit)
	}
	schedule(1, nil)
	clone(1, "app", "c1")
	schedule(2, "rs-s1")
	clone(2, "app", "c2")
	clone(2, "lib", "l2")
	schedule(3, "rs-s1")
	clone(3, "lib", "l3")
	bases := func(epoch int) map[string]string {
		var m map[string]string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) { m, err = gitBases(ctx, tx, "rs", epoch); return }); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		epoch int
		want  map[string]string
	}{
		{1, map[string]string{"app": "c1"}},
		{2, map[string]string{"app": "c2", "lib": "l2"}},
		{3, map[string]string{"app": "c1", "lib": "l3"}},
	} {
		if got := bases(c.epoch); !maps.Equal(got, c.want) {
			t.Errorf("epoch %d: %v, want %v", c.epoch, got, c.want)
		}
	}
	schedule(4, "rs-s3")
	if got := bases(4); !maps.Equal(got, map[string]string{"app": "c1", "lib": "l3"}) {
		t.Errorf("epoch 4 from s3: %v", got)
	}
	// The scheduler records the lineage it assigns: epoch 5's assignment,
	// resumed from s1 again, has app at c1 and no lib.
	var a proto.Assign
	sid := "rs-s1"
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := s.assign(ctx, tx, pendingRun{ID: "rs", TenantID: "t1", State: StateResuming, Epoch: 4, SnapshotID: &sid}, &candidateHost{ID: "h1"}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT payload FROM host_messages WHERE host_id = 'h1' AND type = $1`, proto.MsgAssign).Scan(&a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Epoch != 5 || !maps.Equal(a.GitBases, map[string]string{"app": "c1"}) {
		t.Errorf("assign from s1: epoch %d, bases %v", a.Epoch, a.GitBases)
	}
}

// A legacy scheduled event without snapshotId cannot identify its source
// snapshot. After an older restore, an intervening placement's clones may
// be unrelated; only clones in the queried epoch have a known base.
func TestGitBasesLegacyScheduledLineage(t *testing.T) {
	s, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('legacy', 't1', '{}', 'running', 6)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('l1', 't1', 'legacy', 'h1', 1, 'exited'), ('l2', 't1', 'legacy', 'h1', 2, 'exited'),
		('l3', 't1', 'legacy', 'h1', 3, 'exited'), ('l4', 't1', 'legacy', 'h1', 4, 'exited'),
		('l5', 't1', 'legacy', 'h1', 5, 'exited'), ('l6', 't1', 'legacy', 'h1', 6, 'running')`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES ('legacy-s1', 't1', 'legacy', 'l1', 1, '{}')`)
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'legacy', 1, 'state', '{"state":"scheduled","snapshotId":null}'),
		('t1', 'legacy', 1, 'git.clone', '{"repo":"app","status":"cloned","commit":"root"}'),
		('t1', 'legacy', 2, 'state', '{"state":"scheduled","snapshotId":"legacy-s1"}'),
		('t1', 'legacy', 2, 'git.clone', '{"repo":"app","status":"cloned","commit":"discarded"}'),
		('t1', 'legacy', 3, 'state', '{"state":"scheduled","snapshotId":"legacy-s1"}'),
		('t1', 'legacy', 4, 'git.clone', '{"repo":"lib","status":"cloned","commit":"unrelated"}'),
		('t1', 'legacy', 5, 'state', '{"state":"scheduled"}'),
		('t1', 'legacy', 5, 'git.clone', '{"repo":"app","status":"cloned","commit":"local"}')`)
	bases := func(epoch int) map[string]string {
		var m map[string]string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) {
			m, err = gitBases(ctx, tx, "legacy", epoch)
			return
		}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		epoch int
		want  map[string]string
	}{
		{3, map[string]string{"app": "root"}},      // explicit older restore skips epoch 2
		{4, map[string]string{"lib": "unrelated"}}, // no scheduled event
		{5, map[string]string{"app": "local"}},     // missing snapshotId
		{6, nil},                                   // no scheduled event and no clones
	} {
		if got := bases(c.epoch); !maps.Equal(got, c.want) {
			t.Errorf("legacy epoch %d: %v, want %v", c.epoch, got, c.want)
		}
	}
	// An explicit empty start also prevents recovery of earlier clones.
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'legacy', 7, 'state', '{"state":"scheduled","snapshotId":null}')`)
	if got := bases(7); got != nil {
		t.Errorf("explicit empty start: %v", got)
	}
}

// A request is on one connection throughout. The host's runner
// reconnecting (without diffs, here) before the request could be sent:
// 503 runner_reconnected, nothing sent to the new connection.
func TestDiffConnectionReplacedBeforeTheRequestIsSent(t *testing.T) {
	s, keys := diffFixture(t)
	// A capable connection whose writer is stuck: the request waits to go.
	old := newRunnerConn("h1", nil, []string{proto.CapDiff})
	old.send = make(chan proto.Frame)
	s.hub.install(old)
	done := make(chan struct{})
	var code int
	var body []byte
	go func() { defer close(done); code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "") }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	_, reqs := fakeConnWith(t, s, nil, liveFrames, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request still waits after its connection was replaced")
	}
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "runner_reconnected") || time.Since(start) > 2*time.Second {
		t.Errorf("replaced before send: %d %s after %v", code, body, time.Since(start))
	}
	if len(reqs()) != 0 {
		t.Errorf("the new connection, without diffs, was sent %d requests", len(reqs()))
	}
}

// A connection replaced while its diff is pending fails the request at
// once (not at the deadline), and the cancel goes to the connection the
// request went to, never the new one.
func TestDiffConnectionReplacedWhileTheDiffIsPending(t *testing.T) {
	s, keys := diffFixture(t)
	oldCancels := make(chan string, 4)
	_, oldReqs := fakeConnWith(t, s, []string{proto.CapDiff}, func(proto.DiffRequest) []proto.Frame { return nil }, oldCancels)
	done := make(chan struct{})
	var code int
	var body []byte
	go func() { defer close(done); code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "") }()
	for deadline := time.Now().Add(5 * time.Second); len(oldReqs()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	newCancels := make(chan string, 4)
	start := time.Now()
	_, newReqs := fakeConnWith(t, s, []string{proto.CapDiff}, liveFrames, newCancels)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request still waits after its connection was replaced")
	}
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "runner_reconnected") || time.Since(start) > 2*time.Second {
		t.Errorf("replaced while pending: %d %s after %v", code, body, time.Since(start))
	}
	select {
	case sub := <-oldCancels:
		if sub != oldReqs()[0].SubID {
			t.Errorf("cancelled %s, requested %s", sub, oldReqs()[0].SubID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the original connection was not told to cancel")
	}
	select {
	case sub := <-newCancels:
		t.Errorf("the new connection was sent a cancel for %s", sub)
	case <-time.After(100 * time.Millisecond):
	}
	if len(newReqs()) != 0 {
		t.Errorf("the new connection was sent %d requests", len(newReqs()))
	}
}

// Frames for a subscription on one connection are taken from it alone.
func TestSubscriptionTakesFramesFromItsConnectionOnly(t *testing.T) {
	s, _ := diffFixture(t)
	a, b := newRunnerConn("h1", nil, nil), newRunnerConn("h1", nil, nil)
	sub, cancel := s.hub.subscribeOn(a, "sub", 0)
	defer cancel()
	s.hub.route(b, "sub", proto.Frame{Type: "from-b"})
	s.hub.route(a, "sub", proto.Frame{Type: "from-a"})
	if f := <-sub.ch; f.Type != "from-a" {
		t.Errorf("got %s", f.Type)
	}
}

// Live diffs in flight are limited: per Run, only identical ones, at most
// maxDiffSharers; in all, maxLiveDiffs.
func TestLiveDiffLimits(t *testing.T) {
	var l diffLimiter
	var releases []func()
	for range maxDiffSharers {
		rel, err := l.acquire("r1", "clone")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	if _, err := l.acquire("r1", "clone"); err == nil || err.(*HTTPError).Code != "diff_busy" {
		t.Errorf("a sharer too many: %v", err)
	}
	if _, err := l.acquire("r1", "head"); err == nil || err.(*HTTPError).Code != "diff_busy" {
		t.Errorf("a different diff of the Run: %v", err)
	}
	for i := range maxLiveDiffs - maxDiffSharers {
		rel, err := l.acquire(fmt.Sprint("run", i), "clone")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	if _, err := l.acquire("another", "clone"); err == nil || err.(*HTTPError).Status != http.StatusTooManyRequests {
		t.Errorf("over the luxd's limit: %v", err)
	}
	releases[0]()
	releases[0]() // idempotent
	if rel, err := l.acquire("another", "clone"); err != nil {
		t.Errorf("after a release: %v", err)
	} else {
		rel()
	}
	for _, rel := range releases[1:] {
		rel()
	}
	if l.total != 0 || len(l.runs) != 0 {
		t.Errorf("left: %d, %v", l.total, l.runs)
	}
}

// Several identical callers: each holds at most the diff's budget of
// results however much the runner sends (lib's 15 MiB is past app's 20
// MiB: stats only), one more is refused, and nothing is retained after.
func TestConcurrentIdenticalDiffsAreBounded(t *testing.T) {
	s, keys := diffFixture(t)
	c, reqs := fakeConnWith(t, s, []string{proto.CapDiff}, func(proto.DiffRequest) []proto.Frame { return nil }, nil)
	type resp struct {
		code int
		body []byte
	}
	out := make(chan resp, maxDiffSharers)
	for range maxDiffSharers {
		go func() {
			code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
			out <- resp{code, body}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); len(reqs()) < maxDiffSharers && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusTooManyRequests || !strings.Contains(string(body), "diff_busy") {
		t.Errorf("one caller too many: %d %s", code, body)
	}
	big := func(repo string, n int) proto.DiffStat {
		return proto.DiffStat{Repo: repo, Kind: "clone", Files: 1, Insertions: 7, FileStats: []proto.DiffFile{{Path: "f", Insertions: 7}}, PatchBytes: int64(n)}
	}
	app := proto.DiffResult{Stat: big("app", 20<<20), Patch: bytes.Repeat([]byte("a"), 20<<20)}
	lib := proto.DiffResult{Stat: big("lib", 15<<20), Patch: bytes.Repeat([]byte("l"), 15<<20)}
	appF, libF := proto.Frame{Type: proto.MsgDiffResult, Data: proto.Marshal(app)}, proto.Frame{Type: proto.MsgDiffResult, Data: proto.Marshal(lib)}
	for _, r := range reqs() {
		s.hub.route(c, r.SubID, appF)
		s.hub.route(c, r.SubID, libF)
	}
	var peak int64
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if peak = s.diffs.retained.Load(); peak >= maxDiffSharers*(20<<20) {
			break
		}
	}
	if peak < maxDiffSharers*(20<<20) || peak > maxDiffSharers*proto.DiffBudget {
		t.Errorf("retained %d bytes for %d callers (budget %d each)", peak, maxDiffSharers, proto.DiffBudget)
	}
	for _, r := range reqs() {
		s.hub.route(c, r.SubID, proto.Frame{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: r.SubID})})
	}
	for range maxDiffSharers {
		r := <-out
		if r.code != http.StatusOK {
			t.Fatalf("%d %.200s", r.code, r.body)
		}
		d := decodeDiff(t, r.body)
		if len(d.Repos[0].Patch) != 20<<20 || d.Repos[0].Truncated {
			t.Errorf("app: %d bytes", len(d.Repos[0].Patch))
		}
		if l := d.Repos[1]; !l.Truncated || l.Error != proto.DiffBudgetExceeded || l.Patch != "" || l.Files != 1 || l.Insertions != 7 || len(l.FileStats) != 0 {
			t.Errorf("lib: %+v", l)
		}
	}
	if n := s.diffs.retained.Load(); n != 0 {
		t.Errorf("%d bytes still retained", n)
	}
}

// A subscription whose reader falls behind by more than its byte bound
// is dropped (its request fails), not queued without bound.
func TestSubscriptionQueueIsByteBounded(t *testing.T) {
	s, _ := diffFixture(t)
	c := newRunnerConn("h1", nil, nil)
	sub, cancel := s.hub.subscribeOn(c, "sub", 100)
	defer cancel()
	f := proto.Frame{Type: proto.MsgDiffResult, Data: make([]byte, 60)}
	s.hub.route(c, "sub", f)
	if got := <-sub.ch; len(got.Data) != 60 {
		t.Fatal("first frame")
	}
	sub.took(f)
	s.hub.route(c, "sub", f) // 60 queued
	s.hub.route(c, "sub", f) // 120: over
	if _, ok := <-sub.ch; !ok {
		t.Fatal("the frame within the bound was lost")
	}
	if _, ok := <-sub.ch; ok {
		t.Error("a frame past the bound was queued")
	}
}
