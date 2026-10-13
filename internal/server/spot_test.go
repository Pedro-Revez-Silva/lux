package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func TestSpotSubmitPolicyAndPersistence(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "pool", &testShape)
	for _, policy := range []string{"", "never", "auto", "restart", "manual", "invalid"} {
		t.Run(policy, func(t *testing.T) {
			sp := baseSpec("pool", spec.Resources{CPUs: 3})
			sp.Spot, sp.ResumePolicy = true, policy
			out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
			if policy != "" && policy != "never" {
				var he *HTTPError
				want := fmt.Sprintf("resumePolicy: spot Runs require never (or omit resumePolicy), got %q", policy)
				if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" || !reflect.DeepEqual(he.Details, []string{want}) {
					t.Fatalf("error = %#v, want 422 invalid_spec with %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var stored spec.RunSpec
			systemScan(t, s, `SELECT spec FROM runs WHERE id = $1`, []any{out.Body.ID}, &stored)
			if !stored.Spot || stored.ResumePolicy != "never" || stored.Resources != sized(4) {
				t.Fatalf("stored spec = %+v", stored)
			}
			execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, out.Body.ID)
			for _, validate := range []bool{false, true} {
				_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: out.Body.ID}, Body: &resumeRequest{ValidateOnly: validate}})
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_resumable" {
					t.Fatalf("resume validateOnly=%v: %v", validate, err)
				}
			}
		})
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 2 {
		t.Fatalf("created %d Runs, want only two valid submissions", n)
	}
}

func TestSpotResumeRefusesUnnormalizedStoredPolicy(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "pool", nil)
	execSQL(t, s, context.Background(), `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('legacy', 't1', 'pool', '{"spot":true,"resumePolicy":"auto"}', 'running')`)
	for _, validate := range []bool{false, true} {
		_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "legacy"}, Body: &resumeRequest{ValidateOnly: validate}})
		var he *HTTPError
		if !errors.As(err, &he) || he.Code != "not_resumable" || he.Message != neverResumableReason {
			t.Fatalf("resume validateOnly=%v: %v", validate, err)
		}
	}
}

func TestSpotSchedulerFailsClosed(t *testing.T) {
	s, _, _ := planningFixture(t, 1)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'submitted', spec = spec || '{"spot":true,"resumePolicy":"never"}' WHERE id = 'r0'`)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 10, Memory: 100, Disk: 100, Runs: 10}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapSpotReclaim})
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r0'`, nil, &state, &reason)
	if state != StateSubmitted || reason != "waiting for spot capacity: 1 host has spare capacity free for under 60s" {
		t.Fatalf("spot = %s/%q", state, reason)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 0 {
		t.Fatalf("unsafe spot placements = %d", n)
	}
}

// 25 spot Runs ahead of a normal one in the queue, on a host that could
// take any of them (spot-capable, its space free for long enough): the
// normal Run, read beyond the scheduler's first 20-Run batch, gets it.
func TestSpotQueueDoesNotHideNormalBeyondFirstBatch(t *testing.T) {
	s, _, _ := planningFixture(t, 25)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'submitted', updated_at = now() - interval '1 hour', spec = spec || '{"spot":true,"resumePolicy":"never"}'`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('normal', 't1', 'pool1', '{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'resuming')`)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 1, Memory: 10, Disk: 10, Runs: 1}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapSpotReclaim})
	seedAged(t, s, "h", freeVec{1, 10, 10, 1}, 10*time.Minute)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id = 'normal'`); state != StateScheduled {
		t.Fatalf("normal resume behind 25 spot Runs = %s", state)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 1 {
		t.Fatalf("placements = %d, want only normal resume", n)
	}
	if reason := queryOne[string](t, s, `SELECT state_reason FROM runs WHERE id = 'r0'`); reason != "waiting for spot capacity: normal Runs are waiting for that capacity" {
		t.Fatalf("first spot Run's wait %q: want it held back for the normal Run", reason)
	}
}

func TestSpotDemandDoesNotLaunchOrProtectIdleHosts(t *testing.T) {
	s, pl, p := planningFixture(t, 3)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET spec = spec || '{"spot":true,"resumePolicy":"never"}'`)
	for range 2 {
		planningTick(t, s, pl, p, false)
	}
	if p.calls != 0 {
		t.Fatalf("spot demand launched %d hosts", p.calls)
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		waiting, _, err := waitingRuns(ctx, tx, pl)
		if err != nil {
			return err
		}
		if len(waiting) != 0 {
			t.Fatalf("spot planner demand = %d", len(waiting))
		}
		var st poolState
		if err := s.poolState(ctx, tx, pl, &st); err != nil {
			return err
		}
		if st.demand != 0 {
			t.Fatalf("spot scale demand = %d", st.demand)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.ScaleDownAfter = time.Second
	observePlanningHost(t, s, "idle", "ready", proto.Capacity{CPUs: 10, Memory: 100, Disk: 100, Runs: 10}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() - interval '1 hour' WHERE id = 'idle'`)
	p.instances = map[string]Instance{"idle": {State: "running", Tags: s.poolTags(pl)}}
	planningTick(t, s, pl, p, false)
	if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'idle'`); state != "draining" && state != "terminated" {
		t.Fatalf("spot demand protected idle host: %s", state)
	}
	execSQL(t, s, ctx, `UPDATE runs SET spec = spec - 'spot' WHERE id = 'r0'`)
	planningTick(t, s, pl, p, false)
	if p.calls != 1 {
		t.Fatalf("normal demand launches = %d, want 1", p.calls)
	}
}
