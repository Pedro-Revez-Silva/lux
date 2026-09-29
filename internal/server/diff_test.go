package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// diffFixture: tenant t1's running Run r1 with repository app, its
// placement at epoch 2 running on host h1, restored from epoch 1's
// snapshot, where app was cloned; tenant t2's r2; read keys for both.
func diffFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"git": {"repositories": [{"name": "app", "url": "https://git.example/app.git", "path": "/workspace/repos/app"}]}}',
		current_epoch = 2 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'running')`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES ('s1', 't1', 'r1', 'p1', 1, '{}')`)
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 1, 'state', '{"state":"scheduled","snapshotId":null}'),
		('t1', 'r1', 1, 'git.clone', '{"repo": "app", "status": "cloned", "commit": "base-app"}'),
		('t1', 'r1', 2, 'state', '{"state":"scheduled","snapshotId":"s1"}')`)
	return s, keys
}

// fakeDiffRunner stands in for h1's runner connection with caps: it
// answers each diff.request with answer's result and counts requests.
func fakeDiffRunner(t *testing.T, s *Server, caps []string, answer func(proto.DiffRequest) proto.DiffResult) *int {
	t.Helper()
	c := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 16), notify: make(chan struct{}, 1), done: make(chan struct{}), caps: caps}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = c
	s.hub.mu.Unlock()
	t.Cleanup(func() { close(c.done) })
	n := new(int)
	go func() {
		for {
			select {
			case <-c.done:
				return
			case f := <-c.send:
				var req proto.DiffRequest
				_ = json.Unmarshal(f.Data, &req)
				*n++
				res := answer(req)
				res.SubID = req.SubID
				s.hub.route(req.SubID, proto.Frame{Type: proto.MsgDiffResult, Data: proto.Marshal(res)})
			}
		}
	}()
	return n
}

func getDiff(t *testing.T, s *Server, key, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestDiffStatusCodes(t *testing.T) {
	s, keys := diffFixture(t)
	ok := func(req proto.DiffRequest) proto.DiffResult {
		return proto.DiffResult{Repos: []proto.RepoDiff{{Repo: "app", Base: "base-app", Head: "h", Patch: []byte("+x\n"), Files: 1, Insertions: 1}}}
	}
	fakeDiffRunner(t, s, []string{proto.CapDiff}, ok)

	code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff")
	var d RunDiff
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("%d %s", code, body)
	}
	if d.Base != "clone" || len(d.Repos) != 1 || d.Repos[0].Patch != "+x\n" || d.Repos[0].Files != 1 {
		t.Fatalf("%+v", d)
	}
	if code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?stat=true&base=head"); code != http.StatusOK ||
		strings.Contains(body, `"patch"`) || !strings.Contains(body, `"base":"head"`) {
		t.Fatalf("stat: %d %s", code, body)
	}
	if code, _ := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?base=nope"); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad base: %d", code)
	}

	// Another tenant's Run is not found; its own has no repositories.
	if code, body := getDiff(t, s, keys["t2"], "/v1/runs/r1/diff"); code != http.StatusNotFound || strings.Contains(body, "no_diff") {
		t.Fatalf("t2 on r1: %d %s", code, body)
	}
	if code, body := getDiff(t, s, keys["t2"], "/v1/runs/r2/diff"); code != http.StatusNotFound || !strings.Contains(body, "the Run has no repositories") {
		t.Fatalf("no repositories: %d %s", code, body)
	}

	// Not running: stopped, or not up yet.
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = 'r1'`)
	execSQL(t, s, context.Background(), `UPDATE placements SET state = 'exited' WHERE id = 'p2'`)
	if code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff"); code != http.StatusConflict ||
		!strings.Contains(body, "the Run is stopped: its diff is available only while the Run is running; resume it, or save a patch") {
		t.Fatalf("stopped: %d %s", code, body)
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'starting' WHERE id = 'r1'`)
	execSQL(t, s, context.Background(), `UPDATE placements SET state = 'starting' WHERE id = 'p2'`)
	if code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff"); code != http.StatusConflict || !strings.Contains(body, "not up yet") {
		t.Fatalf("starting: %d %s", code, body)
	}
}

func TestDiffRunnerAnswers(t *testing.T) {
	for _, c := range []struct {
		name string
		caps []string
		res  proto.DiffResult
		code int
		want string
	}{
		{"busy", []string{proto.CapDiff}, proto.DiffResult{Busy: true}, http.StatusConflict, "diff_busy"},
		{"exited", []string{proto.CapDiff}, proto.DiffResult{NotRunning: true}, http.StatusConflict, "its container has exited"},
		{"failed", []string{proto.CapDiff}, proto.DiffResult{Error: "too big"}, http.StatusBadGateway, "too big"},
		{"old runner", nil, proto.DiffResult{}, http.StatusServiceUnavailable, "diff_unsupported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, keys := diffFixture(t)
			n := fakeDiffRunner(t, s, c.caps, func(proto.DiffRequest) proto.DiffResult { return c.res })
			start := time.Now()
			code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff")
			if code != c.code || !strings.Contains(body, c.want) || time.Since(start) > 5*time.Second {
				t.Fatalf("%d %s after %s", code, body, time.Since(start))
			}
			if c.caps == nil && *n != 0 {
				t.Fatal("an old runner was sent a diff request")
			}
		})
	}
}

// A patch that is not UTF-8 travels as base64.
func TestDiffNonUTF8Patch(t *testing.T) {
	s, keys := diffFixture(t)
	fakeDiffRunner(t, s, []string{proto.CapDiff}, func(proto.DiffRequest) proto.DiffResult {
		return proto.DiffResult{Repos: []proto.RepoDiff{{Repo: "app", Patch: []byte("+caf\xe9\n")}}}
	})
	code, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff")
	var d RunDiff
	_ = json.Unmarshal([]byte(body), &d)
	if code != http.StatusOK || d.Repos[0].Patch != "" || string(d.Repos[0].PatchBase64) != "+caf\xe9\n" {
		t.Fatalf("%d %s", code, body)
	}
}

// Clone bases follow the snapshot a placement restored; a placement with
// no recorded lineage has only its own clones.
func TestGitBases(t *testing.T) {
	s, _ := diffFixture(t)
	ctx := context.Background()
	bases := func(epoch int) map[string]string {
		var m map[string]string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) {
			m, err = gitBases(ctx, tx, "r1", epoch)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := bases(2); m["app"] != "base-app" || len(m) != 1 {
		t.Fatalf("epoch 2: %v", m)
	}
	// Epoch 3 re-clones app, and restores epoch 1's snapshot (not 2's).
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 2, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "lib-2"}'),
		('t1', 'r1', 3, 'state', '{"state":"scheduled","snapshotId":"s1"}'),
		('t1', 'r1', 3, 'git.clone', '{"repo": "app", "status": "failed"}')`)
	if m := bases(3); m["app"] != "base-app" || m["lib"] != "" {
		t.Fatalf("epoch 3: %v", m)
	}
	// Epoch 4 was scheduled before lineage was recorded: nothing from
	// earlier placements.
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 4, 'state', '{"state":"scheduled"}')`)
	if m := bases(4); m != nil {
		t.Fatalf("epoch 4: %v", m)
	}
}
