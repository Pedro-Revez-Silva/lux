package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func hostAndPlacement(t *testing.T, s *Server, ctx context.Context) (host, placement string) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT h.state, p.state FROM hosts h JOIN placements p ON p.host_id = h.id WHERE h.id = 'h1'`).Scan(&host, &placement)
	}); err != nil {
		t.Fatal(err)
	}
	return host, placement
}

func reapHeartbeats(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	if err := s.reapLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
}

// No luxd recorded itself alive for an hour (all down, or Postgres): the
// host and its placement look long expired, but nothing is lost, neither
// during the gap nor for a lease after luxd is back. Past that, a host
// that still has not reached luxd is lost as usual.
func TestGapIsNotHeldAgainstHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour'`)
	execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 hour'`)
	execSQL(t, s, ctx, `UPDATE luxd_alive SET at = now() - interval '1 hour'`)

	// A reap before any luxd is back (one stalled across the gap).
	reapHeartbeats(t, s, ctx)
	if h, p := hostAndPlacement(t, s, ctx); h != "ready" || p != "running" {
		t.Fatalf("during the gap: host %q, placement %q; want ready, running", h, p)
	}

	gap, err := s.recordAlive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gap == nil || gap.Minutes() < 59 {
		t.Fatalf("gap = %v, want about an hour", gap)
	}
	reapHeartbeats(t, s, ctx)
	if h, p := hostAndPlacement(t, s, ctx); h != "ready" || p != "running" {
		t.Fatalf("just after the gap: host %q, placement %q; want ready, running", h, p)
	}

	// A lease (and the runner's reconnect wait) since luxd came back.
	execSQL(t, s, ctx, `UPDATE luxd_alive SET resumed_at = now() - interval '1 minute'`)
	if gap, err := s.recordAlive(ctx); err != nil || gap != nil {
		t.Fatalf("recordAlive again: gap %v, err %v; want no gap", gap, err)
	}
	reapHeartbeats(t, s, ctx)
	if h, p := hostAndPlacement(t, s, ctx); h != "lost" || p != "lost" {
		t.Fatalf("a lease after the gap: host %q, placement %q; want lost, lost", h, p)
	}
}

// Before any luxd has recorded itself alive (just migrated, older luxds
// still reaping), there is no gap: reaping goes on as always.
func TestNoRecordNoGap(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour'`)
	gap, err := s.recordAlive(ctx)
	if err != nil || gap != nil {
		t.Fatalf("first record: gap %v, err %v; want no gap", gap, err)
	}
	reapHeartbeats(t, s, ctx)
	if h, _ := hostAndPlacement(t, s, ctx); h != "lost" {
		t.Fatalf("host silent an hour, no earlier record: %q, want lost", h)
	}
}
