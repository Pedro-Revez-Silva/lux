package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// scaleDownFixture is pool1 (ec2, min 0, no warm) with one ready host h,
// registered an hour ago, holding one running placement of Run "live";
// spot says whether that Run is a spot Run.
func scaleDownFixture(t *testing.T, spot bool) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s, pl, p := planningFixture(t, 0)
	ctx := context.Background()
	s.cfg.ScaleDownAfter = time.Second
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() - interval '1 hour' WHERE id = 'h'`)
	sp := `{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}`
	if spot {
		sp = `{"spot":true,"resumePolicy":"never","resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}`
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES ('live', 't1', 'pool1', $1, 'running', 1)`, sp)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at)
		VALUES ('p', 't1', 'live', 'h', 1, 'running', '{"cpus":1,"memory":10,"disk":10}', now() - interval '1 hour')`)
	p.instances = map[string]Instance{"h": {State: "running", Tags: s.poolTags(pl)}}
	return s, pl, p
}

// A host whose only live placement is spot is idle: scale-down drains it
// and reclaims the spot placement (stop reason spot-reclaim, not a drain
// stop that would resume it). A normal placement keeps its host.
func TestScaleDownTreatsSpotOnlyHostAsIdle(t *testing.T) {
	for _, spot := range []bool{true, false} {
		t.Run(map[bool]string{true: "spot", false: "normal"}[spot], func(t *testing.T) {
			s, pl, p := scaleDownFixture(t, spot)
			planningTick(t, s, pl, p, false)
			state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h'`)
			stop := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id = 'p'`)
			if !spot {
				if state != "ready" || stop != "" {
					t.Fatalf("normal: host %s, stop %q; want ready and untouched", state, stop)
				}
				return
			}
			if state != "draining" || stop != stopSpotReclaim {
				t.Fatalf("spot-only: host %s, stop %q; want draining and %s", state, stop, stopSpotReclaim)
			}
			var typ string
			systemScan(t, s, `SELECT type FROM host_messages WHERE run_id = 'live' ORDER BY id DESC LIMIT 1`, nil, &typ)
			if typ != proto.MsgStop {
				t.Fatalf("message %s, want %s", typ, proto.MsgStop)
			}
			// The runner reports the exit: the Run ends terminated with the
			// reclaim's reason, never resumed; then the host is released.
			code := 143
			f := proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "live", Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})}
			if got := s.handleReport(context.Background(), "h", f); got.Type != proto.MsgAck {
				t.Fatalf("exit report: %s %s", got.Type, got.Data)
			}
			var runState, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'live'`, nil, &runState, &reason)
			if runState != StateTerminated || reason != spotScaleDownReason {
				t.Fatalf("spot Run %s %q", runState, reason)
			}
			planningTick(t, s, pl, p, false)
			if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h'`); state != "terminated" {
				t.Fatalf("host after the reclaim: %s", state)
			}
		})
	}
}

// Discovery can race a new spot placement. While scale-down waits for its
// host lock, a scheduler adds one: scale-down must retry, not drain a host
// with a spot Run it never locked and will therefore never signal.
func TestScaleDownRechecksNewSpotPlacement(t *testing.T) {
	s, _, _ := scaleDownFixture(t, true)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,current_epoch) VALUES('new','t1','pool1','{"spot":true,"resumePolicy":"never"}','running',1)`)
	hold, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT set_config('lux.system','on',true)`); err != nil {
		t.Fatal(err)
	}
	if err := lockCostHost(ctx, hold, "h"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		drained, err := s.drainForScaleDown(ctx, "h")
		if drained && err == nil {
			err = errors.New("drained with a newly discovered spot Run")
		}
		done <- err
	}()
	// Wait until scale-down has locked the existing spot Run and is
	// waiting for cost-host. A NOWAIT probe proves its lock was acquired.
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err := s.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = probe.Exec(ctx, `SELECT set_config('lux.system','on',true)`)
		_, err = probe.Exec(ctx, `SELECT 1 FROM runs WHERE id='live' FOR UPDATE NOWAIT`)
		_ = probe.Rollback(ctx)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scale-down did not reach its host lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := hold.Exec(ctx, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,resources) VALUES('newp','t1','new','h',1,'running','{"cpus":1}')`); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id='h'`); state != "ready" {
		t.Fatal(state)
	}
	if drained, err := s.drainForScaleDown(ctx, "h"); err != nil || !drained {
		t.Fatalf("retry: drained=%v err=%v", drained, err)
	}
	if reason := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id='newp'`); reason != stopSpotReclaim {
		t.Fatal(reason)
	}
}

// warm_while_active over a pool's placement history: a live normal
// placement, a normal one ended within the scale-down time, and one never
// ended but created within it make the pool active; long-ended normal
// history and any spot placement (live or recently ended) do not.
func TestPoolActiveLiveAndRecentHistory(t *testing.T) {
	for _, c := range []struct {
		name   string
		spot   bool
		state  string
		ended  string // SQL for ended_at
		active bool
	}{
		{"live normal", false, "running", "NULL", true},
		{"recently ended normal", false, "exited", "now() - interval '10 seconds'", true},
		{"long-ended normal", false, "exited", "now() - interval '1 day'", false},
		{"recently ended spot", true, "exited", "now() - interval '10 seconds'", false},
		{"live spot", true, "running", "NULL", false},
		{"recent never-ended normal", false, "exited", "NULL", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, pl, _ := planningFixture(t, 0)
			ctx := context.Background()
			s.cfg.ScaleDownAfter = time.Minute
			observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
			sp := `{}`
			if c.spot {
				sp = `{"spot":true,"resumePolicy":"never"}`
			}
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES ('r', 't1', 'pool1', $1, 'running', 1)`, sp)
			// Long-ended normal history on the same host, in every case.
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES ('old', 't1', 'pool1', '{}', 'stopped', 1)`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, ended_at)
				SELECT 'old-'||g, 't1', 'old', 'h', g, 'exited', now() - interval '2 days', now() - interval '2 days' FROM generate_series(1, 20) g`)
			created := "now() - interval '10 seconds'"
			if c.name == "long-ended normal" {
				created = "now() - interval '2 days'"
			}
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, ended_at)
				VALUES ('p', 't1', 'r', 'h', 1, '`+c.state+`', `+created+`, `+c.ended+`)`)
			var st poolState
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
				t.Fatal(err)
			}
			if st.active != c.active {
				t.Fatalf("active %v, want %v", st.active, c.active)
			}
		})
	}
}

// warm_while_active: a pool whose only placements are spot is not active,
// and a spot placement's end does not restart its host's idle clock.
func TestSpotPlacementsDoNotKeepPoolActive(t *testing.T) {
	s, pl, _ := scaleDownFixture(t, true)
	ctx := context.Background()
	var st poolState
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if st.active || st.idle != 1 {
		t.Fatalf("active %v idle %d; want inactive with one idle host", st.active, st.idle)
	}
	execSQL(t, s, ctx, `UPDATE runs SET spec = spec - 'spot' WHERE id = 'live'`)
	st = poolState{}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if !st.active || st.idle != 0 {
		t.Fatalf("normal: active %v idle %d", st.active, st.idle)
	}
}
