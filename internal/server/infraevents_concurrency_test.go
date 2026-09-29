package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// heldTx runs write in a transaction of its own and keeps it open until
// released: wrote is closed once write returned, done carries the
// transaction's result.
type heldTx struct {
	pid     chan int
	wrote   chan struct{}
	release chan struct{}
	done    chan error
}

func holdTx(ctx context.Context, s *Server, write func(tx pgx.Tx) error) *heldTx {
	h := &heldTx{pid: make(chan int, 1), wrote: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
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
			case <-h.release:
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
	var pid int
	select {
	case pid = <-h.pid:
	case err := <-h.done:
		t.Fatalf("transaction ended early: %v", err)
	case <-ctx.Done():
		t.Fatal("transaction never started")
	}
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

func (h *heldTx) finish(t *testing.T, ctx context.Context) {
	t.Helper()
	close(h.release)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("transaction never finished: a lock is never released")
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
	second.settle(t, ctx, s)
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
	repeat.settle(t, ctx, s)
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
