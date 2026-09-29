package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// heldTx runs write in a transaction of its own and keeps it open until
// released (with what else to do in it before it commits): wrote is closed
// once write returned, done carries the transaction's result.
type heldTx struct {
	pid     chan int
	wrote   chan struct{}
	release chan func(pgx.Tx) error
	done    chan error
	// backend: its pid, once settle has read it.
	backend int
}

func holdTx(ctx context.Context, s *Server, write func(tx pgx.Tx) error) *heldTx {
	h := &heldTx{pid: make(chan int, 1), wrote: make(chan struct{}), release: make(chan func(pgx.Tx) error, 1), done: make(chan error, 1)}
	go func() {
		h.done <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			h.pid <- pid
			if err := write(tx); err != nil {
				return err
			}
			close(h.wrote)
			select {
			case then := <-h.release:
				if then != nil {
					return then(tx)
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	return h
}

// settle waits until h has either written or is blocked on a lock, and
// reports whether it wrote.
func (h *heldTx) settle(t *testing.T, ctx context.Context, s *Server) bool {
	t.Helper()
	if h.backend == 0 {
		select {
		case h.backend = <-h.pid:
		case err := <-h.done:
			t.Fatalf("transaction ended early: %v", err)
		case <-ctx.Done():
			t.Fatal("transaction never started")
		}
	}
	pid := h.backend
	for {
		select {
		case <-h.wrote:
			return true
		case err := <-h.done:
			t.Fatalf("transaction ended early: %v", err)
		default:
		}
		var waiting bool
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`, pid).Scan(&waiting)
		}); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return false
		}
		if ctx.Err() != nil {
			t.Fatal("neither wrote nor waited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitsOnStream fails unless h is blocked, and blocked on the event-stream
// advisory lock of owner in tbl (lockStream's key), not on some other lock.
func (h *heldTx) waitsOnStream(t *testing.T, ctx context.Context, s *Server, tbl eventTable, owner string) {
	t.Helper()
	h.waitsOnAdvisory(t, ctx, s, tbl.table+":"+owner)
}

// waitsOnAdvisory fails unless h is blocked on the advisory lock keyed
// hashtextextended(key, 0).
func (h *heldTx) waitsOnAdvisory(t *testing.T, ctx context.Context, s *Server, key string) {
	t.Helper()
	if h.settle(t, ctx, s) {
		t.Fatalf("wrote without waiting for the lock %q", key)
	}
	var onLock bool
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `WITH k AS (SELECT hashtextextended($2, 0) AS key)
			SELECT EXISTS (SELECT 1 FROM pg_locks l, k WHERE l.pid = $1 AND NOT l.granted AND l.locktype = 'advisory'
				AND l.classid = ((k.key >> 32) & 4294967295)::oid AND l.objid = (k.key & 4294967295)::oid AND l.objsubid = 1)`,
			h.backend, key).Scan(&onLock)
	}); err != nil {
		t.Fatal(err)
	}
	if !onLock {
		t.Fatalf("blocked, but not on the lock %q", key)
	}
}

func (h *heldTx) finish(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := h.finishWith(t, ctx, nil); err != nil {
		t.Fatal(err)
	}
}

// finishWith runs then in the held transaction, commits it, and returns
// its result.
func (h *heldTx) finishWith(t *testing.T, ctx context.Context, then func(pgx.Tx) error) error {
	t.Helper()
	h.release <- then
	select {
	case err := <-h.done:
		return err
	case <-ctx.Done():
		t.Fatal("transaction never finished: a lock is never released")
		return nil
	}
}

func failure(ctx context.Context, tx pgx.Tx) error {
	return poolRepeatEvent(ctx, tx, "pool1", evLaunchFailed, map[string]any{"host": "hx", "error": "InsufficientInstanceCapacity"}, "host")
}

// Two writers recording the same failure at once (a provisioner lease
// changing hands mid provider call) fold into one row: the second waits
// for the first's to commit, then counts it.
func TestConcurrentIdenticalFailuresFoldIntoOne(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	infraFixture(t, s, ctx)
	first := holdTx(ctx, s, func(tx pgx.Tx) error { return failure(ctx, tx) })
	if !first.settle(t, ctx, s) {
		t.Fatal("the first failure waited on nothing")
	}
	second := holdTx(ctx, s, func(tx pgx.Tx) error { return failure(ctx, tx) })
	second.waitsOnStream(t, ctx, s, poolEvents, "pool1")
	first.finish(t, ctx)
	second.finish(t, ctx)
	if evs := events(t, s, evLaunchFailed); len(evs) != 1 || evs[0].Count != 2 {
		t.Fatalf("launch_failed events %+v, want one with count 2", evs)
	}
}

// A launch that commits while a repeat of an earlier failure is being
// recorded breaks the run of failures: the repeat starts a new row after
// it, rather than folding across it.
func TestFailureDoesNotFoldAcrossAConcurrentLaunch(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	infraFixture(t, s, ctx)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return failure(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	launched := holdTx(ctx, s, func(tx pgx.Tx) error {
		return poolEvent(ctx, tx, "pool1", evHostLaunched, map[string]any{"host": "h9", "providerId": "i-9"})
	})
	if !launched.settle(t, ctx, s) {
		t.Fatal("the launch waited on nothing")
	}
	repeat := holdTx(ctx, s, func(tx pgx.Tx) error { return failure(ctx, tx) })
	repeat.waitsOnStream(t, ctx, s, poolEvents, "pool1")
	launched.finish(t, ctx)
	repeat.finish(t, ctx)
	failed := events(t, s, evLaunchFailed)
	if len(failed) != 2 || failed[0].Count != 1 || failed[1].Count != 1 {
		t.Fatalf("launch_failed events %+v, want two, one each side of the launch", failed)
	}
	order := queryOne[[]string](t, s, `SELECT array_agg(type ORDER BY id) FROM pool_events WHERE pool_id = 'pool1'`)
	if len(order) != 3 || order[1] != evHostLaunched {
		t.Fatalf("pool events %v, want failure, launch, failure", order)
	}
}

// A fold decides from the owner's latest events, whatever its history: on
// a pool with 100k provider errors behind it (every one a retry-loop
// event), recording a launch failure reads a handful of rows, not the
// history (as Postgres counts them for this transaction).
func TestFoldReadsABoundedNumberOfEvents(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	ownerExec(t, s, `INSERT INTO pool_events (tenant_id, pool_id, type, data)
		SELECT 't1', 'pool1', 'pool.provider_error', jsonb_build_object('op', 'list', 'error', 'error ' || i)
		FROM generate_series(1, 100000) i`)
	ownerExec(t, s, `ANALYZE pool_events`)
	var read int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tuples := func() (n int64, err error) {
			err = tx.QueryRow(ctx, `SELECT coalesce(seq_tup_read, 0) + coalesce(idx_tup_fetch, 0)
				FROM pg_stat_xact_user_tables WHERE relname = 'pool_events'`).Scan(&n)
			return n, err
		}
		before, err := tuples()
		if err != nil {
			return err
		}
		if err := failure(ctx, tx); err != nil {
			return err
		}
		after, err := tuples()
		read = after - before
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if read > 2*foldWindow {
		t.Fatalf("recording a failure read %d pool events, want at most %d", read, 2*foldWindow)
	}
	// And the decision is still right: that failure was a new row; its
	// repeats fold into it.
	for range 2 {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return failure(ctx, tx) }); err != nil {
			t.Fatal(err)
		}
	}
	if n := queryOne[int](t, s, `SELECT count FROM pool_events WHERE pool_id = 'pool1' ORDER BY id DESC LIMIT 1`); n != 3 {
		t.Fatalf("latest failure counted %d, want 3", n)
	}
}

// Past the window, a repeat is a new row rather than a fold across
// events the lookup does not read.
func TestFoldLooksNoFurtherThanItsWindow(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return failure(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	for i := range foldWindow {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return poolEvent(ctx, tx, "pool1", evPoolProviderErr, map[string]any{"op": "list", "error": i})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return failure(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	if evs := events(t, s, evLaunchFailed); len(evs) != 2 {
		t.Fatalf("launch_failed events %+v, want two", evs)
	}
}

// A pool removed while one of its hosts registers: the removal locks the
// pool, then drains its hosts; the registration locks its host, then
// writes its pool's event, whose foreign key locks the pool KEY SHARE. The
// removal's pool lock must not conflict with that, or each waits for the
// other (a deadlock Postgres breaks by failing one).
func TestPoolRemovalAndHostRegistrationDoNotDeadlock(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("forceEvict=%v", force), func(t *testing.T) {
			s := testServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			infraFixture(t, s, ctx)
			// registerHost's order: the host row, later its pool's event.
			reg := holdTx(ctx, s, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT 1 FROM hosts WHERE id = 'h1' FOR NO KEY UPDATE`)
				return err
			})
			if !reg.settle(t, ctx, s) {
				t.Fatal("the registration waited on nothing")
			}
			admin := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
			removed := make(chan error, 1)
			go func() {
				_, err := s.deletePool(admin, &deletePoolInput{Name: "burst", ForceEvict: force})
				removed <- err
			}()
			// The removal now holds the pool and waits for the host.
			waitForRunWaiter(t, s, ctx, "")
			if err := reg.finishWith(t, ctx, func(tx pgx.Tx) error {
				return hostPoolEvent(ctx, tx, "h1", evHostRegistered, map[string]any{"host": "h1"})
			}); err != nil {
				t.Fatalf("registration: %v", err)
			}
			select {
			case err := <-removed:
				if err != nil {
					t.Fatalf("removal: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("the removal never finished")
			}
			if evs := events(t, s, evHostRegistered); len(evs) != 1 {
				t.Fatalf("host_registered events %+v, want one", evs)
			}
			if !queryOne[bool](t, s, `SELECT draining FROM hosts WHERE id = 'h1'`) {
				t.Fatal("the removed pool's host is not draining")
			}
		})
	}
}

// A pool edit in flight does not hold up its hosts' events: the edit's
// pool lock leaves the event's foreign-key check alone.
func TestPoolEditDoesNotBlockItsHostsEvents(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	infraFixture(t, s, ctx)
	edit := holdTx(ctx, s, func(tx pgx.Tx) error {
		return ChangePool(ctx, tx, new("t1"), "burst", func() error {
			_, err := tx.Exec(ctx, `UPDATE pools SET max_hosts = 5 WHERE id = 'pool1'`)
			return err
		})
	})
	if !edit.settle(t, ctx, s) {
		t.Fatal("the edit waited on nothing")
	}
	reg := holdTx(ctx, s, func(tx pgx.Tx) error {
		return hostPoolEvent(ctx, tx, "h1", evHostRegistered, map[string]any{"host": "h1"})
	})
	wrote := reg.settle(t, ctx, s)
	edit.finish(t, ctx)
	reg.finish(t, ctx)
	if !wrote {
		t.Fatal("a host's pool event waited for an edit of its pool")
	}
}

// Two first writes of one pool at once, with different settings: one
// creates it, the other changes it, and its change says from what to what.
func TestConcurrentFirstPoolWritesCreateOnce(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	infraFixture(t, s, ctx)
	set := func(tx pgx.Tx, maxHosts int) error {
		return ChangePool(ctx, tx, new("t1"), "fresh", func() error {
			_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, max_hosts) VALUES ($1, 't1', 'fresh', 'ec2', $2)
				ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET max_hosts = EXCLUDED.max_hosts`, ids.New(ids.Pool), maxHosts)
			return err
		})
	}
	first := holdTx(ctx, s, func(tx pgx.Tx) error { return set(tx, 3) })
	if !first.settle(t, ctx, s) {
		t.Fatal("the first write waited on nothing")
	}
	second := holdTx(ctx, s, func(tx pgx.Tx) error { return set(tx, 5) })
	second.waitsOnAdvisory(t, ctx, s, "pool-name:t1/fresh")
	first.finish(t, ctx)
	second.finish(t, ctx)
	evs := events(t, s, evConfigChanged)
	if len(evs) != 2 || evs[0].Data["created"] != true || evs[1].Data["created"] != false {
		t.Fatalf("config_changed events %+v, want one created, then one change", evs)
	}
	want := map[string]any{"maxHosts": map[string]any{"old": 3.0, "new": 5.0}}
	if got := evs[1].Data["changes"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("second write's changes %v, want %v", got, want)
	}
}
