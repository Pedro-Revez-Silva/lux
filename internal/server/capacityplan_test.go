package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			if p.calls != 1 {
				t.Fatalf("%s host must not cover demand, got %d launches", rule, p.calls)
			}
		})
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
	if d["ready"] != float64(0) || d["future"] != float64(0) || d["unmet"] != float64(12) {
		t.Fatalf("summary: %+v", d)
	}
	deficits := d["deficits"].([]any)
	if len(deficits) != 8 {
		t.Fatalf("sample length %d, want 8", len(deficits))
	}
	b := deficits[0].(map[string]any)["blockers"].([]any)[0].(map[string]any)
	if b["resource"] != "cpus" || b["requested"] != float64(3) || b["capacity"] != float64(2) || b["available"] != float64(2) {
		t.Fatalf("blocker: %+v", b)
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
