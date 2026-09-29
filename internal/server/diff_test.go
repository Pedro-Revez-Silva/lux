package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// memS3 is an S3 of one bucket in memory: PUT stores, GET returns.
type memS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		m.objs[r.URL.Path] = b
	case http.MethodGet:
		b, ok := m.objs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	}
}

// diffFixture: tenant t1's Run r1 with repositories app and lib (push:
// false), cloned at base-app and base-lib; tenant t2; keys for both and an
// operator; a host h1; an S3 in memory.
func diffFixture(t *testing.T) (*Server, map[string]string, *memS3) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	mem := &memS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(mem)
	t.Cleanup(srv.Close)
	bs, err := blob.New(ctx, blob.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = bs
	sp := `{"git": {"repositories": [
		{"name": "app", "url": "https://git.example/app.git", "path": "/workspace/repos/app"},
		{"name": "lib", "url": "https://git.example/lib.git", "path": "/workspace/repos/lib", "push": false}]}}`
	execSQL(t, s, ctx, `UPDATE runs SET spec = $1, state = 'stopped', current_epoch = 2 WHERE id = 'r1'`, sp)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r3', 't1', $1, 'stopped')`, sp)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, capabilities) VALUES ('h1', 'h1', 'ready', '{diff}')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'exited')`)
	// lib's base: its latest clone (an earlier one was replaced).
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 1, 'git.clone', '{"repo": "app", "status": "cloned", "commit": "base-app"}'),
		('t1', 'r1', 1, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "old-lib"}'),
		('t1', 'r1', 2, 'git.clone', '{"repo": "lib", "status": "failed"}'),
		('t1', 'r1', 2, 'git.clone', '{"repo": "lib", "status": "cloned", "commit": "base-lib"}')`)
	return s, keys, mem
}

// snapshotWithDiffs reports snapshot id of r1's placement at epoch, as its
// runner would, with one patch per repository and kind, uploaded to the
// memory S3.
func snapshotWithDiffs(t *testing.T, s *Server, mem *memS3, id string, epoch int, patch func(repo, kind string) string) {
	t.Helper()
	ctx := context.Background()
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: id, RunID: "r1", Epoch: epoch, Volumes: []proto.VolumeSnapshot{}}}
	diffs := proto.SnapshotDiffs{SnapshotID: id}
	for _, repo := range []string{"app", "lib"} {
		for _, kind := range []string{proto.DiffBaseClone, proto.DiffBaseHead} {
			p := patch(repo, kind)
			sum := sha256.Sum256([]byte(p))
			d := proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: repo, Kind: kind, Base: "b-" + kind, Head: "h-" + id,
				Files: 1, Insertions: 1, FileStats: []proto.DiffFile{{Path: "f", Insertions: 1}}, PatchBytes: int64(len(p))},
				PatchSHA256: hex.EncodeToString(sum[:])}
			var z bytes.Buffer
			zw, _ := zstd.NewWriter(&z)
			zw.Write([]byte(p))
			zw.Close()
			blobID := ids.New(ids.Blob)
			d.Blob = &proto.BlobInfo{BlobID: blobID, Size: int64(z.Len())}
			mem.mu.Lock()
			mem.objs["/b/"+blob.Key("t1", "r1", blobID)] = z.Bytes()
			mem.mu.Unlock()
			diffs.Diffs = append(diffs.Diffs, d)
		}
	}
	runnerReport(t, s, "r1", epoch, proto.MsgSnapshotDone, sd)
	runnerReport(t, s, "r1", epoch, proto.MsgSnapshotDiffs, diffs)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = 'tenants/t1/runs/r1/' || id WHERE kind = 'diff' AND location = 'host'`)
}

// runnerReport applies a runner report from host h1, as its connection would.
func runnerReport(t *testing.T, s *Server, runID string, epoch int, typ string, v any) {
	t.Helper()
	if err := s.applyReport(context.Background(), "h1", proto.Frame{Type: typ, RunID: runID, Epoch: epoch, Data: proto.Marshal(v)}); err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
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

func TestDiffFromTheLatestSnapshot(t *testing.T) {
	s, keys, mem := diffFixture(t)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), "no_diff") {
		t.Fatalf("before any snapshot: %d %s", code, body)
	}
	snapshotWithDiffs(t, s, mem, "s1", 1, func(repo, kind string) string { return "old " + repo + " " + kind + "\n" })
	snapshotWithDiffs(t, s, mem, "s2", 2, func(repo, kind string) string { return "new " + repo + " " + kind + "\n" })

	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	d := decodeDiff(t, body)
	if d.Base != "clone" || len(d.Repos) != 2 {
		t.Fatalf("%+v", d)
	}
	app, lib := d.Repos[0], d.Repos[1]
	if app.Repo != "app" || app.Source != "snapshot" || app.SnapshotID != "s2" || app.Patch != "new app clone\n" || !app.Push || app.Head != "h-s2" {
		t.Errorf("app: %+v", app)
	}
	if lib.Repo != "lib" || lib.Push || lib.Patch != "new lib clone\n" || lib.At.IsZero() {
		t.Errorf("lib: %+v", lib)
	}
	// One repository, against HEAD.
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=lib&base=head", "")
	if d := decodeDiff(t, body); code != http.StatusOK || len(d.Repos) != 1 || d.Repos[0].Patch != "new lib head\n" {
		t.Errorf("%d %s", code, body)
	}
	// Stats only: no patch.
	_, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?stat=true", "")
	if d := decodeDiff(t, body); d.Repos[0].Patch != "" || d.Repos[0].Files != 1 || len(d.Repos[0].FileStats) != 1 {
		t.Errorf("stat: %s", body)
	}
	// The patches alone.
	code, hdr, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "text/x-diff")
	if code != http.StatusOK || string(body) != "new app clone\nnew lib clone\n" || !strings.HasPrefix(hdr.Get("Content-Type"), "text/x-diff") ||
		hdr.Get("X-Lux-Snapshot-Id") != "s2" {
		t.Errorf("text/x-diff: %d %v %q", code, hdr, body)
	}
	// An unknown repository; a Run without a snapshot with a diff.
	if code, _, _ := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown repo: %d", code)
	}
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r3/diff", ""); code != http.StatusNotFound || !strings.Contains(string(body), "no_diff") {
		t.Errorf("r3: %d %s", code, body)
	}
	// Another tenant's Run is not there; an operator reads it.
	if code, _, _ := getDiff(t, s, keys["t2"], "/v1/runs/r1/diff", ""); code != http.StatusNotFound {
		t.Errorf("t2 read t1's diff: %d", code)
	}
	if code, _, body := getDiff(t, s, keys["op"], "/v1/runs/r1/diff", ""); code != http.StatusOK || decodeDiff(t, body).Repos[0].SnapshotID != "s2" {
		t.Errorf("operator: %d %s", code, body)
	}
	// Row-level security: t2's scope sees none of t1's diff rows, nor may it
	// write one for t1.
	ctx := context.Background()
	err := s.db.Tx(ctx, store.Tenant("t2"), func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM snapshot_diffs`).Scan(&n); err != nil || n != 0 {
			t.Errorf("t2 sees %d diff rows (%v)", n, err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM snapshot_diff_state`).Scan(&n); err != nil || n != 0 {
			t.Errorf("t2 sees %d diff state rows (%v)", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.Tenant("t2"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO snapshot_diffs (tenant_id, run_id, snapshot_id, epoch, repo, kind) VALUES ('t1', 'r1', 's1', 1, 'x', 'clone')`)
		return err
	})
	if err == nil {
		t.Error("t2 wrote a diff row for t1")
	}
	err = s.db.Tx(ctx, store.Tenant("t2"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE snapshot_diff_state SET state = 'failed'`)
		if err == nil {
			var n int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM snapshot_diff_state WHERE state = 'failed'`).Scan(&n)
			if n != 0 {
				t.Errorf("t2 changed %d of t1's diff states", n)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := diffState(t, s, "s2"); st != "complete" {
		t.Errorf("s2's state after t2's update: %q", st)
	}
}

// A patch still on its host: 409, as for an artifact.
func TestDiffNotUploadedYet(t *testing.T) {
	s, keys, mem := diffFixture(t)
	snapshotWithDiffs(t, s, mem, "s1", 1, func(repo, kind string) string { return "p\n" })
	execSQL(t, s, context.Background(), `UPDATE blobs SET location = 'host' WHERE kind = 'diff'`)
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusConflict || !strings.Contains(string(body), "not_uploaded") {
		t.Errorf("%d %s", code, body)
	}
	// Stats need no patch.
	if code, _, _ := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?stat=true", ""); code != http.StatusOK {
		t.Errorf("stat: %d", code)
	}
}

// fakeConn stands in for a runner's WebSocket on host h1: it answers each
// diff request with answer's frames, and sends each diff.cancel's subId to
// cancels (if not nil).
func fakeConn(t *testing.T, s *Server, answer func(proto.DiffRequest) []proto.Frame) func() []proto.DiffRequest {
	return fakeConnCancels(t, s, answer, nil)
}

// fakeConnCancels returns the requests seen so far, when called.
func fakeConnCancels(t *testing.T, s *Server, answer func(proto.DiffRequest) []proto.Frame, cancels chan<- string) func() []proto.DiffRequest {
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
		res := proto.DiffResult{SubID: req.SubID, Stat: proto.DiffStat{Repo: r.Name, Kind: req.Kind, Base: r.Base, Head: "live-head", Files: 1}}
		if !req.StatOnly {
			res.Patch = []byte("live " + r.Name + "\n")
		}
		out = append(out, proto.Frame{Type: proto.MsgDiffResult, Data: proto.Marshal(res)})
	}
	return append(out, proto.Frame{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID})})
}

func TestDiffLiveWhileTheContainerRuns(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 1, func(repo, kind string) string { return "snap " + repo + "\n" })
	reqs := fakeConn(t, s, liveFrames)

	// Stopped: the snapshot, and the host is not asked.
	_, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if d := decodeDiff(t, body); d.Repos[0].Source != "snapshot" || len(reqs()) != 0 {
		t.Fatalf("stopped: %s (%d requests)", body, len(reqs()))
	}

	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	d := decodeDiff(t, body)
	if len(d.Repos) != 2 || d.Repos[0].Source != "live" || d.Repos[0].Patch != "live app\n" || d.Repos[0].SnapshotID != "" || d.Repos[1].Push {
		t.Fatalf("live: %s", body)
	}
	// The runner was told each repository's clone base.
	if r := reqs()[0]; r.Kind != "clone" || len(r.Repos) != 2 || r.Repos[0].Base != "base-app" || r.Repos[1].Base != "base-lib" ||
		r.Repos[1].Path != "/workspace/repos/lib" {
		t.Errorf("request: %+v", r)
	}
	code, hdr, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?base=head&stat=true", "")
	if code != http.StatusOK || reqs()[1].Kind != "head" || !reqs()[1].StatOnly || decodeDiff(t, body).Repos[0].Patch != "" {
		t.Errorf("head stat: %d %v %s", code, hdr, body)
	}
	// Stopping: still the container's while it is there.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'stopping' WHERE id = 'p2'`)
	_, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if decodeDiff(t, body).Repos[0].Source != "live" {
		t.Errorf("stopping: %s", body)
	}
}

// The placement is live in the database but its container has exited
// (snapshotting): the runner says so and the latest snapshot answers.
func TestDiffFallsBackWhenTheContainerHasExited(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 1, func(repo, kind string) string { return "snap " + repo + "\n" })
	fakeConn(t, s, func(req proto.DiffRequest) []proto.Frame {
		return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, NotRunning: true})}}
	})
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'stopping' WHERE id = 'p2'`)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if d := decodeDiff(t, body); code != http.StatusOK || d.Repos[0].Source != "snapshot" || d.Repos[0].Patch != "snap app\n" {
		t.Errorf("%d %s", code, body)
	}
	// A diff the runner could not compute is an error, not a fallback.
	s2, keys2, _ := diffFixture(t)
	fakeConn(t, s2, func(req proto.DiffRequest) []proto.Frame {
		return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, Error: "boom"})}}
	})
	execSQL(t, s2, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	execSQL(t, s2, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	if code, _, body := getDiff(t, s2, keys2["t1"], "/v1/runs/r1/diff", ""); code != http.StatusBadGateway || !strings.Contains(string(body), "boom") {
		t.Errorf("runner error: %d %s", code, body)
	}
}

// What a snapshot's diff says beyond its stat (filters ignored, files git
// converts, dirty submodules, an error's code) is stored and answered.
func TestSnapshotDiffKeepsFiltersAndErrorCode(t *testing.T) {
	s, keys, _ := diffFixture(t)
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDone, proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "s1", RunID: "r1", Epoch: 2, Volumes: []proto.VolumeSnapshot{}}})
	sd := proto.SnapshotDiffs{SnapshotID: "s1"}
	for _, kind := range []string{proto.DiffBaseClone, proto.DiffBaseHead} {
		sd.Diffs = append(sd.Diffs,
			proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: "app", Kind: kind, Files: 1, FiltersIgnored: true, FilteredPaths: []string{"big.psd"},
				NormalizedPaths: []string{"crlf.txt"}, DirtySubmodules: []string{"vendor/lib"},
				FileStats: []proto.DiffFile{{Path: "big.psd", Binary: true}}}},
			proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: "lib", Kind: kind, Error: "gone", ErrorCode: proto.DiffBaseUnreachable}})
	}
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, sd)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?stat=true", "")
	d := decodeDiff(t, body)
	if code != http.StatusOK || !d.Repos[0].FiltersIgnored || len(d.Repos[0].FilteredPaths) != 1 || d.Repos[1].ErrorCode != "base_unreachable" ||
		!slices.Equal(d.Repos[0].NormalizedPaths, []string{"crlf.txt"}) || !slices.Equal(d.Repos[0].DirtySubmodules, []string{"vendor/lib"}) {
		t.Errorf("%d %s", code, body)
	}
}

// snapshot.diffs follows its snapshot.done: accepted from the snapshot's
// own placement even once the Run moved on, once only, never for another
// host's snapshot. Until it arrives the latest snapshot's diff is pending
// (409 diff_pending); a skipped one is 404 diff_unavailable with why, and
// never an older snapshot's diff.
func TestSnapshotDiffsReport(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s0", 1, func(repo, kind string) string { return "older\n" })
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDone, proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "s1", RunID: "r1", Epoch: 2, Volumes: []proto.VolumeSnapshot{}}})
	code, hdr, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusConflict || !strings.Contains(string(body), `"code":"diff_pending"`) || !strings.Contains(string(body), `"retryAfter":5`) ||
		!strings.Contains(string(body), `"snapshotId":"s1"`) || hdr.Get("Retry-After") != "5" {
		t.Errorf("before its diffs: %d %v %s", code, hdr, body)
	}
	// The Run has a newer placement by now: the old one's diffs still count.
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3 WHERE id = 'r1'`)
	skipped := proto.SnapshotDiffs{SnapshotID: "s1", Skipped: "superseded: the Run resumed on this host"}
	for _, repo := range []string{"app", "lib"} {
		for _, k := range []string{"clone", "head"} {
			skipped.Diffs = append(skipped.Diffs, proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: repo, Kind: k, Error: "not computed: " + skipped.Skipped}})
		}
	}
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, skipped)
	// Redelivered: nothing changes, no second event.
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, skipped)
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = 'r1' AND type = 'diff.skipped' AND data->>'snapshotId' = 's1'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Errorf("%d diff.skipped events (%v)", n, err)
	}
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), `"code":"diff_unavailable"`) || !strings.Contains(string(body), `"reason":"superseded`) ||
		!strings.Contains(string(body), `"snapshotId":"s1"`) || strings.Contains(string(body), "older") {
		t.Errorf("skipped: %d %s", code, body)
	}
	// The older one, asked for by id.
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?snapshot=s0", "")
	if d := decodeDiff(t, body); code != http.StatusOK || d.Repos[0].SnapshotID != "s0" || d.Repos[0].Patch != "older\n" {
		t.Errorf("?snapshot=s0: %d %s", code, body)
	}
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?snapshot=nope", ""); code != http.StatusNotFound || !strings.Contains(string(body), "not_found") {
		t.Errorf("?snapshot=nope: %d %s", code, body)
	}
	// A different report for it now: refused.
	other := skipped
	other.Skipped = "something else"
	if err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(other)}); err == nil {
		t.Error("a different report for a finished snapshot diff was accepted")
	}
	// Another host, or a snapshot not the placement's: refused.
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h2', 'h2', 'ready')`)
	err := s.applyReport(ctx, "h2", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(proto.SnapshotDiffs{SnapshotID: "s1"})})
	if err == nil {
		t.Error("another host's snapshot.diffs was accepted")
	}
	err = s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.SnapshotDiffs{SnapshotID: "s1"})})
	if err == nil {
		t.Error("snapshot.diffs from the wrong epoch was accepted")
	}
}

// A live request whose client goes away tells the runner to cancel it; one
// the runner finds busy (another live diff under way) is 429 diff_busy.
func TestLiveDiffCancelAndBusy(t *testing.T) {
	s, keys, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	cancels := make(chan string, 4)
	reqs := fakeConnCancels(t, s, func(req proto.DiffRequest) []proto.Frame {
		if req.Kind == "head" {
			return []proto.Frame{{Type: proto.MsgDiffEnd, Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID, Busy: true})}}
		}
		return nil // never answers: the client gives up
	}, cancels)

	rctx, cancel := context.WithCancel(ctx)
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

// ?repo= reads only that repository's blob: a sibling's missing or
// host-only blob does not touch it. A blob missing from S3 is that
// repository's error, never an empty patch.
func TestDiffRepoFilterAndMissingBlob(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 2, func(repo, kind string) string { return repo + " " + kind + "\n" })
	// lib's clone patch: gone from S3.
	var libKey string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT b.s3_key FROM snapshot_diffs d JOIN blobs b ON b.id = d.blob_id
			WHERE d.snapshot_id = 's1' AND d.repo = 'lib' AND d.kind = 'clone'`).Scan(&libKey)
	}); err != nil {
		t.Fatal(err)
	}
	mem.mu.Lock()
	delete(mem.objs, "/b/"+libKey)
	mem.mu.Unlock()
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=app", "")
	if d := decodeDiff(t, body); code != http.StatusOK || len(d.Repos) != 1 || d.Repos[0].Patch != "app clone\n" {
		t.Errorf("app, lib's blob missing: %d %s", code, body)
	}
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	d := decodeDiff(t, body)
	if code != http.StatusOK || d.Repos[0].Patch != "app clone\n" || d.Repos[1].Error == "" || d.Repos[1].Patch != "" {
		t.Errorf("both, lib's blob missing: %d %s", code, body)
	}
	// lib's blob not uploaded yet: app alone still answers.
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host' WHERE name = 'lib'`)
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=app", ""); code != http.StatusOK {
		t.Errorf("app, lib's blob on its host: %d %s", code, body)
	}
	if code, _, _ := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=lib", ""); code != http.StatusConflict {
		t.Errorf("lib, on its host: %d", code)
	}
	// A non-empty row without a blob at all.
	execSQL(t, s, ctx, `UPDATE snapshot_diffs SET blob_id = NULL WHERE repo = 'lib' AND kind = 'head'`)
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff?repo=lib&base=head", "")
	if d := decodeDiff(t, body); code != http.StatusOK || d.Repos[0].Error == "" {
		t.Errorf("no blob recorded: %d %s", code, body)
	}
}

// storedDiffs counts snapshot snapID's diff rows, and the diff blobs of
// Run runID.
func storedDiffs(t *testing.T, s *Server, snapID, runID string) (rows, blobs int) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM snapshot_diffs WHERE snapshot_id = $1`, snapID).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE run_id = $1 AND kind = 'diff'`, runID).Scan(&blobs)
	}); err != nil {
		t.Fatal(err)
	}
	return rows, blobs
}

func diffBlobIDs(t *testing.T, s *Server, snapID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT repo || '/' || kind, blob_id FROM snapshot_diffs WHERE snapshot_id = $1 AND blob_id IS NOT NULL`, snapID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			out[k] = v
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A report may not point a diff at a blob that is not a new one of its
// own: tenant t2's Run naming tenant t1's blob id is refused whole, and
// nothing of it is stored; so is one Run's snapshot reusing its earlier
// snapshot's blob.
func TestSnapshotDiffsRefuseForeignBlobs(t *testing.T) {
	s, _, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 2, func(repo, kind string) string { return "t1's secret " + repo + "\n" })
	stolen := diffBlobIDs(t, s, "s1")["app/clone"]

	// t2's Run r2, stopped on the same host.
	execSQL(t, s, ctx, `UPDATE runs SET spec = (SELECT spec FROM runs WHERE id = 'r1'), state = 'stopped', current_epoch = 1 WHERE id = 'r2'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p9', 't2', 'r2', 'h1', 1, 'exited')`)
	runnerReport(t, s, "r2", 1, proto.MsgSnapshotDone, proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "s9", RunID: "r2", Epoch: 1, Volumes: []proto.VolumeSnapshot{}}})
	p := "t1's secret app\n"
	sum := sha256.Sum256([]byte(p))
	evil := proto.SnapshotDiffs{SnapshotID: "s9"}
	for _, repo := range []string{"app", "lib"} {
		for _, kind := range []string{"clone", "head"} {
			d := proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: repo, Kind: kind}}
			if repo == "app" && kind == "clone" {
				d.PatchBytes, d.PatchSHA256 = int64(len(p)), hex.EncodeToString(sum[:])
				d.Blob = &proto.BlobInfo{BlobID: stolen, Size: 10, SHA256: "x"}
			}
			evil.Diffs = append(evil.Diffs, d)
		}
	}
	err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r2", Epoch: 1, Data: proto.Marshal(evil)})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("t2's report naming t1's blob: %v", err)
	}
	if rows, blobs := storedDiffs(t, s, "s9", "r2"); rows != 0 || blobs != 0 {
		t.Errorf("stored %d rows, %d blobs for the refused report", rows, blobs)
	}
	var owner string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id || '/' || run_id FROM blobs WHERE id = $1`, stolen).Scan(&owner)
	}); err != nil || owner != "t1/r1" {
		t.Errorf("the blob's owner: %q %v", owner, err)
	}

	// The same Run's next snapshot reusing s1's blob: refused too.
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p3', 't1', 'r1', 'h1', 3, 'exited')`)
	runnerReport(t, s, "r1", 3, proto.MsgSnapshotDone, proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "s3", RunID: "r1", Epoch: 3, Volumes: []proto.VolumeSnapshot{}}})
	evil.SnapshotID = "s3"
	evil.Diffs[0].Blob = &proto.BlobInfo{BlobID: stolen, Size: 10, SHA256: "x"}
	err = s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 3, Data: proto.Marshal(evil)})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("s3 reusing s1's blob: %v", err)
	}
	if rows, _ := storedDiffs(t, s, "s3", "r1"); rows != 0 {
		t.Errorf("stored %d rows for s3", rows)
	}
}

// A stored patch whose bytes are not the ones reported (same length,
// different content) is that repository's error, never served.
func TestDiffBlobSHA256IsVerified(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 2, func(repo, kind string) string { return "real " + repo + "\n" })
	var key string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT b.s3_key FROM snapshot_diffs d JOIN blobs b ON b.id = d.blob_id
			WHERE d.snapshot_id = 's1' AND d.repo = 'app' AND d.kind = 'clone'`).Scan(&key)
	}); err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	zw, _ := zstd.NewWriter(&z)
	zw.Write([]byte("fake app\n"))
	zw.Close()
	mem.mu.Lock()
	mem.objs["/b/"+key] = z.Bytes()
	mem.mu.Unlock()
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	d := decodeDiff(t, body)
	if code != http.StatusOK || d.Repos[0].Patch != "" || !strings.Contains(d.Repos[0].Error, "sha256") || d.Repos[1].Patch != "real lib\n" {
		t.Errorf("%d %s", code, body)
	}
}

// A diff row whose blob is another Run's (written past the report's
// checks) is an error for that repository, never the other Run's bytes.
func TestDiffReadChecksTheBlobsOwner(t *testing.T) {
	s, keys, mem := diffFixture(t)
	ctx := context.Background()
	snapshotWithDiffs(t, s, mem, "s1", 2, func(repo, kind string) string { return "mine " + repo + "\n" })
	// Another Run's blob (r3, the same tenant's: RLS hides another
	// tenant's), with bytes that would pass the row's size and sha256.
	var z bytes.Buffer
	zw, _ := zstd.NewWriter(&z)
	zw.Write([]byte("mine app\n"))
	zw.Close()
	mem.mu.Lock()
	mem.objs["/b/k-other"] = z.Bytes()
	mem.mu.Unlock()
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key) VALUES ('b_other', 't1', 'r3', 1, 'diff', 'x', 's3', 'k-other')`)
	execSQL(t, s, ctx, `UPDATE snapshot_diffs SET blob_id = 'b_other' WHERE snapshot_id = 's1' AND repo = 'app' AND kind = 'clone'`)
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	d := decodeDiff(t, body)
	if code != http.StatusOK || d.Repos[0].Patch != "" || !strings.Contains(d.Repos[0].Error, "not this Run's") || d.Repos[1].Patch != "mine lib\n" {
		t.Errorf("%d %s", code, body)
	}
	// Another tenant's blob: not even visible to the read (RLS).
	execSQL(t, s, ctx, `UPDATE blobs SET tenant_id = 't2', run_id = 'r2' WHERE id = 'b_other'`)
	code, _, body = getDiff(t, s, keys["op"], "/v1/runs/r1/diff", "")
	if d := decodeDiff(t, body); code != http.StatusOK || d.Repos[0].Patch != "" || d.Repos[0].Error == "" {
		t.Errorf("operator, t2's blob: %d %s", code, body)
	}
}

// A runner without the diff capability (an older release) is never sent a
// live diff request: 503 diff_unsupported. luxd's welcome advertises
// snapshot diffs, and a runner's capabilities are stored with its host.
func TestDiffCapabilities(t *testing.T) {
	s, keys, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE id = 'p2'`)
	reqs := fakeConn(t, s, liveFrames)
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

	tok := testHostToken(t, s, ctx)
	for _, caps := range [][]string{{proto.CapDiff}, nil} {
		w, err := s.registerHost(ctx, tok, proto.Hello{Name: "hc", ProtocolVersion: proto.Version, Arch: "arm64", Capabilities: caps})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(w.Capabilities, proto.CapSnapshotDiffs) {
			t.Errorf("welcome: %v", w.Capabilities)
		}
		var got []string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT capabilities FROM hosts WHERE id = $1`, w.HostID).Scan(&got)
		}); err != nil || !slices.Equal(got, nonNil(caps)) {
			t.Errorf("stored %v (%v), want %v", got, err, caps)
		}
	}
}

// diffState is snapshot snapID's diff state and reason ("" if none).
func diffState(t *testing.T, s *Server, snapID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	var state, reason string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, reason FROM snapshot_diff_state WHERE snapshot_id = $1`, snapID).Scan(&state, &reason)
	})
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return state, reason
}

func snapDone(t *testing.T, s *Server, id string, epoch int) {
	t.Helper()
	runnerReport(t, s, "r1", epoch, proto.MsgSnapshotDone, proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: id, RunID: "r1", Epoch: epoch, Volumes: []proto.VolumeSnapshot{}}})
}

// wholeDiffs is a report of empty diffs for each repository, both kinds.
func wholeDiffs(id string, repos ...string) proto.SnapshotDiffs {
	sd := proto.SnapshotDiffs{SnapshotID: id}
	for _, r := range repos {
		for _, k := range []string{"clone", "head"} {
			sd.Diffs = append(sd.Diffs, proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: r, Kind: k}})
		}
	}
	return sd
}

// The state a snapshot's diff goes through: pending, then complete only
// with every expected repository and kind (a partial report is refused,
// nothing stored); a redelivery of the same report is fine, a different
// one refused. A report lost for good: failed (report_lost) after the
// sweep, and a report arriving later still completes it.
func TestSnapshotDiffStates(t *testing.T) {
	s, keys, _ := diffFixture(t)
	ctx := context.Background()
	snapDone(t, s, "s1", 2)
	if st, _ := diffState(t, s, "s1"); st != "pending" {
		t.Fatalf("after snapshot.done: %q", st)
	}
	partial := wholeDiffs("s1", "app")
	err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(partial)})
	if err == nil || !strings.Contains(err.Error(), "2 of the 4") {
		t.Errorf("partial report: %v", err)
	}
	if rows, _ := storedDiffs(t, s, "s1", "r1"); rows != 0 {
		t.Errorf("partial report stored %d rows", rows)
	}
	extra := wholeDiffs("s1", "app", "lib", "other")
	if err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(extra)}); err == nil {
		t.Error("a report with an unexpected repository was accepted")
	}
	if st, _ := diffState(t, s, "s1"); st != "pending" {
		t.Errorf("after refused reports: %q", st)
	}
	// Lost: failed after the sweep.
	execSQL(t, s, ctx, `UPDATE snapshot_diff_state SET updated_at = now() - interval '16 minutes' WHERE snapshot_id = 's1'`)
	if err := s.reapLostDiffReports(ctx); err != nil {
		t.Fatal(err)
	}
	if st, why := diffState(t, s, "s1"); st != "failed" || why != "report_lost" {
		t.Errorf("after the sweep: %q %q", st, why)
	}
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), `"reason":"report_lost"`) {
		t.Errorf("lost: %d %s", code, body)
	}
	// Complete: identical redelivery accepted, a different one refused.
	whole := wholeDiffs("s1", "app", "lib")
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, whole)
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, whole)
	if st, _ := diffState(t, s, "s1"); st != "complete" {
		t.Errorf("after the whole report: %q", st)
	}
	changed := wholeDiffs("s1", "app", "lib")
	changed.Diffs[0].Files = 3
	if err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(changed)}); err == nil {
		t.Error("a different report for a complete diff was accepted")
	}
	if code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", ""); code != http.StatusOK {
		t.Errorf("complete: %d %s", code, body)
	}
	// Its patches lost before their upload: failed.
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, proto.SnapshotDiffs{SnapshotID: "s1", Lost: "a patch file is missing"})
	if st, why := diffState(t, s, "s1"); st != "failed" || !strings.Contains(why, "missing") {
		t.Errorf("lost patches: %q %q", st, why)
	}
}

// A snapshot from a runner that does not compute diffs: unsupported, 404
// diff_unavailable with why. A Run without repositories expects none.
func TestSnapshotDiffUnsupported(t *testing.T) {
	s, keys, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
	snapDone(t, s, "s1", 2)
	if st, _ := diffState(t, s, "s1"); st != "unsupported" {
		t.Errorf("%q", st)
	}
	code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), "diff_unavailable") || !strings.Contains(string(body), "does not compute diffs") {
		t.Errorf("%d %s", code, body)
	}
	// A snapshot from before diffs (no state at all): the same.
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id) VALUES ('s9', 't1', 'r1', 'p2', 3, '{}', 'h1')`)
	code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), "diff_unavailable") || !strings.Contains(string(body), `"snapshotId":"s9"`) {
		t.Errorf("no state: %d %s", code, body)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{diff}' WHERE id = 'h1'`)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{}' WHERE id = 'r1'`)
	snapDone(t, s, "s2", 2)
	if st, _ := diffState(t, s, "s2"); st != "" {
		t.Errorf("no repositories: %q", st)
	}
}

// The latest snapshot is the highest epoch's, whatever order the reports
// arrive in: an older placement's late snapshot.done (and diffs) never
// answer for the newer one.
func TestLateOlderSnapshotIsNotTheLatest(t *testing.T) {
	for _, order := range []string{"late", "in order"} {
		t.Run(order, func(t *testing.T) {
			s, keys, _ := diffFixture(t)
			if order == "late" {
				snapDone(t, s, "s2", 2)
				snapDone(t, s, "s1", 1)
			} else {
				snapDone(t, s, "s1", 1)
				snapDone(t, s, "s2", 2)
			}
			runnerReport(t, s, "r1", 1, proto.MsgSnapshotDiffs, wholeDiffs("s1", "app", "lib"))
			code, _, body := getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
			if code != http.StatusConflict || !strings.Contains(string(body), `"snapshotId":"s2"`) {
				t.Errorf("%d %s", code, body)
			}
			runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, wholeDiffs("s2", "app", "lib"))
			code, _, body = getDiff(t, s, keys["t1"], "/v1/runs/r1/diff", "")
			if d := decodeDiff(t, body); code != http.StatusOK || d.Repos[0].SnapshotID != "s2" {
				t.Errorf("%d %s", code, body)
			}
		})
	}
}

// A repository added on resume is expected from the snapshots of the
// placements that cloned it, not before; one whose clone failed is not.
func TestExpectedReposFollowResumeAdditions(t *testing.T) {
	s, _, _ := diffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec = jsonb_set(spec, '{git,repositories}', spec->'git'->'repositories' ||
		'[{"name": "added", "url": "https://git.example/a.git", "path": "/workspace/repos/added", "addedBy": "req1"},
		  {"name": "broken", "url": "https://git.example/b.git", "path": "/workspace/repos/broken", "addedBy": "req1"}]') WHERE id = 'r1'`)
	snapDone(t, s, "s1", 1)
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
		('t1', 'r1', 2, 'git.clone', '{"repo": "added", "status": "cloned", "commit": "c"}')`)
	snapDone(t, s, "s2", 2)
	for snap, want := range map[string][]string{"s1": {"app", "lib"}, "s2": {"app", "lib", "added"}} {
		var got []string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT repos FROM snapshot_diff_state WHERE snapshot_id = $1`, snap).Scan(&got)
		}); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s expects %v (%v), want %v", snap, got, err, want)
		}
	}
	if err := s.applyReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: "r1", Epoch: 2, Data: proto.Marshal(wholeDiffs("s2", "app", "lib"))}); err == nil {
		t.Error("s2's report without the added repository was accepted")
	}
	runnerReport(t, s, "r1", 2, proto.MsgSnapshotDiffs, wholeDiffs("s2", "app", "lib", "added"))
}
