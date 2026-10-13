package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// reclaimFixture: host h (2 cpus) fully used by running spot Run "spot"
// (placement p), and queued normal Run "n" needing the whole host; one
// scheduler pass claims p for n and signals it.
func reclaimFixture(t *testing.T) *Server {
	t.Helper()
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES
		('spot', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}', 'running', 1),
		('n', 't1', 'pool1', '{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}', 'submitted', 0)`)
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, lease_expires_at)
			VALUES ('p', 't1', 'spot', 'h', 1, 'running', '{"cpus":2,"memory":20,"disk":20}', now() + interval '1 hour')`)
		return err
	})
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[string](t, s, `SELECT coalesce(reclaim_for, '') || '/' || stop_reason FROM placements WHERE id = 'p'`); got != "n/"+stopSpotReclaim {
		t.Fatalf("victim claim/stop %q", got)
	}
	return s
}

func exitReport(t *testing.T, s *Server, host, run string, epoch int) {
	t.Helper()
	code := 143
	f := proto.Frame{Type: proto.MsgStatus, ID: 7, RunID: run, Epoch: epoch,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})}
	if got := s.handleReport(context.Background(), host, f); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s %s", got.Type, got.Data)
	}
}

// A reclaimed victim whose disk cannot be released yet reports
// disk.release_failed: luxd records it on the Run, where its events list
// it, and the placement stays live (stopping) until the end report.
func TestReclaimDiskReleaseFailedEvent(t *testing.T) {
	s := reclaimFixture(t)
	ctx := context.Background()
	f := s.handleReport(ctx, "h", proto.Frame{Type: proto.MsgRunEvent, ID: 9, RunID: "spot", Epoch: 1,
		Data: proto.Marshal(proto.RunEvent{Type: proto.EvDiskReleaseFailed, Data: map[string]any{"error": "volume is busy"}})})
	if f.Type != proto.MsgAck {
		t.Fatalf("%s report: %s %s", proto.EvDiskReleaseFailed, f.Type, f.Data)
	}
	events, err := s.events(ctx, "t1", "spot", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for _, e := range events {
		if e.Type == proto.EvDiskReleaseFailed {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Data["error"] != "volume is busy" || got[0].Epoch == nil || *got[0].Epoch != 1 {
		t.Fatalf("%s events %+v", proto.EvDiskReleaseFailed, got)
	}
	if st := queryOne[string](t, s, `SELECT state FROM placements WHERE id = 'p'`); st != "running" {
		t.Fatalf("placement %s after the event, want it unchanged", st)
	}
}

// The requester is terminated while its victim drains: the pledge goes on
// the next pass, but the victim stays claimed and signalled (irreversible)
// and still ends terminated with the reclaim's reason.
func TestReclaimRequesterCancelledWhileDraining(t *testing.T) {
	s := reclaimFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'terminated' WHERE id = 'n'`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatalf("pledge of a cancelled requester kept: %d", n)
	}
	if got := queryOne[string](t, s, `SELECT coalesce(reclaim_for, '') || '/' || stop_reason FROM placements WHERE id = 'p'`); got != "n/"+stopSpotReclaim {
		t.Fatalf("victim after cancel: %q (a notice cannot be withdrawn)", got)
	}
	exitReport(t, s, "h", "spot", 1)
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'spot'`, nil, &state, &reason)
	if state != StateTerminated || reason != spotReclaimedReason {
		t.Fatalf("victim Run %s %q", state, reason)
	}
}

// The victim exits by itself (it finished) before its notice is acted on:
// the requester starts as soon as the release lands, and the victim's own
// outcome is not rewritten.
func TestReclaimVictimExitsEarly(t *testing.T) {
	s := reclaimFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE placements SET stop_reason = '' WHERE id = 'p'`) // as if exiting before the stop landed
	code := 0
	f := proto.Frame{Type: proto.MsgStatus, ID: 8, RunID: "spot", Epoch: 1, Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code})}
	if got := s.handleReport(ctx, "h", f); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s", got.Type)
	}
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id = 'n'`); state != StateScheduled {
		t.Fatalf("requester after early exit: %s", state)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatalf("pledge left after placement")
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'spot'`, nil, &state, &reason)
	if state != StateTerminated || reason != "succeeded; resumePolicy never" {
		t.Fatalf("victim's own outcome rewritten: %s %q", state, reason)
	}
}

// A requester with two claimed victims (a, b) does not start while b is
// still live, even when a and an unrelated normal Run c have released
// enough for it to fit; once b releases, it starts.
func TestReclaimRequesterWaitsForAllClaimedVictims(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 8, Memory: 40, Runs: 8}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES
		('a', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":4,"memory":5},"placement":{"pool":"burst"}}', 'running', 1),
		('b', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":1,"memory":15},"placement":{"pool":"burst"}}', 'running', 1),
		('c', 't1', 'pool1', '{"resources":{"cpus":2,"memory":20},"placement":{"pool":"burst"}}', 'running', 1),
		('n', 't1', 'pool1', '{"resources":{"cpus":6,"memory":20},"placement":{"pool":"burst"}}', 'submitted', 0)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES
		('pa', 't1', 'a', 'h', 1, 'running', '{"cpus":4,"memory":5}'),
		('pb', 't1', 'b', 'h', 1, 'running', '{"cpus":1,"memory":15}'),
		('pc', 't1', 'c', 'h', 1, 'running', '{"cpus":2,"memory":20}')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE reclaim_for = 'n'`); got != 2 {
		t.Fatalf("claimed victims: %d, want 2", got)
	}
	exitReport(t, s, "h", "a", 1)
	exitReport(t, s, "h", "c", 1)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'n'`, nil, &state, &reason)
	if state == StateScheduled || reason != reclaimWaitReason || queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id = 'n'`) != 0 {
		t.Fatalf("requester with victim b live: %s %q (pb %s)", state, reason, queryOne[string](t, s, `SELECT state FROM placements WHERE id = 'pb'`))
	}
	exitReport(t, s, "h", "b", 1)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id = 'n'`); state != StateScheduled {
		t.Fatalf("requester after every victim released: %s", state)
	}
}

// A runner hello (restart) or the host's loss voids the pledge: its spot
// placements may be gone and its capacity starts over. A scheduler pass
// (cleanupPledges, then the batch) drops it, with no Run queued for that
// host's pool any more, too.
func TestReclaimPledgeVoidedByHostChange(t *testing.T) {
	for _, change := range []string{
		`UPDATE hosts SET capacity_epoch = capacity_epoch + 1 WHERE id = 'h'`,
		`UPDATE hosts SET state = 'lost' WHERE id = 'h'`,
		`UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'h'`,
	} {
		t.Run(change, func(t *testing.T) {
			s := reclaimFixture(t)
			execSQL(t, s, context.Background(), change)
			if err := s.scheduleOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
				t.Fatalf("pledge survived %s", change)
			}
		})
	}
	// The requester's row is held elsewhere, so no batch takes it up (SKIP
	// LOCKED) and discovers its host: cleanupPledges alone drops the pledge.
	t.Run("no batch reaches the host", func(t *testing.T) {
		s := reclaimFixture(t)
		ctx := context.Background()
		execSQL(t, s, ctx, `UPDATE hosts SET capacity_epoch = capacity_epoch + 1 WHERE id = 'h'`)
		hold, err := s.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'n' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		if err := s.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
			t.Fatal("pledge survived a hello with no batch for its host")
		}
	})
	t.Run("dropPledges directly", func(t *testing.T) {
		s := reclaimFixture(t)
		execSQL(t, s, context.Background(), `UPDATE hosts SET capacity_epoch = capacity_epoch + 1 WHERE id = 'h'`)
		if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
			return s.dropPledges(context.Background(), tx, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
			t.Fatal("pledge survived a hello")
		}
	})
}

// A host whose runner lacks the spot capability never gets a spot
// placement, however long its capacity has been free.
func TestSpotNeverPlacedOnOldRunner(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapDiff, proto.CapSyncModes})
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 10*time.Minute)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('s', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}', 'submitted')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 's'`, nil, &state, &reason)
	if state != StateSubmitted || reason != "waiting for spot capacity: 1 host lacks spot support" {
		t.Fatalf("spot on an old runner: %s %q", state, reason)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapSpotReclaim})
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id = 's'`); state != StateScheduled {
		t.Fatalf("with the capability: %s", state)
	}
}

// A victim row another transaction holds (the victim's own exit being
// recorded, say) is skipped, not waited for: the pledge is abandoned for
// this pass, nothing is claimed, and the scheduler does not block.
func TestReclaimSkipsLockedVictim(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES
		('spot', 't1', 'pool1', '{"spot":true,"resumePolicy":"never","resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}', 'running', 1),
		('n', 't1', 'pool1', '{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}', 'submitted', 0)`)
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
			VALUES ('p', 't1', 'spot', 'h', 1, 'running', '{"cpus":2,"memory":20,"disk":20}')`)
		return err
	})
	hold, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM placements WHERE id = 'p' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.scheduleOnce(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler blocked on a locked victim row")
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatalf("pledged with an unclaimed victim")
	}
	_ = hold.Rollback(ctx)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[string](t, s, `SELECT coalesce(reclaim_for, '') FROM placements WHERE id = 'p'`); got != "n" {
		t.Fatalf("victim not claimed once free: %q", got)
	}
}
