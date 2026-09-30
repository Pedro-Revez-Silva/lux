package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	want := map[string]any{"resource": resource, "requested": requested, "used": used, "capacity": capacity, "available": available}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("blocker %+v, want %+v", b, want)
	}
}

// An ordinary launch keeps the exact fit blockers of the ready hosts it could
// not use, per resource, on the pool's scale-up and on each host's stream.
func TestScaleUpRecordsExhaustedReadyHosts(t *testing.T) {
	s, pl, p := planningFixture(t, 3)
	observePlanningHost(t, s, "cpu-full", "ready", proto.Capacity{CPUs: 4, Memory: 1000}, map[string]string{"secret": "v"})
	// mem-full takes one Run (memory 30 of 35), then is exhausted.
	observePlanningHost(t, s, "mem-full", "ready", proto.Capacity{CPUs: 8, Memory: 35}, map[string]string{"secret": "v"})
	livePlacement(t, s, "cpu-full", `{"cpus":4}`)
	livePlacement(t, s, "mem-full", `{"memory":20}`)
	planningTick(t, s, pl, p, false)
	if p.calls != 1 {
		t.Fatalf("launched %d, want 1 (a new host fits three Runs by memory)", p.calls)
	}
	evs := events(t, s, evScaleUp)
	if len(evs) != 1 {
		t.Fatalf("scale-up events %d", len(evs))
	}
	d := evs[0].Data
	if d["ready"] != 1.0 || d["starting"] != 0.0 || d["planned"] != 2.0 || d["unmet"] != 0.0 || d["blocked"] != 0.0 {
		t.Fatalf("summary %+v", d)
	}
	exhausted := d["exhausted"].([]any)
	if len(exhausted) != 2 {
		t.Fatalf("exhausted %v, want one entry per host", exhausted)
	}
	wantResource(t, blockerOf(t, exhausted, "cpu-full"), "cpus", 1, 4, 4, 0)
	wantResource(t, blockerOf(t, exhausted, "mem-full"), "memory", 10, 30, 35, 5)
	if _, ok := d["expected"].(map[string]any)["labels"]; ok {
		t.Fatalf("expected exposes labels: %v", d["expected"])
	}
	cpu := hostDecisions(t, s, "cpu-full")
	if len(cpu) != 1 || cpu[0]["decision"] != "blocked" || cpu[0]["stage"] != "ready" || cpu[0]["pool"] != "burst" {
		t.Fatalf("cpu-full decisions %+v", cpu)
	}
	wantResource(t, cpu[0]["blockers"].([]any)[0].(map[string]any), "cpus", 1, 4, 4, 0)
	mem := hostDecisions(t, s, "mem-full")
	if len(mem) != 1 || mem[0]["decision"] != "exhausted" {
		t.Fatalf("mem-full decisions %+v", mem)
	}
	wantResource(t, mem[0]["blockers"].([]any)[0].(map[string]any), "memory", 10, 30, 35, 5)
}

// A host that cannot fit the oldest Run but takes later ones is exhausted,
// with the first blocker as evidence, whatever order the Runs came in.
func TestHostDecisionExhaustedAfterOversizedHead(t *testing.T) {
	s, pl, p := planningFixture(t, 3)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":8},"placement":{"pool":"burst"}}', updated_at=now()-interval '1 hour' WHERE id='r0'`)
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":1},"placement":{"pool":"burst"}}' WHERE id<>'r0'`)
	// Newest, and blocked too once the two 1-CPU Runs are reserved.
	execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state,updated_at) VALUES ('r3','t1','pool1','{"resources":{"cpus":4},"placement":{"pool":"burst"}}','provisioning',now()+interval '1 minute')`)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
	planningTick(t, s, pl, p, false)
	got := hostDecisions(t, s, "h")
	if len(got) != 1 || got[0]["decision"] != "exhausted" || got[0]["stage"] != "ready" {
		t.Fatalf("decisions %+v, want one exhausted", got)
	}
	wantResource(t, got[0]["blockers"].([]any)[0].(map[string]any), "cpus", 8, 0, 4, 4)
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
	// The start launched on the first pass was reserved for the Run until
	// "full" took it back; then it is idle.
	start := hostDecisions(t, s, p.hosts[0])
	if len(start) != 2 || start[0]["decision"] != "reserved" || start[1]["decision"] != "idle" {
		t.Fatalf("start decisions %+v, want reserved then idle", start)
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
	// Sampled by host id.
	want := []any{map[string]any{"host": "drain", "reason": "draining"}, map[string]any{"host": "stale", "reason": "heartbeat stale"}}
	if in := d["ineligible"]; !reflect.DeepEqual(in, want) {
		t.Fatalf("ineligible %v, want %v", in, want)
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
	if d["unknown"] != "no registered host observations for current template" || d["unmet"] != 1.0 || d["blocked"] != 1.0 {
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
	// An unchanged decision takes no lock: it does not wait on the appender.
	unchanged := holdTx(ctx, s, func(tx pgx.Tx) error { return hostDecisionEvent(ctx, tx, "h1", blocked) })
	if !unchanged.settle(t, ctx, s) {
		t.Fatal("an unchanged decision waited on the stream")
	}
	unchanged.finish(t, ctx)
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

// Once no waiting work considers a host, its latest decision becomes idle,
// written once.
func TestHostDecisionIdleOnceDemandClears(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "full", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	livePlacement(t, s, "full", `{"cpus":1}`)
	planningTick(t, s, pl, p, false)
	execSQL(t, s, context.Background(), `UPDATE runs SET state='cancelled' WHERE id='r0'`)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	got := hostDecisions(t, s, "full")
	if len(got) != 2 || got[0]["decision"] != "blocked" || !reflect.DeepEqual(got[1], map[string]any{"pool": "burst", "stage": "ready", "decision": "idle"}) {
		t.Fatalf("decisions %+v, want blocked then one idle", got)
	}
	// The start launched for the Run was never decided on: nothing to retract.
	if got := hostDecisions(t, s, p.hosts[0]); len(got) != 0 {
		t.Fatalf("start decisions %+v", got)
	}
}

// insertDecision records a host decision as another luxd's provisioner would.
func insertDecision(t *testing.T, s *Server, host, decision string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO host_events (tenant_id, host_id, type, data)
		SELECT tenant_id, id, $2, jsonb_build_object('pool', 'burst', 'stage', 'ready', 'decision', $3::text) FROM hosts WHERE id = $1`,
		host, evCapacityDecision, decision)
}

// This process knows which hosts it decided on. A previous provisioner's
// verdict is read from the database on the first pass per pool and
// retracted once; later passes with no demand read no decisions, so a verdict
// another process writes after that is not retracted by this one: it is that
// process's to retract.
func TestHostDecisionIdleSweepOncePerLease(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	insertDecision(t, s, "h", "blocked")
	planningTick(t, s, pl, p, false)
	got := hostDecisions(t, s, "h")
	if len(got) != 2 || !reflect.DeepEqual(got[1], map[string]any{"pool": "burst", "stage": "ready", "decision": "idle"}) {
		t.Fatalf("decisions %+v, want the other process's blocked, then idle", got)
	}
	insertDecision(t, s, "h", "exhausted")
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if got := hostDecisions(t, s, "h"); len(got) != 3 || got[2]["decision"] != "exhausted" {
		t.Fatalf("decisions %+v, want the later external verdict left alone", got)
	}
	// Taking the provisioner lease again sweeps again.
	s.tookProvisionLease()
	planningTick(t, s, pl, p, false)
	if got := hostDecisions(t, s, "h"); len(got) != 4 || got[3]["decision"] != "idle" {
		t.Fatalf("decisions %+v, want idle after a new lease", got)
	}
}

// Retracting a verdict is scoped to the pool that made it, stages a
// provisioning host as starting, and leaves terminated hosts alone.
func TestHostDecisionIdleScope(t *testing.T) {
	t.Run("other pool untouched", func(t *testing.T) {
		s, pl, p := planningFixture(t, 1)
		ctx := context.Background()
		execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template) VALUES ('pool2','t1','other','ec2','{"version":1}')`)
		observePlanningHost(t, s, "a", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
		livePlacement(t, s, "a", `{"cpus":1}`)
		observePlanningHost(t, s, "b", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
		execSQL(t, s, ctx, `UPDATE hosts SET pool_id='pool2' WHERE id='b'`)
		livePlacement(t, s, "b", `{"cpus":1}`)
		execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state) VALUES ('rb','t1','pool2','{"resources":{"cpus":1},"placement":{"pool":"other"}}','provisioning')`)
		pl2 := poolRow{ID: "pool2", Name: "other", Provider: "ec2", TenantID: new("t1"), Template: json.RawMessage(`{"version":1}`)}
		// pool2 decides first, so pool1's first pass finds b's verdict if it
		// looked beyond its own pool.
		planningTick(t, s, pl2, p, false)
		planningTick(t, s, pl, p, false)
		execSQL(t, s, ctx, `UPDATE runs SET state='cancelled' WHERE id='r0'`)
		for range 2 {
			planningTick(t, s, pl, p, false)
		}
		if got := hostDecisions(t, s, "b"); len(got) != 1 || got[0]["decision"] != "blocked" || got[0]["pool"] != "other" {
			t.Fatalf("other pool's host decisions %+v, want only its blocked", got)
		}
		if got := hostDecisions(t, s, "a"); len(got) != 2 || got[1]["decision"] != "idle" {
			t.Fatalf("host a decisions %+v, want blocked then idle", got)
		}
	})
	t.Run("chosen host in another pool is retracted under its pool", func(t *testing.T) {
		s, pl, p := planningFixture(t, 1)
		ctx := context.Background()
		execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template) VALUES ('pool2','t1','other','ec2','{"version":1}')`)
		observePlanningHost(t, s, "chosen", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
		execSQL(t, s, ctx, `UPDATE hosts SET pool_id='pool2' WHERE id='chosen'`)
		livePlacement(t, s, "chosen", `{"cpus":1}`)
		execSQL(t, s, ctx, `UPDATE runs SET place_on='chosen' WHERE id='r0'`)
		pl2 := poolRow{ID: "pool2", Name: "other", Provider: "ec2", TenantID: new("t1"), Template: json.RawMessage(`{"version":1}`)}
		// pool2 sweeps first, so only pool1's own record can retract it.
		planningTick(t, s, pl2, p, false)
		planningTick(t, s, pl, p, false)
		if got := hostDecisions(t, s, "chosen"); len(got) != 1 || got[0]["decision"] != "blocked" {
			t.Fatalf("chosen decisions %+v, want blocked", got)
		}
		execSQL(t, s, ctx, `UPDATE runs SET state='cancelled' WHERE id='r0'`)
		for range 3 {
			planningTick(t, s, pl2, p, false)
			planningTick(t, s, pl, p, false)
		}
		got := hostDecisions(t, s, "chosen")
		if last := got[len(got)-1]; !reflect.DeepEqual(last, map[string]any{"pool": "other", "stage": "ready", "decision": "idle"}) {
			t.Fatalf("chosen decisions %+v, want idle in pool other last", got)
		}
	})
	t.Run("provisioning host idles as starting", func(t *testing.T) {
		s, pl, p := planningFixture(t, 1)
		observePlanningHost(t, s, "hist", "terminated", proto.Capacity{CPUs: 4}, map[string]string{})
		// The first pass launches the start, the second reserves it.
		for range 2 {
			planningTick(t, s, pl, p, false)
		}
		execSQL(t, s, context.Background(), `UPDATE runs SET state='cancelled'`)
		planningTick(t, s, pl, p, false)
		got := hostDecisions(t, s, p.hosts[0])
		want := []map[string]any{{"pool": "burst", "stage": "starting", "decision": "reserved"}, {"pool": "burst", "stage": "starting", "decision": "idle"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("start decisions %+v, want %+v", got, want)
		}
	})
	t.Run("terminated host", func(t *testing.T) {
		s, pl, p := planningFixture(t, 1)
		observePlanningHost(t, s, "full", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
		livePlacement(t, s, "full", `{"cpus":1}`)
		planningTick(t, s, pl, p, false)
		execSQL(t, s, context.Background(), `UPDATE hosts SET state='terminated' WHERE id='full'`)
		execSQL(t, s, context.Background(), `UPDATE runs SET state='cancelled' WHERE id='r0'`)
		for range 2 {
			planningTick(t, s, pl, p, false)
		}
		if got := hostDecisions(t, s, "full"); len(got) != 1 || got[0]["decision"] != "blocked" {
			t.Fatalf("terminated host decisions %+v, want only blocked", got)
		}
	})
}

// A pass records at most maxHostDecisions hosts; the window rotates, so every
// host of a larger pool gets its decision within two passes.
func TestHostDecisionWindowRotates(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	for i := range 40 {
		id := fmt.Sprintf("full-%02d", i)
		observePlanningHost(t, s, id, "ready", proto.Capacity{CPUs: 1}, map[string]string{})
		livePlacement(t, s, id, `{"cpus":1}`)
	}
	// Enough hosts are starting that no launch adds a 41st host.
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":1},"placement":{"pool":"burst"}}'`)
	pl.Max = 40
	counts := []int{}
	for range 2 {
		planningTick(t, s, pl, p, false)
		counts = append(counts, queryOne[int](t, s, `SELECT count(*) FROM host_events WHERE type=$1`, evCapacityDecision))
	}
	if counts[0] != maxHostDecisions || counts[1] != 40 {
		t.Fatalf("decisions after each pass %v, want [32 40]", counts)
	}
	if n := queryOne[int](t, s, `SELECT count(DISTINCT host_id) FROM host_events WHERE type=$1`, evCapacityDecision); n != 40 {
		t.Fatalf("%d hosts decided, want 40", n)
	}
}

// A Run waiting for its chosen host is no evidence about the pool's other hosts.
func TestCapacityReconcileChosenHostIsNotOtherHostsEvidence(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "chosen", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	observePlanningHost(t, s, "other", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	livePlacement(t, s, "chosen", `{"cpus":1}`)
	execSQL(t, s, context.Background(), `UPDATE runs SET place_on = 'chosen'`)
	pl.Min = 3
	planningTick(t, s, pl, p, false)
	if got := hostDecisions(t, s, "other"); len(got) != 0 {
		t.Fatalf("other host decisions %+v", got)
	}
	exhausted := events(t, s, evScaleUp)[0].Data["exhausted"].([]any)
	if len(exhausted) != 1 || exhausted[0].(map[string]any)["host"] != "chosen" {
		t.Fatalf("exhausted %v, want only the chosen host", exhausted)
	}
}

// A chosen host in another pool is still the evidence for the Run that
// chose it, beside a Run of the same tenant that chose none.
func TestCapacityReconcileChosenHostInAnotherPool(t *testing.T) {
	s, pl, p := planningFixture(t, 2)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template) VALUES ('pool2','t1','other','ec2','{"version":1}')`)
	observePlanningHost(t, s, "hist", "terminated", proto.Capacity{CPUs: 4}, map[string]string{})
	observePlanningHost(t, s, "chosen", "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET pool_id='pool2' WHERE id='chosen'`)
	livePlacement(t, s, "chosen", `{"cpus":1}`)
	execSQL(t, s, ctx, `UPDATE runs SET updated_at=now()-interval '1 hour' WHERE id='r0'`)
	execSQL(t, s, ctx, `UPDATE runs SET place_on='chosen' WHERE id='r1'`)
	planningTick(t, s, pl, p, false)
	d := events(t, s, evScaleUp)[0].Data
	want := []any{map[string]any{"run": "r1", "host": "chosen", "stage": "ready", "blockers": []any{
		map[string]any{"resource": "cpus", "requested": 1.0, "used": 1.0, "capacity": 1.0, "available": 0.0}}}}
	if !reflect.DeepEqual(d["exhausted"], want) {
		t.Fatalf("exhausted %v, want %v", d["exhausted"], want)
	}
}

// A ready host that never heartbeat is ineligible with that reason.
func TestScaleUpRecordsNoHeartbeatHost(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "silent", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE hosts SET last_heartbeat = NULL`)
	planningTick(t, s, pl, p, false)
	in := events(t, s, evScaleUp)[0].Data["ineligible"]
	if want := []any{map[string]any{"host": "silent", "reason": "no heartbeat"}}; !reflect.DeepEqual(in, want) {
		t.Fatalf("ineligible %v, want %v", in, want)
	}
}
