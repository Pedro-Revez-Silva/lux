package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

type planningProvider struct {
	calls     int
	hosts     []string
	instances map[string]Instance
	fail      bool
}

func (p *planningProvider) Launch(_ context.Context, _ json.RawMessage, tags, _ map[string]string) (Launched, error) {
	p.calls++
	if p.fail {
		return Launched{}, errors.New("launch refused")
	}
	id := fmt.Sprintf("i-%d", p.calls)
	p.hosts = append(p.hosts, tags[tagHost])
	if p.instances == nil {
		p.instances = map[string]Instance{}
	}
	p.instances[id] = Instance{State: "pending", Tags: tags}
	return Launched{ProviderID: id}, nil
}
func (p *planningProvider) Terminate(_ context.Context, _ json.RawMessage, id string) error {
	delete(p.instances, id)
	return nil
}
func (p *planningProvider) Instances(context.Context, json.RawMessage, map[string]string) (map[string]Instance, error) {
	return p.instances, nil
}

func planningFixture(t *testing.T, count int) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id,name) VALUES ('t1','t1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template) VALUES ('pool1','t1','burst','ec2','{"version":1}')`)
	for i := 0; i < count; i++ {
		execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state) VALUES ($1,'t1','pool1','{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}','provisioning')`, fmt.Sprintf("r%d", i))
	}
	return s, poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1"), Template: json.RawMessage(`{"version":1}`)}, &planningProvider{}
}
func observePlanningHost(t *testing.T, s *Server, id, state string, capacity proto.Capacity, labels map[string]string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,tenant_id,pool_id,state,provider_id,provision_requested_at,registered_at,last_heartbeat,launch_template,capacity,labels)
 VALUES ($1,$1,'t1','pool1',$2,$1,now(),now(),now(),'{"version":1}',$3,$4)`, id, state, capacity, labels)
	if state == "ready" {
		s.hub.polled(id)
	}
}
func planningTick(t *testing.T, s *Server, pl poolRow, p *planningProvider, check bool) {
	t.Helper()
	if err := s.reconcilePool(context.Background(), p, pl, check); err != nil {
		t.Fatal(err)
	}
}
func TestCapacityReconcileColdBurstAndDelayedRegistration(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	for range 4 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 1 {
		t.Fatalf("cold burst launched %d, want one bootstrap", p.calls)
	}
	// The pool waits for its bootstrap host: that is not a blocked scale-up.
	if evs := events(t, s, evScaleBlocked); len(evs) != 0 {
		t.Fatalf("scale-blocked while the bootstrap starts: %+v", evs)
	}
	id := p.hosts[0]
	execSQL(t, s, context.Background(), `UPDATE hosts SET state='ready',registered_at=now(),last_heartbeat=now(),capacity=$2 WHERE id=$1`, id, proto.Capacity{CPUs: 6, Memory: 60, Disk: 60, Runs: 6})
	s.hub.polled(id)
	planningTick(t, s, pl, p, false)
	if p.calls != 1 {
		t.Fatalf("registered six-run host triggered %d launches", p.calls)
	}
	if err := s.scheduleOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE host_id=$1`, id); got != 6 {
		t.Fatalf("placed %d, want six", got)
	}
}
func TestCapacityReconcileKnownBurst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity proto.Capacity
		want     int
	}{
		{"six", proto.Capacity{CPUs: 6, Memory: 60, Disk: 60, Runs: 6}, 1},
		{"cpu", proto.Capacity{CPUs: 3}, 2},
		{"memory", proto.Capacity{Memory: 30}, 2},
		{"disk", proto.Capacity{Disk: 30}, 2},
		{"runs", proto.Capacity{Runs: 3}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			observePlanningHost(t, s, "history", "terminated", tc.capacity, map[string]string{})
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != tc.want {
				t.Fatalf("launched %d, want %d", p.calls, tc.want)
			}
		})
	}
}
func TestCapacityReconcileLiveReservations(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	observePlanningHost(t, s, "busy", "ready", proto.Capacity{CPUs: 6, Runs: 6}, map[string]string{})
	execSQL(t, s, context.Background(), `INSERT INTO runs (id,tenant_id,spec,state) VALUES ('live','t1','{}','running')`)
	execSQL(t, s, context.Background(), `INSERT INTO placements (id,tenant_id,run_id,host_id,epoch,state,resources) VALUES ('p','t1','live','busy',1,'running','{"cpus":3}')`)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 1 {
		t.Fatalf("busy spare plus one future should cover burst, got %d launches", p.calls)
	}
}
func TestCapacityReconcileConservativeHistoryAndIncompatibleRuns(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	observePlanningHost(t, s, "large", "terminated", proto.Capacity{CPUs: 6}, map[string]string{"nested": "true", "zone": "a"})
	observePlanningHost(t, s, "small", "terminated", proto.Capacity{CPUs: 2}, map[string]string{"zone": "b"})
	execSQL(t, s, context.Background(), `UPDATE runs SET spec='{"resources":{"cpus":3},"placement":{"pool":"burst"}}' WHERE id='r0'`)
	execSQL(t, s, context.Background(), `UPDATE runs SET spec='{"sandbox":{"nestedContainers":true},"placement":{"pool":"burst"}}' WHERE id='r1'`)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("four compatible runs need two hosts, got %d", p.calls)
	}
}
func TestCapacityReconcileTemplateEditColdBootstrap(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 6}, map[string]string{})
	planningTick(t, s, pl, p, false)
	pl.Template = json.RawMessage(`{"version":2}`)
	execSQL(t, s, context.Background(), `UPDATE pools SET template=$1 WHERE id='pool1'`, pl.Template)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("new template must bootstrap once independently, got %d", p.calls)
	}
}
func TestCapacityReconcileProviderGoneBeforePlan(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 6}, map[string]string{})
	planningTick(t, s, pl, p, false)
	p.instances["i-1"] = Instance{State: "terminated"}
	planningTick(t, s, pl, p, true)
	if p.calls != 2 {
		t.Fatalf("gone future host needs replacement this tick, got %d", p.calls)
	}
}

// A warm start whose launch never recorded its instance id, past the launch
// timeout, is written off and replaced in the same pass, without a provider
// check.
func TestCapacityReconcileAbandonedWarmStart(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	pl.Warm = 1
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,tenant_id,pool_id,state,provision_requested_at,launch_template,tagged)
		VALUES ('abandoned','abandoned','t1','pool1','provisioning',now()-interval '1 day','{"version":1}',true)`)
	planningTick(t, s, pl, p, false)
	if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id='abandoned'`); state != "terminated" || p.calls != 1 {
		t.Fatalf("abandoned start %s, %d launches; want terminated and 1", state, p.calls)
	}
}

// Runs that fit the expected host are planned as usual beside an oversized
// one; the planned hosts are the re-observation, no extra probe.
func TestCapacityReconcileProbeBesidePlannedHosts(t *testing.T) {
	s, pl, p := staleExpectation(t)
	for i := range 3 {
		execSQL(t, s, context.Background(), `INSERT INTO runs (id,tenant_id,pool_id,spec,state) VALUES ($1,'t1','pool1','{"resources":{"cpus":2},"placement":{"pool":"burst"}}','provisioning')`,
			fmt.Sprintf("fit%d", i))
	}
	planningTick(t, s, pl, p, false)
	up := events(t, s, evScaleUp)
	if p.calls != 3 || len(up) != 1 || up[0].Data["planned"] != 3.0 || up[0].Data["probe"] != nil {
		t.Fatalf("launched %d with %+v, want 3 planned and no probe", p.calls, up)
	}
}

// A warm start the provider check finds gone is replaced in the same pass:
// the pass counts starts again after the check.
func TestCapacityReconcileProviderGoneWarmStart(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	pl.Warm = 1
	planningTick(t, s, pl, p, false)
	p.instances["i-1"] = Instance{State: "terminated"}
	planningTick(t, s, pl, p, true)
	if p.calls != 2 {
		t.Fatalf("gone warm start needs replacement this tick, got %d launches", p.calls)
	}
}
func TestCapacityReconcileBlockedDemandAndIndependentTargets(t *testing.T) {
	for _, blocked := range []string{"chosen", "secrets", "snapshot", "unavailable", "invalid"} {
		t.Run(blocked, func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			switch blocked {
			case "chosen":
				execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,state) VALUES ('other','other','lost')`)
				execSQL(t, s, context.Background(), `UPDATE runs SET place_on='other'`)
			case "secrets":
				execSQL(t, s, context.Background(), `UPDATE runs SET secrets='["TOKEN"]'`)
			case "snapshot", "unavailable", "invalid":
				execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,state) VALUES ('source','source','ready')`)
				execSQL(t, s, context.Background(), `INSERT INTO placements (id,tenant_id,run_id,host_id,epoch,state) VALUES ('source-p','t1','r0','source',1,'exited')`)
				execSQL(t, s, context.Background(), `INSERT INTO snapshots (id,tenant_id,run_id,placement_id,host_id,epoch,manifest,available,uploaded) VALUES ('snap','t1','r0','source-p','source',1,'{}',true,false)`)
				execSQL(t, s, context.Background(), `UPDATE runs SET snapshot_id='snap'`)
				if blocked == "unavailable" {
					execSQL(t, s, context.Background(), `UPDATE snapshots SET available=false, uploaded=true`)
				}
				if blocked == "invalid" {
					execSQL(t, s, context.Background(), `UPDATE snapshots SET uploaded=true, manifest='{"volumes":[{"blobId":"foreign"}]}'`)
				}
			}
			planningTick(t, s, pl, p, false)
			if p.calls != 0 {
				t.Fatalf("blocked demand launched %d hosts", p.calls)
			}
			pl.Min = 2
			for range 2 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != 2 {
				t.Fatalf("minimum should independently launch two, got %d", p.calls)
			}
		})
	}
}
func TestCapacityReconcileProtectsReservedIdleAndWarm(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	observePlanningHost(t, s, "idle", "ready", proto.Capacity{CPUs: 6, Runs: 6}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE hosts SET registered_at=now()-interval '1 day' WHERE id='idle'`)
	pl.Warm = 1
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if queryOne[bool](t, s, `SELECT draining FROM hosts WHERE id='idle'`) {
		t.Fatal("reserved idle host drained")
	}
	if p.calls != 1 {
		t.Fatalf("warm physical idle target needs one extra host, got %d", p.calls)
	}
}
func TestCapacityReconcileExpiredStartDoesNotSuppressBootstrap(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	planningTick(t, s, pl, p, false)
	execSQL(t, s, context.Background(), `UPDATE hosts SET provision_requested_at=now()-interval '1 day'`)
	planningTick(t, s, pl, p, false)
	if p.calls != 2 {
		t.Fatalf("expired start suppressed replacement: %d launches", p.calls)
	}
	if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id=$1`, p.hosts[0]); state != "terminated" {
		t.Fatalf("expired host state %s", state)
	}
}

func TestCapacityReconcileObservedUnlimitedAndCommonLabels(t *testing.T) {
	for _, finite := range []bool{false, true} {
		t.Run(fmt.Sprint(finite), func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			observePlanningHost(t, s, "unlimited", "terminated", proto.Capacity{}, map[string]string{"arch": "arm64"})
			if finite {
				observePlanningHost(t, s, "finite", "terminated", proto.Capacity{Runs: 2}, map[string]string{"arch": "arm64"})
			}
			execSQL(t, s, context.Background(), `UPDATE runs SET spec=jsonb_set(spec,'{placement,requires}','{"arch":"arm64"}')`)
			for range 2 {
				planningTick(t, s, pl, p, false)
			}
			want := 1
			if finite {
				want = 3
			}
			if p.calls != want {
				t.Fatalf("observed limits launched %d, want %d", p.calls, want)
			}
		})
	}
}

func TestCapacityReconcileWarmWhileActive(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	pl.Warm = 2
	pl.WarmWhileActive = true
	planningTick(t, s, pl, p, false)
	if p.calls != 0 {
		t.Fatalf("inactive pool launched %d warm hosts", p.calls)
	}
	observePlanningHost(t, s, "busy", "ready", proto.Capacity{Runs: 6}, map[string]string{})
	execSQL(t, s, context.Background(), `INSERT INTO runs (id,tenant_id,spec,state) VALUES ('live','t1','{}','running')`)
	execSQL(t, s, context.Background(), `INSERT INTO placements (id,tenant_id,run_id,host_id,epoch,state) VALUES ('p','t1','live','busy',1,'running')`)
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("active pool needs two physical warm hosts, got %d", p.calls)
	}
}

func TestCapacityReconcileOldTemplateStartCountsForPhysicalWarm(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	pl.Warm = 1
	planningTick(t, s, pl, p, false)
	pl.Template = json.RawMessage(`{"version":2}`)
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 1 {
		t.Fatalf("template edits must not duplicate physical warm starts: %d", p.calls)
	}
}

func TestCapacityReconcileSafetyLimits(t *testing.T) {
	for _, rule := range []string{"max", "quota", "retired"} {
		t.Run(rule, func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			observePlanningHost(t, s, "history", "terminated", proto.Capacity{Runs: 1}, map[string]string{})
			want := 2
			switch rule {
			case "max":
				pl.Max = 2
			case "quota":
				execSQL(t, s, context.Background(), `UPDATE tenants SET max_hosts=2 WHERE id='t1'`)
			case "retired":
				pl.Retired = true
				execSQL(t, s, context.Background(), `UPDATE pools SET retired=true WHERE id='pool1'`)
				want = 0
			}
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != want {
				t.Fatalf("%s launched %d, want %d", rule, p.calls, want)
			}
		})
	}
}

func TestCapacityReconcileReadyEligibility(t *testing.T) {
	for _, rule := range []string{"stale", "disconnected", "draining"} {
		t.Run(rule, func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			observePlanningHost(t, s, "host", "ready", proto.Capacity{CPUs: 6}, map[string]string{})
			switch rule {
			case "stale":
				execSQL(t, s, context.Background(), `UPDATE hosts SET last_heartbeat=now()-interval '1 day'`)
			case "disconnected":
				s.hub.mu.Lock()
				delete(s.hub.polls, "host")
				s.hub.mu.Unlock()
			case "draining":
				execSQL(t, s, context.Background(), `UPDATE hosts SET draining=true`)
			}
			planningTick(t, s, pl, p, false)
			want := 1
			if rule == "disconnected" {
				// Another luxd may hold its connection: a fresh heartbeat counts.
				want = 0
			}
			if p.calls != want {
				t.Fatalf("%s host: %d launches, want %d", rule, p.calls, want)
			}
		})
	}
}

func TestCapacityReconcileHeartbeatHostOnOtherInstance(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "elsewhere", "ready", proto.Capacity{CPUs: 6}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE hosts SET registered_at=now()-interval '1 day' WHERE id='elsewhere'`)
	s.hub.mu.Lock()
	delete(s.hub.polls, "elsewhere")
	s.hub.mu.Unlock()
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 0 {
		t.Fatalf("host connected to another luxd: %d launches", p.calls)
	}
	if got := queryOne[int](t, s, `SELECT count(*) FROM hosts WHERE draining`); got != 0 {
		t.Fatalf("drained %d hosts reserved for the waiting run", got)
	}
}

func TestCapacityReconcileExpectationIdentity(t *testing.T) {
	for _, rule := range []string{"pool", "tenant", "template", "static", "unregistered", "jsonb"} {
		t.Run(rule, func(t *testing.T) {
			s, pl, p := planningFixture(t, 6)
			observePlanningHost(t, s, "history", "terminated", proto.Capacity{Runs: 2}, map[string]string{})
			want := 1
			switch rule {
			case "pool":
				execSQL(t, s, context.Background(), `INSERT INTO pools (id,name,provider) VALUES ('other','other','ec2')`)
				execSQL(t, s, context.Background(), `UPDATE hosts SET pool_id='other'`)
			case "tenant":
				execSQL(t, s, context.Background(), `INSERT INTO tenants (id,name) VALUES ('other','other')`)
				execSQL(t, s, context.Background(), `UPDATE hosts SET tenant_id='other'`)
			case "template":
				execSQL(t, s, context.Background(), `UPDATE hosts SET launch_template='{"version":2}'`)
			case "static":
				execSQL(t, s, context.Background(), `UPDATE hosts SET provision_requested_at=NULL`)
			case "unregistered":
				execSQL(t, s, context.Background(), `UPDATE hosts SET registered_at=NULL`)
			case "jsonb":
				pl.Template = json.RawMessage(`{ "version" : 1.0 }`)
				want = 3
			}
			for range 2 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != want {
				t.Fatalf("%s launched %d, want %d", rule, p.calls, want)
			}
		})
	}
}

func TestCapacityReconcileBoundedExactSummary(t *testing.T) {
	s, pl, p := planningFixture(t, 12)
	observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	execSQL(t, s, context.Background(), `UPDATE runs SET spec='{"resources":{"cpus":3},"placement":{"pool":"burst"}}'`)
	pl.Min = 1
	planningTick(t, s, pl, p, false)
	if p.calls != 1 {
		t.Fatalf("only minimum should launch, got %d", p.calls)
	}
	evs := events(t, s, evScaleUp)
	if len(evs) != 1 {
		t.Fatalf("scale-up events: %d", len(evs))
	}
	d := evs[0].Data
	if d["ready"] != float64(0) || d["starting"] != float64(0) || d["planned"] != float64(0) || d["unmet"] != float64(12) || d["omitted"] != float64(4) {
		t.Fatalf("summary: %+v", d)
	}
	deficits := d["deficits"].([]any)
	if len(deficits) != 8 {
		t.Fatalf("sample length %d, want 8", len(deficits))
	}
	wantBlockers := []any{map[string]any{"resource": "cpus", "requested": 3.0, "used": 0.0, "capacity": 2.0, "available": 2.0}}
	if b := deficits[0].(map[string]any)["blockers"]; !reflect.DeepEqual(b, wantBlockers) {
		t.Fatalf("blockers %+v, want %+v", b, wantBlockers)
	}
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 1 {
		t.Fatalf("oversized demand caused repeated launches: %d", p.calls)
	}
}

func TestCapacityReconcileFailureRetriesOneBootstrap(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	p.fail = true
	for range 2 {
		if err := s.reconcilePool(context.Background(), p, pl, false); err == nil {
			t.Fatal("want launch error")
		}
	}
	if p.calls != 2 {
		t.Fatalf("failed cold launches must retry one per tick, got %d", p.calls)
	}
	p.fail = false
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 3 {
		t.Fatalf("successful bootstrap must stop further demand launches, got %d", p.calls)
	}
}

// staleExpectation: one Run needing 4 CPUs, and a 2-CPU observation of the
// current template registered before the Run began waiting.
func staleExpectation(t *testing.T) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":4},"placement":{"pool":"burst"}}'`)
	observePlanningHost(t, s, "old", "terminated", proto.Capacity{CPUs: 2}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at=now()-interval '1 day' WHERE id='old'`)
	return s, pl, p
}

func registerProbe(t *testing.T, s *Server, id string, capacity proto.Capacity) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE hosts SET state='ready',registered_at=now(),last_heartbeat=now(),capacity=$2 WHERE id=$1`, id, capacity)
	s.hub.polled(id)
}

func TestCapacityReconcileStaleExpectationProbesOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cpus   float64
		placed int
	}{{"larger", 8, 1}, {"same", 2, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			s, pl, p := staleExpectation(t)
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != 1 {
				t.Fatalf("stale expectation launched %d, want one probe", p.calls)
			}
			if evs := events(t, s, evScaleBlocked); len(evs) != 0 {
				t.Fatalf("scale-blocked while the probe starts: %+v", evs)
			}
			up := events(t, s, evScaleUp)
			if len(up) != 1 || up[0].Data["probe"] != true {
				t.Fatalf("scale-up %+v, want one probe", up)
			}
			registerProbe(t, s, p.hosts[0], proto.Capacity{CPUs: tc.cpus})
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != 1 {
				t.Fatalf("after the probe registered: %d launches, want 1", p.calls)
			}
			if err := s.scheduleOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE host_id=$1`, p.hosts[0]); got != tc.placed {
				t.Fatalf("placed %d on the probe, want %d", got, tc.placed)
			}
		})
	}
}

// With want 0 and unmet Runs, the pool says why it does not scale; a stuck
// pool records that once, however many passes it stays stuck.
func TestCapacityReconcileScaleBlockedOnce(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":4},"placement":{"pool":"burst"}}'`)
	// Registered after the Run began waiting: no probe is due.
	observePlanningHost(t, s, "old", "terminated", proto.Capacity{CPUs: 2}, map[string]string{})
	s.providerError(ctx, poolEvents, "pool1", "list", "", errors.New("throttled"))
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 0 {
		t.Fatalf("launched %d", p.calls)
	}
	evs := events(t, s, evScaleBlocked)
	if len(evs) != 1 || evs[0].Count != 1 {
		t.Fatalf("scale-blocked rows %+v, want one", evs)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(`{"cause":"no_fit","waiting":1,"total":0,"max":0,
		"ready":0,"starting":0,"planned":0,"unmet":1,"blocked":0,"unknown":"",
		"expected":{"capacity":{"cpus":2,"memory":0,"disk":0,"runs":0},"observations":1},
		"deficits":[{"run":"r0","stage":"new_host","blockers":[{"resource":"cpus","requested":4,"used":0,"capacity":2,"available":2}]}],
		"exhausted":[],"ineligible":[],"omitted":0}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evs[0].Data, want) {
		t.Fatalf("scale-blocked data\n got %v\nwant %v", evs[0].Data, want)
	}
	// A stuck pool's provider errors and its blocked passes are one retry
	// loop: a repeated error folds across the blocked row written after it.
	s.providerError(ctx, poolEvents, "pool1", "list", "", errors.New("throttled"))
	if evs := events(t, s, evPoolProviderErr); len(evs) != 1 || evs[0].Count != 2 {
		t.Fatalf("provider-error rows %+v, want one with count 2", evs)
	}
}

// A pool stuck on one oversized Run beside a busy host: placements land and
// the host's usage moves every pass, and the blocked state is recorded once.
// A new Run it cannot serve changes the state and adds a row.
func TestCapacityReconcileScaleBlockedBusyPool(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":64},"placement":{"pool":"burst"}}', updated_at=now()-interval '1 hour'`)
	observePlanningHost(t, s, "busy", "ready", proto.Capacity{CPUs: 8}, map[string]string{})
	for i := range 10 {
		execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state) VALUES ($1,'t1','pool1','{"resources":{"cpus":0.5},"placement":{"pool":"burst"}}','provisioning')`,
			fmt.Sprintf("small%d", i))
		s.hub.polled("busy")
		if err := s.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
		planningTick(t, s, pl, p, false)
	}
	if got := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE host_id='busy'`); got != 10 {
		t.Fatalf("placed %d small Runs, want 10", got)
	}
	if p.calls != 0 {
		t.Fatalf("launched %d", p.calls)
	}
	evs := events(t, s, evScaleBlocked)
	if len(evs) != 1 || evs[0].Count != 1 {
		t.Fatalf("scale-blocked rows %d (%+v), want one over 10 passes", len(evs), evs)
	}
	// Waiting since before the latest registration: no probe, a new deficit.
	execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state,updated_at) VALUES ('big2','t1','pool1','{"resources":{"cpus":32},"placement":{"pool":"burst"}}','provisioning',now()-interval '1 hour')`)
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	evs = events(t, s, evScaleBlocked)
	if len(evs) != 2 || evs[1].Data["unmet"] != 2.0 {
		t.Fatalf("scale-blocked rows %+v, want a second with unmet 2", evs)
	}
}

// no_fit deficits are told apart by stage and blockers, not by Run: a queue
// of oversized Runs rotating keeps one row; a new kind of blocker adds one.
func TestCapacityReconcileScaleBlockedNoFitRotatingQueue(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "old", "terminated", proto.Capacity{CPUs: 2, Memory: 100}, map[string]string{})
	// Waiting since before the registration: no probe is due.
	arrive := func(id, res string) {
		execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state,updated_at) VALUES ($1,'t1','pool1',
			jsonb_build_object('resources', $2::jsonb, 'placement', '{"pool":"burst"}'::jsonb),'provisioning',now()-interval '1 hour')`, id, res)
	}
	for i := range 3 {
		arrive(fmt.Sprintf("big%d", i), `{"cpus":4}`)
	}
	for i := range 8 {
		execSQL(t, s, ctx, `UPDATE runs SET state='cancelled' WHERE id=$1`, fmt.Sprintf("big%d", i))
		arrive(fmt.Sprintf("big%d", i+3), `{"cpus":4}`)
		planningTick(t, s, pl, p, false)
	}
	evs := events(t, s, evScaleBlocked)
	if p.calls != 0 || len(evs) != 1 || evs[0].Data["cause"] != "no_fit" {
		t.Fatalf("launched %d, scale-blocked %+v, want one no_fit row over 8 passes", p.calls, evs)
	}
	// Same count waiting; one Run is now blocked on memory instead.
	execSQL(t, s, ctx, `UPDATE runs SET state='cancelled' WHERE id='big8'`)
	arrive("fat", `{"memory":1000}`)
	planningTick(t, s, pl, p, false)
	if evs := events(t, s, evScaleBlocked); len(evs) != 2 || evs[1].Data["unmet"] != 3.0 {
		t.Fatalf("scale-blocked %+v, want a second row for the memory blocker", evs)
	}
}

// capFixture: 20 one-slot Runs, new hosts known to take 8, and one full
// ready host of an older template.
func capFixture(t *testing.T) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s, pl, p := planningFixture(t, 20)
	observePlanningHost(t, s, "history", "terminated", proto.Capacity{Runs: 8}, map[string]string{})
	observePlanningHost(t, s, "full", "ready", proto.Capacity{Runs: 1}, map[string]string{})
	// An older template's host: not an observation of new hosts.
	execSQL(t, s, context.Background(), `UPDATE hosts SET launch_template='{"version":0}' WHERE id='full'`)
	livePlacement(t, s, "full", `{"cpus":1}`)
	return s, pl, p
}

// --max and the tenant's host quota stopping a scale-up are recorded once,
// with how many hosts were wanted.
func TestCapacityReconcileScaleBlockedCause(t *testing.T) {
	for _, tc := range []struct {
		cause  string
		wanted float64
	}{{"max", 3}, {"quota", 3}} {
		t.Run(tc.cause, func(t *testing.T) {
			s, pl, p := capFixture(t)
			ctx := context.Background()
			if tc.cause == "max" {
				pl.Max = 1
			} else {
				execSQL(t, s, ctx, `UPDATE tenants SET max_hosts=1 WHERE id='t1'`)
			}
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			if p.calls != 0 {
				t.Fatalf("launched %d", p.calls)
			}
			evs := events(t, s, evScaleBlocked)
			if len(evs) != 1 {
				t.Fatalf("scale-blocked rows %+v, want one", evs)
			}
			d := evs[0].Data
			delete(d, "exhausted")
			want := map[string]any{"cause": tc.cause, "wanted": tc.wanted, "waiting": 20.0, "total": 1.0, "max": float64(pl.Max),
				"ready": 0.0, "starting": 0.0, "planned": 20.0, "unmet": 0.0, "blocked": 0.0, "unknown": "",
				"expected": map[string]any{"capacity": map[string]any{"cpus": 0.0, "memory": 0.0, "disk": 0.0, "runs": 8.0}, "observations": 1.0},
				"deficits": []any{}, "ineligible": []any{}, "omitted": 0.0}
			if !reflect.DeepEqual(d, want) {
				t.Fatalf("scale-blocked\n got %v\nwant %v", d, want)
			}
			// Backlog size and fit change, but the cap remains the cause.
			for i := range 4 {
				execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state)
					VALUES ($1, 't1', 'pool1', '{"resources":{"cpus":1},"placement":{"pool":"burst"}}', 'provisioning')`, fmt.Sprintf("arrival%d", i))
				planningTick(t, s, pl, p, false)
			}
			execSQL(t, s, ctx, `UPDATE runs SET spec = '{"resources":{"cpus":1},"placement":{"pool":"burst","requires":{"arch":"missing"}}}' WHERE id = 'arrival0'`)
			planningTick(t, s, pl, p, false)
			execSQL(t, s, ctx, `UPDATE runs SET cancel_requested = true WHERE id LIKE 'arrival%'`)
			planningTick(t, s, pl, p, false)
			if evs := events(t, s, evScaleBlocked); len(evs) != 1 {
				t.Fatalf("moving backlog wrote %d blocked rows for %s, want one", len(evs), tc.cause)
			}
		})
	}
}

// A scale-up ends a blocked state: the same cap on the same queue after one
// is recorded again. A changed cause alone is a new state too.
func TestCapacityReconcileScaleBlockedEnds(t *testing.T) {
	t.Run("by a scale-up", func(t *testing.T) {
		s, pl, p := capFixture(t)
		for _, max := range []int{1, 2, 1, 1} {
			pl.Max = max
			planningTick(t, s, pl, p, false)
		}
		evs := events(t, s, evScaleBlocked)
		if p.calls != 1 || len(evs) != 2 || evs[0].Data["cause"] != "max" || evs[1].Data["cause"] != "max" {
			t.Fatalf("launched %d, scale-blocked %+v, want two max rows around one launch", p.calls, evs)
		}
	})
	t.Run("by its cause", func(t *testing.T) {
		s, pl, p := capFixture(t)
		ctx := context.Background()
		// --max 2 leaves room for one host, and the quota refuses it.
		pl.Max = 2
		execSQL(t, s, ctx, `UPDATE tenants SET max_hosts=1 WHERE id='t1'`)
		planningTick(t, s, pl, p, false)
		// A second full host: --max now stops it first; only the cause changed.
		execSQL(t, s, ctx, `UPDATE tenants SET max_hosts=0 WHERE id='t1'`)
		observePlanningHost(t, s, "full2", "ready", proto.Capacity{Runs: 1}, map[string]string{})
		execSQL(t, s, ctx, `UPDATE hosts SET launch_template='{"version":0}' WHERE id='full2'`)
		livePlacement(t, s, "full2", `{"cpus":1}`)
		planningTick(t, s, pl, p, false)
		evs := events(t, s, evScaleBlocked)
		if p.calls != 0 || len(evs) != 2 || evs[0].Data["cause"] != "quota" || evs[1].Data["cause"] != "max" {
			t.Fatalf("launched %d, scale-blocked %+v, want quota then max", p.calls, evs)
		}
	})
}

// A pool above a lowered --max with nothing wanted is not blocked.
func TestCapacityReconcileScaleBlockedNothingWanted(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	for _, id := range []string{"a", "b", "c"} {
		observePlanningHost(t, s, id, "ready", proto.Capacity{CPUs: 1}, map[string]string{})
	}
	pl.Max = 1
	planningTick(t, s, pl, p, false)
	if evs := events(t, s, evScaleBlocked); p.calls != 0 || len(evs) != 0 {
		t.Fatalf("launched %d, scale-blocked %+v, want none", p.calls, evs)
	}
}

// A warm start in flight is not a start that will serve an unfittable Run.
func TestCapacityReconcileScaleBlockedBesideWarmStart(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":4},"placement":{"pool":"burst"}}', updated_at=now()-interval '1 hour'`)
	observePlanningHost(t, s, "old", "terminated", proto.Capacity{CPUs: 2}, map[string]string{})
	pl.Warm = 1
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	evs := events(t, s, evScaleBlocked)
	if p.calls != 1 || len(evs) != 1 || evs[0].Data["cause"] != "no_fit" {
		t.Fatalf("launched %d, scale-blocked %+v, want one warm start and one no_fit row", p.calls, evs)
	}
}

// A pool whose Runs fit its ready host, and a pass that launches, record no
// blocked scale-up.
func TestCapacityReconcileScaleBlockedAbsent(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		s, pl, p := planningFixture(t, 1)
		observePlanningHost(t, s, "fits", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
		for range 3 {
			planningTick(t, s, pl, p, false)
		}
		if evs := events(t, s, evScaleBlocked); p.calls != 0 || len(evs) != 0 {
			t.Fatalf("launched %d, scale-blocked %+v", p.calls, evs)
		}
	})
	t.Run("launching", func(t *testing.T) {
		s, pl, p := planningFixture(t, 3)
		planningTick(t, s, pl, p, false)
		if evs := events(t, s, evScaleBlocked); p.calls != 1 || len(evs) != 0 {
			t.Fatalf("launched %d, scale-blocked %+v", p.calls, evs)
		}
	})
}

// Only the latest expectationWindow registrations count: an older, smaller
// instance type ages out.
func TestCapacityReconcileExpectationWindowAgesOut(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec='{"resources":{"cpus":4},"placement":{"pool":"burst"}}'`)
	observePlanningHost(t, s, "old", "terminated", proto.Capacity{CPUs: 2}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at=now()-interval '1 day' WHERE id='old'`)
	for i := range expectationWindow {
		observePlanningHost(t, s, fmt.Sprintf("new%d", i), "terminated", proto.Capacity{CPUs: 8}, map[string]string{})
	}
	planningTick(t, s, pl, p, false)
	up := events(t, s, evScaleUp)
	if p.calls != 1 || len(up) != 1 || up[0].Data["planned"] != 1.0 || up[0].Data["probe"] != nil {
		t.Fatalf("launched %d with %+v, want one planned host", p.calls, up)
	}
	if got := up[0].Data["expected"].(map[string]any); got["observations"] != float64(expectationWindow) || got["capacity"].(map[string]any)["cpus"] != 8.0 {
		t.Fatalf("expected %v", got)
	}
}
