package server

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// pending is a Run's cost_pending row as "reason claimed_by", or "" when it
// has none.
func pending(t *testing.T, s *Server, runID string) string {
	t.Helper()
	ctx := context.Background()
	var out string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason || ' ' || coalesce(claimed_by, '-') FROM cost_pending WHERE run_id = $1`, runID).Scan(&out)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return out
}

// Leaving running queues the Run's costs in the state change's own
// transaction, whatever the scope (an API stop runs as the tenant); a live
// state doesn't. A rolled-back change queues nothing, and several changes
// merge into one row.
func TestSetRunStateQueuesCosts(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		state string
		scope store.Scope
		want  string
	}{
		{StateStarting, store.System(), ""},
		{StateRunning, store.System(), ""},
		{StateStopping, store.Tenant("t1"), "state:stopping -"},
		{StateStopped, store.System(), "state:stopped -"},
		{StateLost, store.System(), "state:lost -"},
		{StateSucceeded, store.Tenant("t1"), "state:succeeded -"},
		{StateFailed, store.System(), "state:failed -"},
		{StateCancelled, store.Tenant("t1"), "state:cancelled -"},
	} {
		execSQL(t, s, ctx, `DELETE FROM cost_pending`)
		if err := s.db.Tx(ctx, c.scope, func(tx pgx.Tx) error {
			return setRunState(ctx, tx, "t1", "r1", c.state, "", 1)
		}); err != nil {
			t.Fatal(err)
		}
		if got := pending(t, s, "r1"); got != c.want {
			t.Errorf("%s: queued %q, want %q", c.state, got, c.want)
		}
	}

	// Rolled back: nothing queued.
	execSQL(t, s, ctx, `DELETE FROM cost_pending`)
	rollback := errors.New("rollback")
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := setRunState(ctx, tx, "t1", "r1", StateFailed, "", 1); err != nil {
			return err
		}
		if got := pending(t, s, "r1"); got != "" {
			t.Errorf("visible before commit: %q", got)
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got := pending(t, s, "r1"); got != "" {
		t.Errorf("rolled back, yet queued %q", got)
	}

	// Several changes, one row: the earliest due_at stays, the last reason
	// wins, and a claim is freed (its result was read before the change).
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
		VALUES ('r1', now() - interval '1 hour', 'tick', 'other', now() + interval '1 minute')`)
	for _, st := range []string{StateStopping, StateStopped, StateCancelled} {
		if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
			return setRunState(ctx, tx, "t1", "r1", st, "", 1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var early bool
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), bool_and(due_at < now() - interval '59 minutes') FROM cost_pending`).Scan(&n, &early)
	}); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, s, "r1"); n != 1 || !early || got != "state:cancelled -" {
		t.Errorf("merged into %d rows (earliest kept %v): %q", n, early, got)
	}

	// A tenant's scope queues only its own Runs, and never reads the queue.
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return enqueueCost(ctx, tx, "r2", "state:stopped")
	}); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, s, "r2"); got != "" {
		t.Errorf("t1 queued t2's run: %q", got)
	}
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_pending`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("t1 reads %d queue rows (%v), want 0", n, err)
	}
}
