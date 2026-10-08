package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// resizeBase is what the resize tests submit; submit adds pids 1024.
var resizeBase = spec.Resources{CPUs: 2, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(20 * gib)}

// withPids is r as submit stores it: with the default pids.
func withPids(r spec.Resources) spec.Resources {
	r.Pids = 1024
	return r
}

// submitOnHA submits a Run of tenant t1 with res and a state volume to pool
// "default", which has the ready host "ha". Returns its id.
func submitOnHA(t *testing.T, s *Server, res spec.Resources) string {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	namedPools(t, s, "default")
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Volumes:   []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}},
		Resources: res,
		Placement: spec.Placement{Pool: "default"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', 'default', 'ready', '{"cpus":4,"memory":8589934592}', now()) ON CONFLICT DO NOTHING`)
	return out.Body.ID
}

// stoppedRun submits a Run with res (submitOnHA), then stops it as a
// placement on host "ha" that reported a snapshot would: placement 1
// exited with peakDisk (nil: no usage recorded), the snapshot on ha,
// uploaded. Returns its id.
func stoppedRun(t *testing.T, s *Server, res spec.Resources, peakDisk *int64) string {
	t.Helper()
	ctx := context.Background()
	id := submitOnHA(t, s, res)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, peak_disk_bytes)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'exited', '{}', $2)`, id, peakDisk)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap-'||$1, 't1', $1, 'p-'||$1, 1, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', current_epoch = 1, snapshot_id = 'snap-'||id, pool_id = 'default' WHERE id = $1`, id)
	return id
}

func resumeWith(s *Server, id string, r *resumeResources) (*resumeOutput, error) {
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Resources: r}})
}

// storedResources is the Run's spec.resources in the database.
func storedResources(t *testing.T, s *Server, id string) spec.Resources {
	t.Helper()
	var r spec.Resources
	systemScan(t, s, `SELECT spec->'resources' FROM runs WHERE id = $1`, []any{id}, &r)
	return r
}

func storedState(t *testing.T, s *Server, id string) string {
	t.Helper()
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{id}, &state)
	return state
}

// refused fails t unless err is an HTTPError with status and, when code is
// not empty, that code.
func refused(t *testing.T, err error, status int, code, what string) *HTTPError {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != status || (code != "" && he.Code != code) {
		t.Fatalf("%s: %v, want %d %s", what, err, status, code)
	}
	return he
}

// resumeEvent is the data of the Run's latest resume.requested event.
func resumeEvent(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	var raw []byte
	systemScan(t, s, `SELECT data FROM run_events WHERE run_id = $1 AND type = 'resume.requested' ORDER BY id DESC LIMIT 1`, []any{id}, &raw)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func f64(v float64) *float64       { return &v }
func bytesPtr(v int64) *spec.Bytes { b := spec.Bytes(v); return &b }
func i64(v int64) *int64           { return &v }
func eventRes(m map[string]any, k string) map[string]any {
	res, _ := m["resources"].(map[string]any)
	sub, _ := res[k].(map[string]any)
	return sub
}

// cpus and memory change both ways: the stored spec, GET's view, the
// resume's answer and its resume.requested event all carry the new values;
// disk and pids stay as they were.
func TestResumeResizesCPUsAndMemory(t *testing.T) {
	for _, c := range []struct {
		name   string
		cpus   float64
		memory int64
	}{
		{"up", 4, 16 * gib},
		{"down", 0.5, gib / 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, i64(gib))
			out, err := resumeWith(s, id, &resumeResources{CPUs: f64(c.cpus), Memory: bytesPtr(c.memory)})
			if err != nil {
				t.Fatal(err)
			}
			want := spec.Resources{CPUs: c.cpus, Memory: spec.Bytes(c.memory), Disk: spec.Bytes(20 * gib), Pids: 1024}
			if got := storedResources(t, s, id); got != want {
				t.Errorf("stored %+v, want %+v", got, want)
			}
			got, err := s.getRun(tenantCtx("t1"), &RunPath{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if got.Body.Spec.Resources != want || got.Body.State != StateResuming {
				t.Errorf("GET: %s %+v, want resuming %+v", got.Body.State, got.Body.Spec.Resources, want)
			}
			if rz := out.Body.Resize; rz == nil || rz.Applied != want || rz.Disk != nil ||
				rz.Requested != (spec.Resources{CPUs: c.cpus, Memory: spec.Bytes(c.memory)}) {
				t.Errorf("answer's resize %+v", rz)
			}
			ev := resumeEvent(t, s, id)
			req, app := eventRes(ev, "requested"), eventRes(ev, "applied")
			if req["cpus"] != c.cpus || req["memory"] != float64(c.memory) || req["disk"] != nil {
				t.Errorf("event requested %v", req)
			}
			if app["cpus"] != c.cpus || app["memory"] != float64(c.memory) || app["disk"] != float64(20*gib) {
				t.Errorf("event applied %v", app)
			}
		})
	}
}

// Values submit would refuse are refused the same way (422 invalid_spec),
// and the Run stays as it was: stopped, its spec unchanged.
func TestResumeRefusesInvalidResources(t *testing.T) {
	for _, c := range []struct {
		name string
		r    resumeResources
	}{
		{"zero cpus", resumeResources{CPUs: f64(0)}},
		{"negative cpus", resumeResources{CPUs: f64(-1)}},
		{"zero memory", resumeResources{Memory: bytesPtr(0)}},
		{"negative memory", resumeResources{Memory: bytesPtr(-1)}},
		{"negative disk", resumeResources{Disk: -1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, i64(gib))
			_, err := resumeWith(s, id, &c.r)
			if he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume"); len(he.Details) == 0 {
				t.Fatalf("resume: %v, want 422 invalid_spec with details", err)
			}
			if got, state := storedResources(t, s, id), storedState(t, s, id); got != withPids(resizeBase) || state != StateStopped {
				t.Errorf("after refusal: %s %+v, want stopped %+v", state, got, withPids(resizeBase))
			}
		})
	}
	// Submit's own answer for a negative resource, for the shape.
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	_, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: spec.RunSpec{Image: spec.Image{Ref: "alpine"},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}}, Resources: spec.Resources{CPUs: -1}}})
	refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "submit")
}

// Disk: larger is applied; smaller is applied down to the snapshot's
// placement's peak use plus max(25%, 1 GiB), and otherwise kept, the Run
// resuming anyway, the answer and event saying why.
func TestResumeDisk(t *testing.T) {
	for _, c := range []struct {
		name      string
		peak      *int64
		requested int64
		applied   int64
		kept      bool
	}{
		{"grow", i64(gib), 40 * gib, 40 * gib, false},
		{"shrink, 1 GiB headroom fits", i64(2 * gib), 3 * gib, 3 * gib, false},
		{"shrink, 25% headroom fits", i64(8 * gib), 10 * gib, 10 * gib, false},
		{"shrink below 1 GiB headroom", i64(2 * gib), 3*gib - 1, 20 * gib, true},
		{"shrink below 25% headroom", i64(8 * gib), 10*gib - 1, 20 * gib, true},
		{"shrink without a measurement", nil, 10 * gib, 20 * gib, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, c.peak)
			out, err := resumeWith(s, id, &resumeResources{Disk: spec.Bytes(c.requested)})
			if err != nil {
				t.Fatal(err)
			}
			if got, state := storedResources(t, s, id).Disk, storedState(t, s, id); int64(got) != c.applied || state != StateResuming {
				t.Errorf("%s with disk %d, want resuming with %d", state, got, c.applied)
			}
			if got := out.Body.Spec.Resources.Disk; int64(got) != c.applied {
				t.Errorf("answer's spec disk %d, want %d", got, c.applied)
			}
			rz := out.Body.Resize
			if rz == nil || int64(rz.Applied.Disk) != c.applied || int64(rz.Requested.Disk) != c.requested || (rz.Disk != nil) != c.kept {
				t.Fatalf("answer's resize %+v", rz)
			}
			evDisk := eventRes(resumeEvent(t, s, id), "disk")
			if !c.kept {
				if evDisk != nil {
					t.Errorf("event says disk kept: %v", evDisk)
				}
				return
			}
			if int64(rz.Disk.Kept) != 20*gib || int64(rz.Disk.Requested) != c.requested || rz.Disk.Reason == "" {
				t.Errorf("answer's disk %+v", rz.Disk)
			}
			if c.peak != nil && (rz.Disk.Measured == nil || *rz.Disk.Measured != *c.peak || rz.Disk.Needed <= c.requested) {
				t.Errorf("answer's disk measurement %+v", rz.Disk)
			}
			if evDisk["requested"] != float64(c.requested) || evDisk["kept"] != float64(20*gib) || evDisk["reason"] != rz.Disk.Reason {
				t.Errorf("event's disk %v", evDisk)
			}
		})
	}
}

// The measurement is the placement that took the snapshot the Run resumes
// from: fromSnapshot's, not the latest.
func TestResumeDiskShrinkMeasuresFromSnapshot(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id := stoppedRun(t, s, resizeBase, i64(gib))
	// A later placement used 15 GiB and took the latest snapshot.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, peak_disk_bytes)
		VALUES ('p2', 't1', $1, 'ha', 2, 'exited', $2)`, id, 15*gib)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap2', 't1', $1, 'p2', 2, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET snapshot_id = 'snap2', current_epoch = 2 WHERE id = $1`, id)

	if _, err := resumeWith(s, id, &resumeResources{Disk: spec.Bytes(4 * gib)}); err != nil {
		t.Fatal(err)
	}
	if got := storedResources(t, s, id).Disk; int64(got) != 20*gib {
		t.Fatalf("from the latest snapshot (15 GiB used): disk %d, want kept", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{
		FromSnapshot: "snap-" + id, Resources: &resumeResources{Disk: spec.Bytes(4 * gib)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedResources(t, s, id).Disk; int64(got) != 4*gib {
		t.Fatalf("from the older snapshot (1 GiB used): disk %d, want 4 GiB", got)
	}
}

// A Run resume does not accept is refused as before, its spec unchanged.
// One already resuming takes a retry of the same sizes, and refuses others.
func TestResumeResizeOfANonResumableRun(t *testing.T) {
	t.Run("resuming", func(t *testing.T) {
		s := testServer(t)
		id := stoppedRun(t, s, resizeBase, nil)
		r := &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(10 * gib)}
		for i := range 2 {
			if _, err := resumeWith(s, id, r); err != nil {
				t.Fatalf("resume %d: %v", i, err)
			}
		}
		for _, other := range []*resumeResources{{CPUs: f64(2)}, {Memory: bytesPtr(2 * gib)}, {Disk: spec.Bytes(40 * gib)}} {
			_, err := resumeWith(s, id, other)
			refused(t, err, http.StatusConflict, "not_resumable", fmt.Sprintf("resume with %+v while resuming", other))
		}
		want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(20 * gib), Pids: 1024}
		if got := storedResources(t, s, id); got != want {
			t.Errorf("spec %+v, want %+v", got, want)
		}
	})
	for _, state := range []string{StateRunning, StateTerminated} {
		t.Run(state, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, i64(gib))
			execSQL(t, s, context.Background(), `UPDATE runs SET state = $2 WHERE id = $1`, id, state)
			_, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib)})
			refused(t, err, http.StatusConflict, "not_resumable", "resume")
			if got := storedResources(t, s, id); got != withPids(resizeBase) {
				t.Errorf("spec %+v, want %+v", got, withPids(resizeBase))
			}
		})
	}
}

// A resized resume is reserved and placed at its new size. Smaller, it
// stays on its snapshot's host. Larger than that host has free, it goes to
// another host of its pool that fits; with none, it waits for capacity,
// saying so, exactly as a submit of that size would.
func TestResumeResizedPlacement(t *testing.T) {
	setup := func(t *testing.T) (*Server, string) {
		s := testServer(t)
		s.cfg.LeaseDuration = time.Minute
		id := stoppedRun(t, s, resizeBase, i64(gib))
		s.hub.polled("ha")
		return s, id
	}
	placed := func(t *testing.T, s *Server, id string) (string, spec.Resources) {
		t.Helper()
		var host string
		var res spec.Resources
		systemScan(t, s, `SELECT coalesce(p.host_id, ''), coalesce(p.resources, '{}') FROM runs r
			LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch AND p.epoch > 1 WHERE r.id = $1`, []any{id}, &host, &res)
		return host, res
	}

	t.Run("smaller, on its snapshot's host", func(t *testing.T) {
		s, id := setup(t)
		readyHost(t, s, "hb", "default", "", true)
		execSQL(t, s, context.Background(), `UPDATE hosts SET capacity = '{"cpus":64,"memory":274877906944}' WHERE id = 'hb'`)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, res := placed(t, s, id)
		if host != "ha" || res.CPUs != 1 || int64(res.Memory) != gib {
			t.Fatalf("placed on %q with %+v, want ha with 1 cpu, 1 GiB", host, res)
		}
	})
	t.Run("larger than its snapshot's host, elsewhere", func(t *testing.T) {
		s, id := setup(t)
		readyHost(t, s, "hb", "default", "", false)
		execSQL(t, s, context.Background(), `UPDATE hosts SET capacity = '{"cpus":16,"memory":68719476736}' WHERE id = 'hb'`)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(8), Memory: bytesPtr(32 * gib)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, res := placed(t, s, id)
		if host != "hb" || res.CPUs != 8 || int64(res.Memory) != 32*gib {
			t.Fatalf("placed on %q with %+v, want hb with 8 cpus, 32 GiB", host, res)
		}
	})
	t.Run("larger than any host, waits as a submit would", func(t *testing.T) {
		s, id := setup(t)
		if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(8)}); err != nil {
			t.Fatal(err)
		}
		schedule(t, s)
		host, _ := placed(t, s, id)
		_, _, reason := placedOn(t, s, id)
		const want = "waiting for capacity: 1 host in its pool lacks cpus (requested 8)"
		if host != "" || reason != want {
			t.Fatalf("host %q reason %q, want none and %q", host, reason, want)
		}
		other := submitAs(t, s, "t1", "default", "")
		execSQL(t, s, context.Background(), `UPDATE runs SET spec = jsonb_set(spec, '{resources,cpus}', '8') WHERE id = $1`, other)
		schedule(t, s)
		if _, _, r := placedOn(t, s, other); r != want {
			t.Fatalf("a submit of 8 cpus waits with %q, want %q", r, want)
		}
	})
}

// stoppingRun is a Run with resizeBase (submitOnHA) whose placement 1 on
// host ha is stopping, as stop leaves it: the Run is stopping, the
// placement asked to stop. Returns its id.
func stoppingRun(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	id := submitOnHA(t, s, resizeBase)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, stop_reason, lease_expires_at)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'stopping', '{}', 'stop', now() + interval '1 minute')`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping', current_epoch = 1, pool_id = 'default' WHERE id = $1`, id)
	return id
}

// A placement's peak disk is only a measurement of its snapshot once its
// exit status (which carries the final sample, taken after the snapshot
// was reported) has arrived. Lost between the two, a heartbeat's older,
// smaller peak does not admit a shrink: the disk is kept, saying why, and
// the Run resumes with its cpus and memory changed.
func TestResumeDiskShrinkNeedsTheFinalMeasurement(t *testing.T) {
	// The exit status cases stay under 2 GiB: recordUsage cannot store a
	// larger value (its nullif parameters are typed int4).
	const mib = 1 << 20
	for _, c := range []struct {
		name    string
		saved   int64  // the snapshot's final disk sample
		status  string // the exit status, with that sample; "": lost before it
		disk    int64
		applied int64
		reason  string
	}{
		{"lost before its exit status", 8 * gib, "", 2 * gib, 20 * gib, "no final measurement"},
		{"lost before its exit status, a shrink its peak admits", 8 * gib, "", 11 * gib, 20 * gib, "no final measurement"},
		{"exited, its final usage refuses the shrink", 1800 * mib, "exited", 2 * gib, 20 * gib, "its saved state used up to"},
		{"exited, its final usage admits the shrink", 1800 * mib, "exited", 3 * gib, 3 * gib, ""},
		{"failed, its final usage admits the shrink", 1800 * mib, "failed", 3 * gib, 3 * gib, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			id := stoppingRun(t, s)
			if err := s.heartbeat(ctx, "ha", proto.Heartbeat{Leases: []proto.LivePlacement{
				{RunID: id, Epoch: 1, State: "running", Usage: &proto.Usage{PeakDiskBytes: 100 * mib}}}}); err != nil {
				t.Fatal(err)
			}
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				_, err := s.applySnapshotDone(ctx, tx, "t1", "ha", id, 1, 1, proto.SnapshotDone{Manifest: proto.Manifest{
					SnapshotID: "snap-" + id, RunID: id, Epoch: 1,
					Volumes: []proto.VolumeSnapshot{{Name: "data", Path: "/data", BlobID: "blob-" + id, Size: c.saved, SHA256: "x"}}}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if c.status != "" {
					code := 0
					return s.applyStatus(ctx, tx, "t1", id, 1, proto.Status{State: c.status, ExitCode: &code, Reason: "stopped",
						Usage: &proto.Usage{PeakDiskBytes: c.saved}})
				}
				var later laterEvents
				if err := s.placementLost(ctx, tx, id, 1, "host lost: missed heartbeats", &later); err != nil {
					return err
				}
				return later.write()
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := resumeWith(s, id, &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(c.disk)})
			if err != nil {
				t.Fatal(err)
			}
			want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(c.applied), Pids: 1024}
			if got := storedResources(t, s, id); got != want || out.Body.State != StateResuming {
				t.Errorf("%s with %+v, want resuming with %+v", out.Body.State, got, want)
			}
			rz := out.Body.Resize
			if rz == nil || rz.Applied != want || (rz.Disk != nil) != (c.reason != "") {
				t.Fatalf("answer's resize %+v", rz)
			}
			if c.reason != "" && !strings.Contains(rz.Disk.Reason, c.reason) {
				t.Errorf("reason %q, want it to say %q", rz.Disk.Reason, c.reason)
			}
		})
	}
}

// A Run already resuming takes a retry of its resume's request (the same
// requested resources, or none) and answers it with that resume's resize;
// any other resources are refused, 409, invalid ones 422. Nothing changes.
func TestResumeRetryWhileResuming(t *testing.T) {
	first := &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(3*gib - 1)} // under the 3 GiB floor: kept
	for _, c := range []struct {
		name   string
		retry  *resumeResources
		status int // 0: 202
		resize bool
	}{
		{"repeat of the refused shrink", first, 0, true},
		{"no resources", nil, 0, false},
		{"empty resources", &resumeResources{}, 0, false},
		{"the applied disk", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(20 * gib)}, http.StatusConflict, false},
		{"a new feasible shrink", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(10 * gib)}, http.StatusConflict, false},
		{"a new impossible shrink", &resumeResources{CPUs: f64(1), Memory: bytesPtr(gib), Disk: spec.Bytes(gib)}, http.StatusConflict, false},
		{"the disk alone", &resumeResources{Disk: spec.Bytes(3*gib - 1)}, http.StatusConflict, false},
		{"other cpus", &resumeResources{CPUs: f64(2), Memory: bytesPtr(gib), Disk: spec.Bytes(3*gib - 1)}, http.StatusConflict, false},
		{"negative disk", &resumeResources{Disk: -1}, http.StatusUnprocessableEntity, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, i64(2*gib))
			out1, err := resumeWith(s, id, first)
			if err != nil {
				t.Fatal(err)
			}
			if out1.Body.Resize == nil || out1.Body.Resize.Disk == nil {
				t.Fatalf("first resume's resize %+v, want the disk kept", out1.Body.Resize)
			}
			out2, err := resumeWith(s, id, c.retry)
			if c.status != 0 {
				refused(t, err, c.status, "", "retry")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got := out2.Body.Resize
				if c.resize && (got == nil || !reflect.DeepEqual(*got, *out1.Body.Resize)) {
					t.Errorf("retry's resize %+v, want the first's %+v", got, out1.Body.Resize)
				}
				if !c.resize && got != nil {
					t.Errorf("retry's resize %+v, want none", got)
				}
			}
			want := spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(20 * gib), Pids: 1024}
			var events int
			systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'resume.requested'`, []any{id}, &events)
			if got := storedResources(t, s, id); got != want || events != 1 {
				t.Errorf("after the retry: %+v and %d resume.requested, want %+v and 1", got, events, want)
			}
		})
	}
}

// A Run resumed again after a resume that resized it, which ran and
// stopped, is compared with the new resume, not the old one. lux's own
// resume after a move writes no resume.requested, so it counts as a
// resume that requested no resources.
func TestResumeRetryComparesTheCurrentResume(t *testing.T) {
	for _, c := range []struct {
		name  string
		byLux bool
	}{
		{"resumed by the user", false},
		{"resumed by lux after a move", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			id := stoppedRun(t, s, resizeBase, i64(gib))
			if _, err := resumeWith(s, id, &resumeResources{CPUs: f64(1)}); err != nil {
				t.Fatal(err)
			}
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if !c.byLux {
					return setRunState(ctx, tx, "t1", id, StateStopped, "stop", 1)
				}
				if err := setRunState(ctx, tx, "t1", id, StateStopped, "migrate", 1); err != nil {
					return err
				}
				return s.requestResume(ctx, tx, "t1", id, nil, "auto-resume after migrate")
			})
			if err != nil {
				t.Fatal(err)
			}
			if !c.byLux {
				if _, err := resumeWith(s, id, nil); err != nil {
					t.Fatal(err)
				}
			}
			out, err := resumeWith(s, id, &resumeResources{CPUs: f64(1)})
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusConflict {
				if err == nil {
					t.Fatalf("retry with the earlier resume's cpus: 202 with resize %+v, want 409", out.Body.Resize)
				}
				t.Fatalf("retry with the earlier resume's cpus: %v, want 409", err)
			}
			if out, err := resumeWith(s, id, nil); err != nil || out.Body.Resize != nil {
				t.Fatalf("retry without resources: %v, resize %+v", err, out)
			}
		})
	}
}

// Through the HTTP API: request decoding (bytes as numbers or strings,
// absent fields), validation's 422 and the answer's JSON, then GET.
func TestResumeResizeHTTP(t *testing.T) {
	before := map[string]any{"cpus": 2.0, "memory": float64(4 * gib), "disk": float64(20 * gib), "pids": 1024.0}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range before {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name      string
		resources map[string]any
		status    int
		requested map[string]any // the answer's resize.requested
		applied   map[string]any // its resize.applied, and GET's spec.resources
		details   []string       // a 422's error.details
	}{
		{name: "memory alone, cpus absent", resources: map[string]any{"memory": "1Gi"}, status: 202,
			requested: map[string]any{"memory": float64(gib)}, applied: with("memory", float64(gib))},
		{name: "cpus alone, memory absent", resources: map[string]any{"cpus": 0.5}, status: 202,
			requested: map[string]any{"cpus": 0.5}, applied: with("cpus", 0.5)},
		{name: "memory as a number", resources: map[string]any{"memory": 2 * gib}, status: 202,
			requested: map[string]any{"memory": float64(2 * gib)}, applied: with("memory", float64(2*gib))},
		{name: "disk as a string, an old client's request", resources: map[string]any{"disk": "40Gi"}, status: 202,
			requested: map[string]any{"disk": float64(40 * gib)}, applied: with("disk", float64(40*gib))},
		{name: "disk as a number", resources: map[string]any{"disk": 30 * gib}, status: 202,
			requested: map[string]any{"disk": float64(30 * gib)}, applied: with("disk", float64(30*gib))},
		{name: "cpus, memory and disk", resources: map[string]any{"cpus": 4, "memory": "8Gi", "disk": "3Gi"}, status: 202,
			requested: map[string]any{"cpus": 4.0, "memory": float64(8 * gib), "disk": float64(3 * gib)},
			applied:   with("cpus", 4.0, "memory", float64(8*gib), "disk", float64(3*gib))},
		{name: "zero cpus", resources: map[string]any{"cpus": 0}, status: 422,
			details: []string{"resources.cpus must be greater than 0"}},
		{name: "zero memory", resources: map[string]any{"memory": 0}, status: 422,
			details: []string{"resources.memory must be greater than 0"}},
		{name: "zero cpus and memory", resources: map[string]any{"cpus": 0, "memory": "0"}, status: 422,
			details: []string{"resources.cpus must be greater than 0", "resources.memory must be greater than 0"}},
		{name: "negative disk", resources: map[string]any{"disk": -1}, status: 422,
			details: []string{"resources must not be negative"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedRun(t, s, resizeBase, i64(gib))
			key := apiKey(t, s, new("t1"), "run", "read")
			w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{"resources": c.resources})
			if w.Code != c.status {
				t.Fatalf("POST: %d %s, want %d", w.Code, w.Body, c.status)
			}
			var answer struct {
				Resize *struct {
					Requested map[string]any `json:"requested"`
					Applied   map[string]any `json:"applied"`
					Disk      map[string]any `json:"disk"`
				} `json:"resize"`
				Error struct {
					Code    string   `json:"code"`
					Details []string `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
				t.Fatalf("answer %s: %v", w.Body, err)
			}
			wantState, wantResources := StateResuming, c.applied
			if c.status == 422 {
				if answer.Error.Code != "invalid_spec" || !reflect.DeepEqual(answer.Error.Details, c.details) {
					t.Errorf("422 %s, want invalid_spec with details %q", w.Body, c.details)
				}
				wantState, wantResources = StateStopped, before
			} else if rz := answer.Resize; rz == nil || !reflect.DeepEqual(rz.Requested, c.requested) ||
				!reflect.DeepEqual(rz.Applied, c.applied) || rz.Disk != nil {
				t.Errorf("resize %s, want requested %v applied %v", w.Body, c.requested, c.applied)
			}
			g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
			var run struct {
				State string `json:"state"`
				Spec  struct {
					Resources map[string]any `json:"resources"`
				} `json:"spec"`
				Resize any `json:"resize"`
			}
			if g.Code != http.StatusOK || json.Unmarshal(g.Body.Bytes(), &run) != nil {
				t.Fatalf("GET: %d %s", g.Code, g.Body)
			}
			if run.State != wantState || !reflect.DeepEqual(run.Spec.Resources, wantResources) || run.Resize != nil {
				t.Errorf("GET: %s %v resize %v, want %s %v and no resize", run.State, run.Spec.Resources, run.Resize, wantState, wantResources)
			}
		})
	}
}
