package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 033 on a database with hosts and snapshots from before it: every host
// has no capabilities (its runner has not said hello since), and no
// snapshot has a diff state, which the API reads as no diff; a diff blob
// is a valid kind from then on.
func TestSnapshotDiffsMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "029_process_samples"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'stopped', 1)`,
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'exited')`,
		`INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest) VALUES ('s1', 't1', 'r1', 'p1', 1, '{}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location) VALUES ('b0', 't1', 'r1', 1, 'diff', 'x', 'host')`); err == nil {
		t.Error("a diff blob before 033")
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var caps []string
	if err := conn.QueryRow(ctx, `SELECT capabilities FROM hosts WHERE id = 'h1'`).Scan(&caps); err != nil || len(caps) != 0 {
		t.Errorf("h1's capabilities: %v %v", caps, err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM snapshot_diff_state`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d diff states (%v)", n, err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location) VALUES ('b1', 't1', 'r1', 1, 'diff', 'x', 'host')`); err != nil {
		t.Errorf("a diff blob after 033: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO snapshot_diff_state (snapshot_id, tenant_id, run_id, state) VALUES ('s1', 't1', 'r1', 'lost')`); err == nil {
		t.Error("an unknown diff state was stored")
	}
}
