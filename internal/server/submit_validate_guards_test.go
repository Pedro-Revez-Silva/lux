package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func TestSubmitValidateOnlyMalformedSpecAndNamesOnlySecrets(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "pool", nil)
	sp := baseSpec("pool", spec.Resources{})
	sp.Spot, sp.ResumePolicy = true, "auto"
	_, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" {
		t.Fatalf("malformed: %v, want 422 invalid_spec", err)
	}
	sp.ResumePolicy = ""
	sp.Secrets = []spec.Secret{{Name: "TOKEN"}}
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil || !out.Body.Valid {
		t.Fatalf("names-only secrets: %v", err)
	}
	// Normal submission still requires the values.
	_, err = s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if !errors.As(err, &he) || he.Code != "secrets_required" {
		t.Fatalf("normal submit without values: %v", err)
	}
	sp.Secrets[0].Value = "not-stored-by-validation"
	if _, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp}); err != nil {
		t.Fatal(err)
	}
	s.secrets.mu.Lock()
	cache, msgs := len(s.secrets.m), len(s.secrets.msgs)
	s.secrets.mu.Unlock()
	if cache != 0 || msgs != 0 {
		t.Fatalf("validation cached secrets: %d Runs/%d messages", cache, msgs)
	}
	for _, table := range []string{"runs", "placements", "run_events", "host_messages", "spot_pledges", "host_capacity_age", "host_events", "pool_events"} {
		if n := queryOne[int](t, s, `SELECT count(*) FROM `+table); n != 0 {
			t.Fatalf("validation wrote %s: %d rows", table, n)
		}
	}
}

func TestSubmitValidateOnlyShapedRoundingAndPoolID(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "shaped", &testShape)
	sp := baseSpec("", spec.Resources{CPUs: 3})
	sp.Placement.PoolID = "shaped"
	sp.Spot = true
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.EffectivePool != "shaped" || out.Body.EffectivePoolID != "shaped" || *out.Body.EffectiveResources != sized(4) {
		t.Fatalf("effective answer %+v; want shaped/shaped, m-4 resources", out.Body)
	}
	if out.Body.Run != nil {
		t.Fatal("validation invented a Run")
	}
}

func TestSubmitValidateOnlyBlockers(t *testing.T) {
	for _, c := range []struct {
		name   string
		code   string
		change func(*testing.T, *Server, *spec.RunSpec)
	}{
		{"aged spare", "", func(t *testing.T, s *Server, sp *spec.RunSpec) {}},
		{"no host", blockNoHosts, func(t *testing.T, s *Server, sp *spec.RunSpec) {
			execSQL(t, s, context.Background(), `UPDATE hosts SET state='lost' WHERE id='h'`)
		}},
		{"labels", blockConstraint, func(t *testing.T, s *Server, sp *spec.RunSpec) {
			sp.Placement.Requires = map[string]string{"gpu": "yes"}
		}},
		{"nested", blockConstraint, func(t *testing.T, s *Server, sp *spec.RunSpec) { sp.Sandbox.NestedContainers = true }},
		{"old runner", blockCapability, func(t *testing.T, s *Server, sp *spec.RunSpec) {
			execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities=ARRAY[]::text[] WHERE id='h'`)
		}},
		{"not enough", blockNotEnough, func(t *testing.T, s *Server, sp *spec.RunSpec) { sp.Resources.CPUs = 5 }},
		{"not settled", blockNotSettled, func(t *testing.T, s *Server, sp *spec.RunSpec) {
			at := time.Now()
			s.now = func() time.Time { return at }
			seedHistory(t, s, "h", ageEntry{at.Add(-30 * time.Second), freeVec{4, 40, 40, 4}})
		}},
		{"normal demand", blockNormalPending, func(t *testing.T, s *Server, sp *spec.RunSpec) {
			execSQL(t, s, context.Background(), `INSERT INTO runs(id,tenant_id,pool_id,spec,state) VALUES('normal','t1','pool1','{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}','resuming')`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := planningFixture(t, 0)
			observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
			execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
			seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
			sp := baseSpec("burst", spec.Resources{CPUs: 1, Memory: 10, Disk: 10})
			sp.Spot = true
			c.change(t, s, &sp)
			before := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`)
			events := queryOne[int](t, s, `SELECT (SELECT count(*) FROM host_events) + (SELECT count(*) FROM pool_events)`)
			out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
			if err != nil {
				t.Fatal(err)
			}
			if !out.Body.Valid || out.Body.WouldStartNow != (c.code == "") {
				t.Fatalf("answer %+v", out.Body)
			}
			var codes []string
			for _, b := range out.Body.Blockers {
				codes = append(codes, b.Code)
				// Free since 30s before now: it qualifies once over 60s, in 31s.
				if c.code == blockNotSettled && (b.RemainingSeconds == nil || *b.RemainingSeconds != 31) {
					t.Fatalf("remainingSeconds %v, want 31", derefInt(b.RemainingSeconds))
				}
			}
			var want []string
			if c.code != "" {
				want = []string{c.code}
			}
			if !reflect.DeepEqual(codes, want) {
				t.Fatalf("blockers %v, want %v", codes, want)
			}
			if after := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`); after != before {
				t.Fatal("age mutated")
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 0 {
				t.Fatal("placement created")
			}
			if n := queryOne[int](t, s, `SELECT (SELECT count(*) FROM host_events) + (SELECT count(*) FROM pool_events)`); n != events {
				t.Fatalf("validation wrote host or pool events: %d -> %d", events, n)
			}
		})
	}
}

// space_not_settled reports the shortest wait over its hosts: one host's
// spare space has been free for 30s (31s to go), another's for 50s (11s).
func TestSubmitValidateOnlyNotSettledReportsShortestWait(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	at := time.Now()
	s.now = func() time.Time { return at }
	for id, since := range map[string]time.Duration{"a": 30 * time.Second, "b": 50 * time.Second} {
		observePlanningHost(t, s, id, "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
		execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities=$1 WHERE id=$2`, []string{proto.CapSpotReclaim}, id)
		seedHistory(t, s, id, ageEntry{at.Add(-since), freeVec{4, 40, 40, 4}})
	}
	sp := baseSpec("burst", spec.Resources{CPUs: 1, Memory: 10, Disk: 10})
	sp.Spot = true
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.WouldStartNow || len(out.Body.Blockers) != 1 {
		t.Fatalf("answer %+v", out.Body)
	}
	b := out.Body.Blockers[0]
	if b.Code != blockNotSettled || b.Hosts != 2 || b.RemainingSeconds == nil || *b.RemainingSeconds != 11 {
		t.Fatalf("blocker %+v (remaining %v), want space_not_settled on 2 hosts, 11s", b, derefInt(b.RemainingSeconds))
	}
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// A normal Run's dry run: hostFit only. A host it fits gives wouldStartNow
// and that host; a full one gives not_enough_spare.
func TestSubmitValidateOnlyNormalRun(t *testing.T) {
	for _, c := range []struct {
		name string
		cpus float64
		host string
		code string
	}{
		{"fits", 2, "h", ""},
		{"misfit", 5, "", blockNotEnough},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := planningFixture(t, 0)
			observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
			sp := baseSpec("burst", spec.Resources{CPUs: c.cpus, Memory: 10, Disk: 10})
			out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, b := range out.Body.Blockers {
				codes = append(codes, b.Code)
			}
			if !out.Body.Valid || out.Body.WouldStartNow != (c.host != "") || out.Body.EligibleHost != c.host {
				t.Fatalf("answer %+v, want wouldStartNow on %q", out.Body, c.host)
			}
			var want []string
			if c.code != "" {
				want = []string{c.code}
			}
			if !reflect.DeepEqual(codes, want) {
				t.Fatalf("blockers %v, want %v", codes, want)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
				t.Fatal("validation created a Run")
			}
		})
	}
}
