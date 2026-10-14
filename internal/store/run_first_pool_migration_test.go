package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 078 and 079: an ended Run started before them gets first_pool_id from the
// host of its first running placement (the one whose started_at is
// first_started_at, else the earliest), even when bound to another pool
// since; it stays NULL for a Run never started, one without a started
// placement, one whose first host has no pool, and every Run not ended
// (running or resting). How readers count a NULL is tested in
// internal/server (TestPoolFlowMovedRun and the other TestPoolFlow tests).
func TestRunFirstPoolMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "077_host_disk_pressure"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO pools (id, name, provider) VALUES ('pa', 'a', 'static'), ('pb', 'b', 'static')`,
		`INSERT INTO hosts (id, name, state, pool_id) VALUES ('ha', 'ha', 'ready', 'pa'), ('hb', 'hb', 'ready', 'pb'), ('hn', 'hn', 'ready', NULL)`,
		`INSERT INTO runs (id, tenant_id, spec, state, pool_id, first_started_at) VALUES
			('moved', 't1', '{}', 'succeeded', 'pb', '2026-01-01 10:00Z'),
			('earliest', 't1', '{}', 'failed', 'pb', '2026-01-01 10:30Z'),
			('never', 't1', '{}', 'stopped', 'pa', NULL),
			('unplaced', 't1', '{}', 'terminated', 'pa', '2026-01-01 10:00Z'),
			('poolless', 't1', '{}', 'succeeded', 'pa', '2026-01-01 10:00Z'),
			('resting', 't1', '{}', 'stopped', 'pb', '2026-01-01 10:00Z'),
			('live', 't1', '{}', 'running', 'pb', now() - interval '30 minutes')`,
		// moved: an earlier placement on hb never matched first_started_at;
		// the one that did ran on ha. live: first ran on ha, now on hb.
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at) VALUES
			('m1', 't1', 'moved', 'hb', 1, 'exited', '2026-01-01 09:00Z'),
			('m2', 't1', 'moved', 'ha', 2, 'exited', '2026-01-01 10:00Z'),
			('m3', 't1', 'moved', 'hb', 3, 'exited', '2026-01-01 11:00Z'),
			('e1', 't1', 'earliest', 'hb', 1, 'exited', NULL),
			('e2', 't1', 'earliest', 'hb', 2, 'exited', '2026-01-01 10:20Z'),
			('e3', 't1', 'earliest', 'ha', 3, 'exited', '2026-01-01 10:10Z'),
			('n1', 't1', 'never', 'ha', 1, 'exited', NULL),
			('p1', 't1', 'poolless', 'hn', 1, 'exited', '2026-01-01 10:00Z'),
			('p2', 't1', 'poolless', 'ha', 2, 'exited', '2026-01-01 11:00Z'),
			('s1', 't1', 'resting', 'ha', 1, 'exited', '2026-01-01 10:00Z')`,
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at) VALUES
			('l1', 't1', 'live', 'ha', 1, 'exited', now() - interval '30 minutes'),
			('l2', 't1', 'live', 'hb', 2, 'running', now() - interval '10 minutes')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"moved": "pa", "earliest": "pa", "never": "", "unplaced": "", "poolless": "", "resting": "", "live": ""}
	for id, pool := range want {
		var got string
		if err := conn.QueryRow(ctx, `SELECT coalesce(first_pool_id, '') FROM runs WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != pool {
			t.Errorf("%s: first_pool_id %q, want %q", id, got, pool)
		}
	}
}
