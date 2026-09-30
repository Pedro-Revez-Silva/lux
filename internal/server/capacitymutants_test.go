package server

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// idleExpired adds ready one-Run hosts, idle past scale-down, oldest first in
// the order given; the plan reserves the first by id for the waiting Run.
func idleExpired(t *testing.T, s *Server, ids ...string) {
	t.Helper()
	for i, id := range ids {
		observePlanningHost(t, s, id, "ready", proto.Capacity{Runs: 1}, map[string]string{})
		execSQL(t, s, context.Background(), `UPDATE hosts SET registered_at = now() - interval '1 day' - make_interval(mins => $2) WHERE id = $1`,
			id, len(ids)-i)
	}
}

func drained(t *testing.T, s *Server) []string {
	t.Helper()
	return queryOne[[]string](t, s, `SELECT coalesce(array_agg(id ORDER BY id), '{}') FROM hosts WHERE draining`)
}

func TestCapacityReconcileReservedIdleScaleDown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
		warm  int
		want  []string
	}{
		// "a" is reserved for the one Run (first by id) and oldest idle.
		{"only the reserved host", []string{"a"}, 0, []string{}},
		{"the unreserved host drains", []string{"a", "b"}, 0, []string{"b"}},
		{"warm keeps one unreserved", []string{"a", "b", "c"}, 1, []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pl, p := planningFixture(t, 1)
			idleExpired(t, s, tc.hosts...)
			pl.Warm = tc.warm
			planningTick(t, s, pl, p, false)
			if got := drained(t, s); !slices.Equal(got, tc.want) {
				t.Fatalf("drained %v, want %v", got, tc.want)
			}
			if p.calls != 0 {
				t.Fatalf("launched %d", p.calls)
			}
		})
	}
}

// platformFixture is a non-shared platform pool with a {runs: 4}
// observation and one waiting Run each from t1 and t2.
func platformFixture(t *testing.T) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id,name) VALUES ('t1','t1'), ('t2','t2')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id,name,provider,template,shared) VALUES ('plat','plat','ec2','{"version":1}',false)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id,name,pool_id,state,provider_id,provision_requested_at,registered_at,launch_template,capacity)
		VALUES ('hist','hist','plat','terminated','hist',now(),now(),'{"version":1}','{"runs":4}')`)
	for _, tenant := range []string{"t1", "t2"} {
		execSQL(t, s, ctx, `INSERT INTO runs (id,tenant_id,pool_id,spec,state) VALUES ($1,$2,'plat','{"placement":{"pool":"plat"}}','provisioning')`,
			"run-"+tenant, tenant)
	}
	return s, poolRow{ID: "plat", Name: "plat", Provider: "ec2", Template: json.RawMessage(`{"version":1}`)}, &planningProvider{}
}

// A non-shared platform host serves one tenant at a time, planned hosts too.
func TestCapacityReconcileNonSharedPlatformPool(t *testing.T) {
	s, pl, p := platformFixture(t)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("two tenants on a non-shared pool launched %d, want 2", p.calls)
	}
}

func TestCapacityReconcileNonSharedPlatformStartingHost(t *testing.T) {
	s, pl, p := platformFixture(t)
	ctx := context.Background()
	// t1's Run is older: it takes the start, which t2 then cannot share.
	execSQL(t, s, ctx, `UPDATE runs SET updated_at = now() - interval '1 hour' WHERE id = 'run-t1'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id,name,pool_id,state,provider_id,provision_requested_at,launch_template)
		VALUES ('start','start','plat','provisioning','i-start',now(),'{"version":1}')`)
	for range 3 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 1 {
		t.Fatalf("t2 beside t1's start launched %d, want 1", p.calls)
	}
}

// Warm is a physical idle target: a start reserved for a waiting Run is not
// idle, so warm still launches one.
func TestCapacityReconcileWarmBesideReservedStart(t *testing.T) {
	s, pl, p := planningFixture(t, 1)
	observePlanningHost(t, s, "hist", "terminated", proto.Capacity{Runs: 1}, map[string]string{})
	planningTick(t, s, pl, p, false)
	pl.Warm = 1
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("launched %d, want the Run's start plus one warm", p.calls)
	}
}

// The same with unknown capacity: the bootstrap start is the demand's, and
// warm adds one more.
func TestCapacityReconcileWarmBesideBootstrap(t *testing.T) {
	s, pl, p := planningFixture(t, 6)
	planningTick(t, s, pl, p, false)
	pl.Warm = 1
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 2 {
		t.Fatalf("launched %d, want the bootstrap plus one warm", p.calls)
	}
}

// Observations disagreeing on a label leave it out of the expectation: a Run
// requiring it fits no new host. The label's values are not exported.
func TestCapacityReconcileExpectedLabelIntersection(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := map[bool]string{false: "registered while waiting", true: "registered before"}[stale]
		t.Run(name, func(t *testing.T) {
			s, pl, p := planningFixture(t, 1)
			ctx := context.Background()
			execSQL(t, s, ctx, `UPDATE runs SET spec = '{"placement":{"pool":"burst","requires":{"arch":"arm64"}}}', updated_at = now() - interval '1 hour'`)
			observePlanningHost(t, s, "amd", "terminated", proto.Capacity{}, map[string]string{"arch": "amd64"})
			observePlanningHost(t, s, "arm", "terminated", proto.Capacity{}, map[string]string{"arch": "arm64"})
			if stale {
				execSQL(t, s, ctx, `UPDATE hosts SET registered_at = registered_at - interval '1 day'`)
			}
			for range 3 {
				planningTick(t, s, pl, p, false)
			}
			// Observations newer than the Run: no probe. Older: one probe.
			want := map[bool]int{false: 0, true: 1}[stale]
			if p.calls != want {
				t.Fatalf("launched %d, want %d", p.calls, want)
			}
			var d map[string]any
			if stale {
				d = events(t, s, evScaleUp)[0].Data
			} else {
				d = events(t, s, evScaleBlocked)[0].Data
			}
			deficit := d["deficits"].([]any)[0].(map[string]any)
			wantDeficit := map[string]any{"run": "r0", "stage": "new_host", "blockers": []any{map[string]any{"reason": "required labels do not match"}}}
			if !reflect.DeepEqual(deficit, wantDeficit) {
				t.Fatalf("deficit %v, want %v", deficit, wantDeficit)
			}
			raw, _ := json.Marshal(d)
			if strings.Contains(string(raw), "arm64") || strings.Contains(string(raw), "amd64") {
				t.Fatalf("payload exports label values: %s", raw)
			}
		})
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
