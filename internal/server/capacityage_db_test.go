package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// hostKey is host id's capacity-history key as the scheduler computes it.
func ageKeyOf(t *testing.T, s *Server, id string) string {
	t.Helper()
	var key string
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		hs, err := s.candidateHosts(context.Background(), tx, []string{id})
		if err != nil || len(hs) != 1 {
			return errors.Join(err, errors.New("host not a candidate"))
		}
		key = ageKey(hs[0])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// seedAged writes host id's history: free has been free since ago.
func seedAged(t *testing.T, s *Server, id string, free freeVec, ago time.Duration) {
	t.Helper()
	c := capacityAge{}
	c.observe(ageKeyOf(t, s, id), time.Now().Add(-ago), free)
	execSQL(t, s, context.Background(), `INSERT INTO host_capacity_age (host_id, key, history) VALUES ($1, $2, $3)
		ON CONFLICT (host_id) DO UPDATE SET key = excluded.key, history = excluded.history`, id, c.Key, c.marshal())
}

// agedOn is what host id has had continuously free for over 60s, as the
// scheduler reads it now.
func agedOn(t *testing.T, s *Server, id string) freeVec {
	t.Helper()
	var v freeVec
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		hs, err := s.candidateHosts(context.Background(), tx, []string{id})
		if err != nil {
			return err
		}
		v = hs[0].age.aged(time.Now())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// withHostLock runs f in a transaction holding id's cost-host lock, as
// every luxd writer of placements does.
func withHostLock(t *testing.T, s *Server, id string, f func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := lockCostHost(ctx, tx, id); err != nil {
			return err
		}
		return f(tx)
	}); err != nil {
		t.Fatal(err)
	}
}

// A placement's allocation and release, made by a lock-holding writer
// between scheduler passes, land in the durable history: the old 1-cpu gap
// stays aged, the freshly freed 3 cpus do not.
func TestCapacityAgeDurableTransitions(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'running')`)
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
			VALUES ('p', 't1', 'r', 'h', 1, 'running', '{"cpus":3,"memory":30,"disk":30}')`)
		return err
	})
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE placements SET state = 'exited' WHERE id = 'p'`)
		return err
	})
	if aged := agedOn(t, s, "h"); !aged.covers(1, 10, 10) || aged.covers(2, 20, 20) {
		t.Fatalf("aged %+v: want the 1-cpu gap only", aged)
	}
	// A runner hello bumps the epoch: the history no longer applies.
	execSQL(t, s, ctx, `UPDATE hosts SET capacity_epoch = capacity_epoch + 1 WHERE id = 'h'`)
	if aged := agedOn(t, s, "h"); aged.covers(1, 10, 10) {
		t.Fatalf("after a hello: aged %+v", aged)
	}
}

// A write without the host's lock cannot be ordered with a scheduler's
// read: the trigger drops the history (conservative) rather than lock.
func TestCapacityAgeUnlockedWriterForgetsHistory(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'running')`)
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		VALUES ('p', 't1', 'r', 'h', 1, 'running', '{"cpus":1}')`)
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_capacity_age`); n != 0 {
		t.Fatalf("history kept after an unlocked write")
	}
}

// Two luxd processes (distinct ids) share one history: the second's pass
// neither resets nor loses what the first recorded, and a restarted one
// carries on from it.
func TestCapacityAgeSharedAcrossLuxd(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
	other := New(s.cfg, s.db, nil, s.log)
	other.id = s.id + "-other"
	other.hub.polled("h")
	for _, srv := range []*Server{other, s} {
		if err := srv.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if aged := agedOn(t, srv, "h"); !aged.covers(4, 40, 40) {
			t.Fatalf("after luxd %s's pass: aged %+v", srv.id, aged)
		}
	}
}

// The lock order holds when a scheduler pass that places Runs (and so
// fires the trigger under its host locks) races the other placement
// writers on the same host: status reports (running, stopping, exited),
// requestStop and the lease reaper's lost placements. All finish: no
// deadlock (Postgres would abort one with 40P01) and no error.
func TestCapacityAgeTriggerUnderConcurrentWriters(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 64, Memory: 640, Disk: 640, Runs: 64}, map[string]string{})
	seedAged(t, s, "h", freeVec{64, 640, 640, 64}, 2*time.Minute)
	const n = 8
	for i := range n {
		id := "live" + string(rune('a'+i))
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES ($1, 't1', 'pool1', '{"placement":{"pool":"burst"}}', 'running', 1)`, id)
		withHostLock(t, s, "h", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, lease_expires_at)
				VALUES ($1, 't1', $2, 'h', 1, 'running', '{"cpus":1,"memory":1,"disk":1}', now() + interval '1 hour')`, "p"+id, id)
			return err
		})
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES ($1, 't1', 'pool1', '{"resources":{"cpus":1,"memory":1,"disk":1},"placement":{"pool":"burst"}}', 'submitted')`, "q"+id)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4*n)
	for i := range n {
		id := "live" + string(rune('a'+i))
		wg.Add(3)
		go func() { defer wg.Done(); errs <- s.scheduleOnce(ctx) }()
		go func() {
			defer wg.Done()
			// stopping first: a transition within the live states, made
			// without the host lock, which must not touch the history.
			stopping := proto.Frame{Type: proto.MsgStatus, ID: int64(i + 100), RunID: id, Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "stopping"})}
			if got := s.handleReport(ctx, "h", stopping); got.Type != proto.MsgAck {
				errs <- errors.New("report: " + got.Type + " " + string(got.Data))
			}
			code := 0
			f := proto.Frame{Type: proto.MsgStatus, ID: int64(i + 10), RunID: id, Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code})}
			if got := s.handleReport(ctx, "h", f); got.Type != proto.MsgAck {
				errs <- errors.New("report: " + got.Type + " " + string(got.Data))
			}
		}()
		go func() {
			defer wg.Done()
			errs <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, id); err != nil {
					return err
				}
				_, err := s.requestStop(ctx, tx, "t1", id, "stop")
				return err
			})
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- s.reapLeases(ctx) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_capacity_age`); n != 1 {
		t.Fatalf("history rows %d: a locked writer dropped it", n)
	}
}

// seedHistory writes host id's history as entries, under its current key.
func seedHistory(t *testing.T, s *Server, id string, entries ...ageEntry) {
	t.Helper()
	c := capacityAge{Key: ageKeyOf(t, s, id), History: entries}
	execSQL(t, s, context.Background(), `INSERT INTO host_capacity_age (host_id, key, history) VALUES ($1, $2, $3)
		ON CONFLICT (host_id) DO UPDATE SET key = excluded.key, history = excluded.history`, id, c.Key, c.marshal())
}

// The trigger's own overflow merge keeps the component-wise minimum: an
// old 1-cpu hole, then more than 32 locked transitions within 60s (a
// 3-cpu placement made and removed, free alternating 5 and 8 cpus), and
// the hole still bounds what is aged.
func TestCapacityAgeTriggerOverflowKeepsMinimum(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 8, Memory: 80, Disk: 80, Runs: 8}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'running')`)
	now := time.Now()
	seedHistory(t, s, "h", ageEntry{now.Add(-2 * time.Minute), freeVec{1, 10, 10, 1}}, ageEntry{now.Add(-50 * time.Second), freeVec{8, 80, 80, 8}})
	for i := range 20 {
		withHostLock(t, s, "h", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
				VALUES ($1, 't1', 'r', 'h', $2, 'running', '{"cpus":3,"memory":30,"disk":30}')`, "p"+string(rune('a'+i)), i+1)
			return err
		})
		withHostLock(t, s, "h", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM placements WHERE id = $1`, "p"+string(rune('a'+i)))
			return err
		})
	}
	if n := queryOne[int](t, s, `SELECT jsonb_array_length(history) FROM host_capacity_age WHERE host_id = 'h'`); n != maxAgeHistory {
		t.Fatalf("history of %d entries, want merged to %d", n, maxAgeHistory)
	}
	if aged := agedOn(t, s, "h"); aged.covers(2, 20, 20) || !aged.covers(1, 10, 10) {
		t.Fatalf("aged %+v: want only the old 1-cpu hole", aged)
	}
}

// A transition within the live states (running to stopping) made without
// the host's lock, as status reports make it, neither rewrites nor drops
// the history: its old gap stays aged.
func TestCapacityAgeLiveTransitionKeepsHistory(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'running')`)
	seedAged(t, s, "h", freeVec{4, 40, 40, 4}, 2*time.Minute)
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
			VALUES ('p', 't1', 'r', 'h', 1, 'running', '{"cpus":1,"memory":10,"disk":10}')`)
		return err
	})
	before := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id = 'h'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'stopping', stop_reason = 'stop' WHERE id = 'p'`)
	if after := queryOne[string](t, s, `SELECT coalesce((SELECT history::text FROM host_capacity_age WHERE host_id = 'h'), '<dropped>')`); after != before {
		t.Fatalf("history after running->stopping:\n%s\nwant unchanged:\n%s", after, before)
	}
	if aged := agedOn(t, s, "h"); !aged.covers(3, 30, 30) {
		t.Fatalf("aged %+v: want the old 3-cpu gap", aged)
	}
}
