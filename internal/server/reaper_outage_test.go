package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// No luxd recorded itself alive for an hour (all down, or Postgres): the
// host and its placements look long expired, but the gap is forgiven, so
// none is lost. A lease is never cut short (a fresh assignment's is four
// leases) nor made longer than a heartbeat now would make it.
func TestOutageIsNotHeldAgainstHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r2', 't1', '{}', 'starting', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, lease_expires_at)
		VALUES ('p2', 't1', 'r2', 'h1', 1, 'assigned', now() + interval '2 minutes')`)
	execSQL(t, s, ctx, `UPDATE reaper_alive SET at = now() - interval '1 hour'`)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' + interval '5 seconds'`)
	execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 hour' + interval '25 seconds' WHERE id = 'p1'`)

	if err := s.forgiveOutage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
	var host, p1, p2 string
	var p1Capped, p2Kept bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT h.state, a.state, b.state,
				a.lease_expires_at <= now() + interval '30 seconds', b.lease_expires_at > now() + interval '1 minute'
			FROM hosts h, placements a, placements b WHERE h.id = 'h1' AND a.id = 'p1' AND b.id = 'p2'`).Scan(&host, &p1, &p2, &p1Capped, &p2Kept)
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "ready" || p1 != "running" || p2 != "assigned" || !p1Capped || !p2Kept {
		t.Fatalf("after the outage: host %q, p1 %q (capped %v), p2 %q (kept %v); want ready, running (true), assigned (true)",
			host, p1, p1Capped, p2, p2Kept)
	}

	// The gap is forgiven once: a host that stays silent is lost a lease later.
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 minute'`)
	if err := s.forgiveOutage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM hosts WHERE id = 'h1'`).Scan(&host)
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "lost" {
		t.Fatalf("a silent host after the forgiven gap: %q, want lost", host)
	}
}

// Before any luxd has recorded itself alive (just migrated, older luxds
// still reaping), there is no gap to forgive.
func TestNoRecordNoForgiveness(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour'`)
	if err := s.forgiveOutage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
	var host string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM hosts WHERE id = 'h1'`).Scan(&host)
	}); err != nil {
		t.Fatal(err)
	}
	if host != "lost" {
		t.Fatalf("host silent an hour, no alive record: %q, want lost", host)
	}
}
