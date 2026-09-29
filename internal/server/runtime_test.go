package server

import (
	"context"
	"testing"
	"time"
)

// A Run's runtime sums its placements from started_at to ended_at, a live
// one up to the response; a placement that never reached running adds 0.
func TestRunRuntime(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r3', 't1', '{}', 'failed', 1)`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 4 WHERE id = 'r1'`)
	live := time.Now().Add(-30 * time.Second).Truncate(time.Microsecond)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, started_at, ended_at) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited', $1, $1, $1::timestamptz + interval '100 seconds'),
		('p2', 't1', 'r1', 'h1', 2, 'lost', $1, $1::timestamptz + interval '200 seconds', $1::timestamptz + interval '260.5 seconds'),
		('p3', 't1', 'r1', 'h1', 3, 'exited', $1, NULL, $1::timestamptz + interval '300 seconds'),
		('p4', 't1', 'r1', 'h1', 4, 'running', $2, $2, NULL),
		('p5', 't1', 'r3', 'h1', 1, 'exited', $1, NULL, $1::timestamptz + interval '10 seconds')`, t0, live)
	// r4: a stopping placement still accrues; r5: a lost one missing
	// ended_at (inconsistent) adds nothing and is not live.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
		('r4', 't1', '{}', 'stopping', 1), ('r5', 't1', '{}', 'lost', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, started_at, ended_at) VALUES
		('p6', 't1', 'r4', 'h1', 1, 'stopping', $1, $1, NULL),
		('p7', 't1', 'r5', 'h1', 1, 'lost', $2, $2, NULL)`, live, t0)

	check := func(name string, r *Run, before, after time.Time) {
		t.Helper()
		if r.RuntimeSince == nil || !r.RuntimeSince.Equal(live) {
			t.Errorf("%s: runtimeSince %v, want %v", name, r.RuntimeSince, live)
		}
		lo, hi := 160.5+before.Sub(live).Seconds(), 160.5+after.Sub(live).Seconds()
		if r.RuntimeSeconds < lo-0.001 || r.RuntimeSeconds > hi+0.001 {
			t.Errorf("%s: runtimeSeconds %v, want within [%v, %v]", name, r.RuntimeSeconds, lo, hi)
		}
	}

	before := time.Now()
	var list listRunsBody
	if code := getJSON(t, s, keys["t1"], "/v1/runs", &list); code != 200 {
		t.Fatalf("list: %d", code)
	}
	after := time.Now()
	byID := map[string]*Run{}
	for _, r := range list.Runs {
		byID[r.ID] = r
	}
	check("list r1", byID["r1"], before, after)
	if r := byID["r3"]; r == nil || r.RuntimeSeconds != 0 || r.RuntimeSince != nil {
		t.Errorf("never-started r3: %+v", r)
	}
	if r := byID["r4"]; r == nil || r.RuntimeSince == nil || !r.RuntimeSince.Equal(live) || r.RuntimeSeconds < before.Sub(live).Seconds()-0.001 {
		t.Errorf("stopping r4: want live since %v, got %+v", live, r)
	}
	if r := byID["r5"]; r == nil || r.RuntimeSeconds != 0 || r.RuntimeSince != nil {
		t.Errorf("lost r5 without ended_at: want 0 and not live, got %+v", r)
	}

	before = time.Now()
	var got Run
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r1", &got); code != 200 {
		t.Fatalf("get: %d", code)
	}
	check("get r1", &got, before, time.Now())

	// Once the live placement ends, runtime is fixed and since is absent.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = started_at + interval '40 seconds' WHERE id = 'p4'`)
	got = Run{}
	if code := getJSON(t, s, keys["t1"], "/v1/runs/r1", &got); code != 200 {
		t.Fatalf("get: %d", code)
	}
	if got.RuntimeSeconds != 200.5 || got.RuntimeSince != nil {
		t.Errorf("ended: runtimeSeconds %v since %v, want 200.5 and none", got.RuntimeSeconds, got.RuntimeSince)
	}
}
