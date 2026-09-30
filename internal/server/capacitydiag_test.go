package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// livePlacement occupies a ready host with a running placement of res.
func livePlacement(t *testing.T, s *Server, host, res string) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,spec,state) VALUES ($1,'t1','{}','running')`, "live-"+host)
	execSQL(t, s, ctx, `INSERT INTO placements (id,tenant_id,run_id,host_id,epoch,state,resources) VALUES ($1,'t1',$2,$3,1,'running',$4)`,
		"p-"+host, "live-"+host, host, res)
}

func hostDecisions(t *testing.T, s *Server, host string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range events(t, s, evCapacityDecision) {
		if e.Owner == host {
			out = append(out, e.Data)
		}
	}
	return out
}

func blockerOf(t *testing.T, entries []any, host string) map[string]any {
	t.Helper()
	for _, e := range entries {
		m := e.(map[string]any)
		if m["host"] == host {
			return m["blockers"].([]any)[0].(map[string]any)
		}
	}
	t.Fatalf("no exhausted entry for %s in %v", host, entries)
	return nil
}

func wantResource(t *testing.T, b map[string]any, resource string, requested, used, capacity, available float64) {
	t.Helper()
	if b["resource"] != resource || b["requested"] != requested || b["used"] != used || b["capacity"] != capacity || b["available"] != available {
		t.Fatalf("blocker %+v, want %s requested %g used %g capacity %g available %g", b, resource, requested, used, capacity, available)
	}
}

// An ordinary launch keeps the exact fit blockers of the ready hosts it could
// not use, per resource, on the pool's scale-up and on each host's stream.
func TestScaleUpRecordsExhaustedReadyHosts(t *testing.T) {
	s, pl, p := planningFixture(t, 3)
	observePlanningHost(t, s, "cpu-full", "ready", proto.Capacity{CPUs: 4, Memory: 1000}, map[string]string{"secret": "v"})
	observePlanningHost(t, s, "mem-full", "ready", proto.Capacity{CPUs: 8, Memory: 25}, map[string]string{"secret": "v"})
	livePlacement(t, s, "cpu-full", `{"cpus":4}`)
	livePlacement(t, s, "mem-full", `{"memory":20}`)
	planningTick(t, s, pl, p, false)
	if p.calls != 2 {
		t.Fatalf("launched %d, want 2 (each new host fits two Runs by memory)", p.calls)
	}
	evs := events(t, s, evScaleUp)
	if len(evs) != 1 {
		t.Fatalf("scale-up events %d", len(evs))
	}
	d := evs[0].Data
	if d["ready"] != 0.0 || d["future"] != 0.0 || d["planned"] != 3.0 || d["unmet"] != 0.0 || d["blocked"] != 0.0 {
		t.Fatalf("summary %+v", d)
	}
	exhausted := d["exhausted"].([]any)
	if len(exhausted) != 2 {
		t.Fatalf("exhausted %v, want one entry per host", exhausted)
	}
	wantResource(t, blockerOf(t, exhausted, "cpu-full"), "cpus", 1, 4, 4, 0)
	wantResource(t, blockerOf(t, exhausted, "mem-full"), "memory", 10, 20, 25, 5)
	if _, ok := d["expected"].(map[string]any)["labels"]; ok {
		t.Fatalf("expected exposes labels: %v", d["expected"])
	}
	cpu := hostDecisions(t, s, "cpu-full")
	if len(cpu) != 1 || cpu[0]["decision"] != "blocked" || cpu[0]["stage"] != "ready" || cpu[0]["pool"] != "burst" {
		t.Fatalf("cpu-full decisions %+v", cpu)
	}
	wantResource(t, cpu[0]["blockers"].([]any)[0].(map[string]any), "cpus", 1, 4, 4, 0)
	mem := hostDecisions(t, s, "mem-full")
	if len(mem) != 1 {
		t.Fatalf("mem-full decisions %+v", mem)
	}
	wantResource(t, mem[0]["blockers"].([]any)[0].(map[string]any), "memory", 10, 20, 25, 5)
}

// Unchanged decisions are not re-appended on later passes; a changed one is.
func TestHostDecisionAppendsOnlyOnChange(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "full", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	livePlacement(t, s, "full", `{"cpus":1}`)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if n := len(hostDecisions(t, s, "full")); n != 1 {
		t.Fatalf("%d decisions after three identical passes, want 1", n)
	}
	execSQL(t, s, context.Background(), `UPDATE placements SET state='exited' WHERE id='p-full'`)
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	got := hostDecisions(t, s, "full")
	if len(got) != 2 || got[1]["decision"] != "reserved" {
		t.Fatalf("decisions %+v, want blocked then reserved", got)
	}
	// Only hosts the pass considered get decisions: here the one start launched on the first pass.
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_events WHERE type=$1 AND host_id <> 'full'`, evCapacityDecision); n != 1 {
		t.Fatalf("%d decisions on other hosts, want 1 (the start)", n)
	}
}

// Ready hosts the scheduler would not use are reported with why.
func TestScaleUpRecordsIneligibleReadyHosts(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "stale", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
	observePlanningHost(t, s, "drain", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE hosts SET last_heartbeat=now()-interval '1 day' WHERE id='stale'`)
	execSQL(t, s, context.Background(), `UPDATE hosts SET draining=true WHERE id='drain'`)
	planningTick(t, s, pl, p, false)
	d := events(t, s, evScaleUp)[0].Data
	in := d["ineligible"].([]any)
	if len(in) != 2 || in[0].(map[string]any)["reason"] != "draining" || in[1].(map[string]any)["reason"] != "heartbeat stale" {
		t.Fatalf("ineligible %v", in)
	}
	if got := hostDecisions(t, s, "stale"); len(got) != 1 || got[0]["decision"] != "ineligible" {
		t.Fatalf("stale decisions %+v", got)
	}
}

// Without registered observations, new-host capacity is unknown and says so.
func TestScaleUpUnknownCapacityReason(t *testing.T) {
	s, pl, p := planningFixture(t, 2)
	execSQL(t, s, context.Background(), `UPDATE runs SET secrets='["TOKEN"]' WHERE id='r1'`)
	planningTick(t, s, pl, p, false)
	d := events(t, s, evScaleUp)[0].Data
	if d["unknown"] == "" || d["unmet"] != 1.0 || d["blocked"] != 1.0 {
		t.Fatalf("summary %+v", d)
	}
	reasons := map[string]bool{}
	for _, e := range d["deficits"].([]any) {
		reasons[e.(map[string]any)["blockers"].([]any)[0].(map[string]any)["reason"].(string)] = true
	}
	if !reasons["run secrets unavailable"] || !reasons["new host capacity unknown"] {
		t.Fatalf("deficit reasons %v", reasons)
	}
}

// A decision writer holds the host's stream exclusively from before its read:
// a second identical writer waits, then sees the first and appends nothing; a
// shared appender in flight delays it; a different decision appends.
func TestConcurrentHostDecisions(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	infraFixture(t, s, ctx)
	blocked := map[string]any{"decision": "blocked", "pool": "burst"}
	first := holdTx(ctx, s, func(tx pgx.Tx) error { return hostDecisionEvent(ctx, tx, "h1", blocked) })
	if !first.settle(t, ctx, s) {
		t.Fatal("the first decision waited on nothing")
	}
	second := holdTx(ctx, s, func(tx pgx.Tx) error { return hostDecisionEvent(ctx, tx, "h1", blocked) })
	second.waitsOnStream(t, ctx, s, hostEvents, "h1")
	first.finish(t, ctx)
	second.finish(t, ctx)
	if n := len(hostDecisions(t, s, "h1")); n != 1 {
		t.Fatalf("%d decisions from two identical writers, want 1", n)
	}

	appender := holdTx(ctx, s, func(tx pgx.Tx) error { return hostEvent(ctx, tx, "h1", evReady, nil) })
	if !appender.settle(t, ctx, s) {
		t.Fatal("the appender waited on nothing")
	}
	changed := holdTx(ctx, s, func(tx pgx.Tx) error {
		return hostDecisionEvent(ctx, tx, "h1", map[string]any{"decision": "reserved", "pool": "burst"})
	})
	changed.waitsOnStream(t, ctx, s, hostEvents, "h1")
	appender.finish(t, ctx)
	changed.finish(t, ctx)
	got := hostDecisions(t, s, "h1")
	if len(got) != 2 || got[1]["decision"] != "reserved" {
		t.Fatalf("decisions %+v, want blocked then reserved", got)
	}
	order := queryOne[[]string](t, s, `SELECT array_agg(type ORDER BY id) FROM host_events WHERE host_id='h1'`)
	if len(order) != 3 || order[1] != evReady {
		t.Fatalf("host events %v", order)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return hostDecisionEvent(ctx, tx, "h1", blocked) }); err != nil {
		t.Fatal(err)
	}
	if n := len(hostDecisions(t, s, "h1")); n != 3 {
		t.Fatalf("a reversion to blocked must append: %d decisions", n)
	}
}
