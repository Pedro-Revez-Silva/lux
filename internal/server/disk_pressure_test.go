package server

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// pressureOf is a runner's disk pressure report: 4.1 GiB free under a 5%
// low mark of a 82 GiB filesystem.
func pressureOf(noRelief bool) *proto.DiskPressure {
	return &proto.DiskPressure{FreeBytes: 4403019776, LowFreeBytes: 4402341478, RecoverFreeBytes: 8804682956,
		LowFree: "5%", RecoverFree: "10%", NoRelief: noRelief}
}

const pressureBlocker = "disk pressure: 4.1 GiB free, below 5%"

// A host under disk pressure refuses every Run, whatever else fits, with
// a blocker naming the free space and the low mark; the wait reason
// counts it, spot admission classifies it, and the planner's evidence
// names it without the volatile free bytes.
func TestHostFitDiskPressure(t *testing.T) {
	r, h := fitFixture()
	h.DiskPressure = pressureOf(false)
	want := []fitBlocker{{Kind: kindDiskPressure, Reason: pressureBlocker}}
	if got := hostFit(r, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers %+v, want %+v", got, want)
	}
	r.Spec.Spot = true
	h.Caps = []string{proto.CapSpotReclaim}
	if got := hostFit(r, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("spot: blockers %+v, want %+v", got, want)
	}
	h.DiskPressure.NoRelief = true
	if got := hostFit(r, h); len(got) != 1 || got[0].Reason != pressureBlocker+"; nothing to reclaim and no Run over its disk limit" {
		t.Fatalf("no relief: %+v", got)
	}
	set := blockerSet{}
	if set.classify(hostFit(r, h)) || set.list()[0].Code != blockDiskPressure {
		t.Fatalf("spot classification %+v", set.list())
	}
	if got := diagnosticBlockers(want); !reflect.DeepEqual(got, []planBlocker{{Reason: "disk pressure"}}) {
		t.Fatalf("plan blockers %+v", got)
	}
	var w waitCapacity
	w.add(want)
	w.add([]fitBlocker{{Kind: kindResource, Resource: "cpus"}})
	if got := w.reason(r); got != "waiting for capacity: 1 host in its pool lacks cpus (requested 2.5), 1 is under disk pressure (low free disk)" {
		t.Fatalf("reason %q", got)
	}
	h.DiskPressure = nil
	if got := hostFit(r, h); len(got) != 0 {
		t.Fatalf("no pressure: %+v", got)
	}
	// A host the Run could not use anyway is a constraint, whatever its
	// pressure: another pool's, another tenant's, or not its chosen host.
	for name, scope := range map[string]func(*pendingRun, *candidateHost){
		"another pool":    func(_ *pendingRun, h *candidateHost) { h.PoolID = "elsewhere" },
		"another tenant":  func(_ *pendingRun, h *candidateHost) { h.TenantID = new("t2") },
		"not chosen host": func(r *pendingRun, _ *candidateHost) { r.PlaceOn = "other" },
	} {
		r, h := fitFixture()
		r.Spec.Spot = true
		h.Caps = []string{proto.CapSpotReclaim}
		h.DiskPressure = pressureOf(false)
		scope(&r, h)
		set := blockerSet{}
		if set.classify(hostFit(r, h)) || len(set.list()) != 1 || set.list()[0].Code != blockConstraint {
			t.Errorf("%s under pressure: classified %+v, want only %s", name, set.list(), blockConstraint)
		}
		var w waitCapacity
		w.add(hostFit(r, h))
		if got := w.reason(r); got != "" {
			t.Errorf("%s under pressure: wait reason %q, want none", name, got)
		}
	}
}

// pressureHost is planningFixture's pool with one connected ready host h
// that fits one Run of the fixture's size, its spot reclamation on.
func pressureHost(t *testing.T) (*Server, context.Context, poolRow, *planningProvider) {
	t.Helper()
	s, pl, p := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapSpotReclaim, proto.CapDiskScale})
	return s, ctx, pl, p
}

// beat sends h's heartbeat with pressure p (nil: none), with Usage nil when
// noUsage is set, as a runner that could not read its host's usage.
func beat(t *testing.T, s *Server, ctx context.Context, p *proto.DiskPressure, noUsage bool) {
	t.Helper()
	hb := proto.Heartbeat{}
	if !noUsage {
		free := int64(1 << 30)
		hb.Usage = &proto.HostUsage{FSFreeBytes: &free, DiskPressure: p}
	}
	if err := s.heartbeat(ctx, "h", hb); err != nil {
		t.Fatal(err)
	}
}

func runWaitReason(t *testing.T, s *Server, id string) (string, string) {
	t.Helper()
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = $1`, []any{id}, &state, &reason)
	return state, reason
}

// A heartbeat reporting disk pressure blocks the host for normal, spot and
// resuming Runs alike, each waiting with a reason naming it; the host's
// events record when it started and ended, and GET /v1/hosts shows it.
// A heartbeat without usage (the runner could not read it) keeps the
// pressure; once one with usage carries none, the same Runs are placed.
func TestDiskPressureBlocksAdmissionsUntilItEnds(t *testing.T) {
	for _, noUsage := range []bool{false, true} {
		name := "cleared by a heartbeat without pressure"
		if noUsage {
			name = "kept by a heartbeat without usage, then cleared"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx, _, _ := pressureHost(t)
			at := time.Now()
			s.now = func() time.Time { return at }
			beat(t, s, ctx, pressureOf(false), false)
			if got := queryOne[*proto.DiskPressure](t, s, `SELECT disk_pressure FROM hosts WHERE id = 'h'`); !reflect.DeepEqual(got, pressureOf(false)) {
				t.Fatalf("stored pressure %+v", got)
			}
			// The spot Run's space has been free for over a minute.
			if err := s.scheduleOnce(ctx); err != nil {
				t.Fatal(err)
			}
			at = at.Add(61 * time.Second)
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, session_id) VALUES
				('normal', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'submitted', ''),
				('resumed', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'resuming', 'sess'),
				('spot', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'submitted', '')`)
			if err := s.scheduleOnce(ctx); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]string{
				"normal":  "waiting for capacity: 1 host in its pool is under disk pressure (low free disk)",
				"resumed": "waiting for capacity: 1 host in its pool is under disk pressure (low free disk)",
				"spot":    "waiting for spot capacity: 1 host is under disk pressure (low free disk)",
			} {
				if state, reason := runWaitReason(t, s, id); state == StateScheduled || reason != want {
					t.Errorf("%s: %s %q, want waiting with %q", id, state, reason, want)
				}
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 0 {
				t.Fatalf("%d placements on a host under pressure", n)
			}
			// The same pressure again records nothing; a change of relief does.
			beat(t, s, ctx, pressureOf(false), false)
			beat(t, s, ctx, pressureOf(true), false)
			evs := events(t, s, evDiskPressure)
			if len(evs) != 2 || evs[0].Data["reason"] != pressureBlocker || evs[0].Data["noRelief"] != false || evs[1].Data["noRelief"] != true {
				t.Fatalf("host.disk_pressure events %+v", evs)
			}
			var host Host
			if code := getJSON(t, s, operatorKey(t, s, ctx), "/v1/hosts/h", &host); code != http.StatusOK ||
				host.DiskPressure == nil || !host.DiskPressure.NoRelief || host.DiskPressure.LowFree != "5%" {
				t.Fatalf("GET /v1/hosts/h: %d diskPressure %+v", code, host.DiskPressure)
			}

			if noUsage {
				beat(t, s, ctx, nil, true)
				if got := queryOne[*proto.DiskPressure](t, s, `SELECT disk_pressure FROM hosts WHERE id = 'h'`); !reflect.DeepEqual(got, pressureOf(true)) {
					t.Fatalf("pressure %+v after a heartbeat without usage, want it kept", got)
				}
				if evs := events(t, s, evDiskPressureEnded); len(evs) != 0 {
					t.Fatalf("host.disk_pressure_ended events %+v after a heartbeat without usage", evs)
				}
				execSQL(t, s, ctx, `UPDATE runs SET updated_at = now()`)
				if err := s.scheduleOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 0 {
					t.Fatalf("%d placements after a heartbeat without usage", n)
				}
			}
			beat(t, s, ctx, nil, false)
			if got := queryOne[*proto.DiskPressure](t, s, `SELECT disk_pressure FROM hosts WHERE id = 'h'`); got != nil {
				t.Fatalf("pressure %+v after a heartbeat without it", got)
			}
			if evs := events(t, s, evDiskPressureEnded); len(evs) != 1 {
				t.Fatalf("host.disk_pressure_ended events %+v", evs)
			}
			execSQL(t, s, ctx, `UPDATE runs SET updated_at = now()`)
			if err := s.scheduleOnce(ctx); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"normal", "resumed"} {
				if state, reason := runWaitReason(t, s, id); state != StateScheduled {
					t.Errorf("%s after the pressure: %s %q, want scheduled", id, state, reason)
				}
			}
			// Spot admission waits for the space to settle again: the
			// two normal Runs took part of it. The host now refuses it
			// for capacity age, not pressure.
			if _, reason := runWaitReason(t, s, "spot"); strings.Contains(reason, "disk pressure") {
				t.Errorf("spot after the pressure: %q still names it", reason)
			}
		})
	}
}

// A heartbeat that never carried pressure stores none and records no
// event: an older runner's host is never blocked for it.
func TestHeartbeatWithoutPressureFieldIsNoPressure(t *testing.T) {
	s, ctx, _, _ := pressureHost(t)
	beat(t, s, ctx, nil, true)
	beat(t, s, ctx, nil, false)
	if got := queryOne[*proto.DiskPressure](t, s, `SELECT disk_pressure FROM hosts WHERE id = 'h'`); got != nil {
		t.Fatalf("stored %+v", got)
	}
	if evs := append(events(t, s, evDiskPressure), events(t, s, evDiskPressureEnded)...); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('normal', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'submitted')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := runWaitReason(t, s, "normal"); state != StateScheduled {
		t.Fatalf("%s %q, want scheduled", state, reason)
	}
}

// A disk.pressure_stop report stops the placement (stop reason
// disk-pressure, the Run stopping), and its end fails the Run with a
// reason saying how far over which limit it was; the event keeps that
// reason for the tenant.
func TestDiskPressureStopFailsTheRunWithItsReason(t *testing.T) {
	s, ctx, _ := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE run_id = $1`, r1)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, r1)
	gib := float64(1 << 30)
	ev := proto.RunEvent{Type: proto.EvDiskPressureStop, Data: map[string]any{"usedBytes": 49.8 * gib, "limitBytes": 37.5 * gib,
		"excessBytes": 12.3 * gib, "freeBytes": 4 * gib, "lowFreeBytes": 8 * gib, "recoverFreeBytes": 16 * gib}}
	if reply := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgRunEvent, RunID: r1, Epoch: 1, Data: proto.Marshal(ev)}); reply.Type != proto.MsgAck {
		t.Fatalf("report: %s %s", reply.Type, reply.Data)
	}
	const why = "stopped: host disk pressure, 12.3 GiB over its 37.5 GiB disk limit"
	var stop, state string
	systemScan(t, s, `SELECT p.stop_reason, r.state FROM placements p JOIN runs r ON r.id = p.run_id WHERE p.run_id = $1`, []any{r1}, &stop, &state)
	if stop != stopDiskPressure || state != StateStopping {
		t.Fatalf("placement stop %q, Run %s; want %q and stopping", stop, state, stopDiskPressure)
	}
	if got := queryOne[string](t, s, `SELECT data->>'reason' FROM run_events WHERE run_id = $1 AND type = $2`, r1, proto.EvDiskPressureStop); got != why {
		t.Fatalf("event reason %q, want %q", got, why)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_messages WHERE run_id = $1 AND type = $2`, r1, proto.MsgStop); n != 1 {
		t.Fatalf("%d stop messages queued, want 1", n)
	}
	code := 143
	reportStatus(t, s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})
	if state, reason := runWaitReason(t, s, r1); state != StateFailed || reason != why {
		t.Fatalf("Run %s %q, want failed with %q", state, reason, why)
	}
}

// A Run the guard stopped whose export then ran out of space (its
// snapshot.done carries the error) still ends failed for the disk pressure:
// the snapshot failure is its own event, not its reason.
func TestDiskPressureStopWithAFailedExportKeepsItsReason(t *testing.T) {
	s, ctx, _ := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'running' WHERE run_id = $1`, r1)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, r1)
	gib := float64(1 << 30)
	ev := proto.RunEvent{Type: proto.EvDiskPressureStop, Data: map[string]any{"usedBytes": 49.8 * gib, "limitBytes": 37.5 * gib,
		"excessBytes": 12.3 * gib, "freeBytes": 0, "lowFreeBytes": 8 * gib, "recoverFreeBytes": 16 * gib}}
	if reply := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgRunEvent, RunID: r1, Epoch: 1, Data: proto.Marshal(ev)}); reply.Type != proto.MsgAck {
		t.Fatalf("report: %s %s", reply.Type, reply.Data)
	}
	const enospc = "export data: write /var/lib/lux/snapshots/blob.zst.tmp: no space left on device"
	sd := proto.SnapshotDone{Error: enospc}
	if reply := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgSnapshotDone, RunID: r1, Epoch: 1, Data: proto.Marshal(sd)}); reply.Type != proto.MsgAck {
		t.Fatalf("snapshot.done: %s %s", reply.Type, reply.Data)
	}
	code := 143
	reportStatus(t, s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})
	const why = "stopped: host disk pressure, 12.3 GiB over its 37.5 GiB disk limit"
	if state, reason := runWaitReason(t, s, r1); state != StateFailed || reason != why {
		t.Fatalf("Run %s %q, want failed with %q", state, reason, why)
	}
	if got := queryOne[string](t, s, `SELECT data->>'error' FROM run_events WHERE run_id = $1 AND type = 'snapshot.failed'`, r1); got != enospc {
		t.Fatalf("snapshot.failed error %q, want %q", got, enospc)
	}
}

// A resume onto a chosen host under pressure waits for it, saying so.
func TestDiskPressureBlocksAResumeOntoItsChosenHost(t *testing.T) {
	s, ctx, _, _ := pressureHost(t)
	beat(t, s, ctx, pressureOf(false), false)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, session_id, place_on) VALUES
		('resumed', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'resuming', 'sess', 'h')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// An ec2 pool's waiting Run is provisioning; it is not placed.
	if state, reason := runWaitReason(t, s, "resumed"); state == StateScheduled || reason != "waiting for capacity: its chosen host is under disk pressure (low free disk)" {
		t.Fatalf("%s %q", state, reason)
	}
}

// The capacity planner does not count a ready host under pressure as
// room: a waiting Run that would fit it asks for a new host. Without the
// pressure the same Run is reserved on it and nothing launches.
func TestCapacityPlanSkipsHostUnderDiskPressure(t *testing.T) {
	for _, pressured := range []bool{false, true} {
		s, ctx, pl, p := pressureHost(t)
		// Known capacity for a new host, so the plan can launch one.
		observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
		if pressured {
			beat(t, s, ctx, pressureOf(false), false)
		}
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
			('r0', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'provisioning')`)
		planningTick(t, s, pl, p, false)
		want := 0
		if pressured {
			want = 1
		}
		if p.calls != want {
			t.Errorf("pressure %v: launched %d hosts, want %d", pressured, p.calls, want)
		}
		if pressured {
			evs := events(t, s, evCapacityDecision)
			if len(evs) == 0 || !strings.Contains(fmt.Sprint(evs[len(evs)-1].Data["blockers"]), "disk pressure") {
				t.Errorf("host decision %+v, want its blocker to name disk pressure", evs)
			}
		}
	}
}
