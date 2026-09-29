package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// No reaper ran for an hour (luxd down, or Postgres): the host and its
// placement look long expired, but the gap is forgiven, so neither is lost.
// Only what a heartbeat now would give is restored, never more.
func TestOutageIsNotHeldAgainstHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE reaper_alive SET at = now() - interval '1 hour'`)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' + interval '5 seconds'`)
	execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 hour' + interval '25 seconds'`)

	if err := s.forgiveOutage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapHosts(ctx); err != nil {
		t.Fatal(err)
	}
	var host, placement string
	var leaseOK bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT h.state, p.state, p.lease_expires_at <= now() + interval '30 seconds'
			FROM hosts h JOIN placements p ON p.host_id = h.id WHERE h.id = 'h1'`).Scan(&host, &placement, &leaseOK)
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "ready" || placement != "running" || !leaseOK {
		t.Fatalf("after the outage: host %q, placement %q, lease within one lease: %v; want ready, running, true", host, placement, leaseOK)
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
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	if host != "lost" {
		t.Fatalf("a silent host after the forgiven gap: %q, want lost", host)
	}
}
