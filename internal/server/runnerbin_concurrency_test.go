package server

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// drainIfOutdatedTx runs drainIfOutdated for hostID reporting outdated
// arm64 binaries, in its own transaction.
func drainIfOutdatedTx(s *Server, ctx context.Context, hostID string) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var later laterEvents
		if _, err := s.drainIfOutdated(ctx, tx, &later, hostID, "arm64", "old-r", "old-s"); err != nil {
			return err
		}
		return later.write()
	})
}

func outdatedDrainingCount(t *testing.T, s *Server, ctx context.Context) int {
	t.Helper()
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE draining AND $1 = ANY(drain_causes)`, causeOutdated).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// The per-pool cap on outdated-binaries drains must hold even when every
// host in the pool reports outdated binaries in the same instant (a luxd
// restart wakes every runner in a pool at once): pg_advisory_xact_lock in
// drainIfOutdated serializes the count-then-cordon against every other
// concurrent caller for the same pool, so 20 concurrent drains against a
// cap of 2 (10% of 20) cordon exactly 2, never more.
func TestDrainIfOutdatedCapHoldsUnderConcurrentHellos(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	s.cfg.OutdatedDrainPercent = 10
	ctx := context.Background()

	const n = 20
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("h%02d", i)
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state) VALUES ($1, $1, 'default', 'ready')`, ids[i])
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			errs[i] = drainIfOutdatedTx(s, ctx, id)
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("drainIfOutdated for %s: %v", ids[i], err)
		}
	}

	if draining := outdatedDrainingCount(t, s, ctx); draining != 2 {
		t.Fatalf("hosts cordoned for outdated binaries: %d, want exactly 2 (max(1, 20*10/100))", draining)
	}
}

// A terminated host still carrying the "outdated" cause (the provisioner's
// replace path terminates a drained host without ever clearing
// drain_causes or draining) must not count against the pool's cap
// forever: only a host still draining counts, so the pool's continuous
// replace-the-next-outdated-instance cycle is never stuck below its own
// cap once earlier instances are gone.
func TestDrainIfOutdatedCapIgnoresTerminatedHosts(t *testing.T) {
	s := testServer(t)
	s.bins = matchingBins()
	s.cfg.OutdatedDrainPercent = 100
	ctx := context.Background()

	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, draining, state_reason, drain_causes) VALUES
		('gone', 'gone', 'burst', 'terminated', true, $1, ARRAY[$2])`, outdatedBinariesReason, causeOutdated)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state) VALUES ('h1', 'h1', 'burst', 'ready')`)

	if err := drainIfOutdatedTx(s, ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if draining, _, _, _, _, _ := hostState(t, s, ctx, "h1"); !draining {
		t.Fatal("h1 was not drained: a terminated host still carrying the outdated cause was counted against the cap")
	}
}

// The percentage maths: max(1, live*percent/100), so a pool of 5 with a
// 10% cap still cordons 1 (not 0), and a pool of 30 with a 10% cap
// cordons 3.
func TestDrainIfOutdatedCapPercentMaths(t *testing.T) {
	cases := []struct {
		name       string
		live, want int
	}{
		{"below one percent point: at least 1", 5, 1},
		{"exact multiple", 30, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			s.bins = matchingBins()
			s.cfg.OutdatedDrainPercent = 10
			ctx := context.Background()

			for i := range c.live {
				execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state) VALUES ($1, $1, 'default', 'ready')`, fmt.Sprintf("h%02d", i))
			}
			for i := range c.live {
				if err := drainIfOutdatedTx(s, ctx, fmt.Sprintf("h%02d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if draining := outdatedDrainingCount(t, s, ctx); draining != c.want {
				t.Fatalf("hosts cordoned out of %d: %d, want %d", c.live, draining, c.want)
			}
		})
	}
}
