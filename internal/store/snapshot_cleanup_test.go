package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 054 flags exactly the Runs with an available snapshot other than their
// current one, a legacy Run with no current snapshot (snapshot_id NULL)
// but an available one among them.
func TestSnapshotCleanupMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "053_run_expiry"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
			('two', 't1', '{}', 'stopped', 2), ('one', 't1', '{}', 'stopped', 1),
			('gone', 't1', '{}', 'stopped', 2), ('none', 't1', '{}', 'stopped', 0),
			('legacy', 't1', '{}', 'stopped', 1)`,
		`INSERT INTO hosts (id, name, state) VALUES ('h', 'h', 'ready')`,
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
			('p1', 't1', 'two', 'h', 1, 'exited'), ('p2', 't1', 'two', 'h', 2, 'exited'), ('p3', 't1', 'one', 'h', 1, 'exited'),
			('p4', 't1', 'gone', 'h', 1, 'exited'), ('p5', 't1', 'gone', 'h', 2, 'exited'), ('p6', 't1', 'legacy', 'h', 1, 'exited')`,
		`INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, available) VALUES
			('s1', 't1', 'two', 'p1', 1, '{}', true), ('s2', 't1', 'two', 'p2', 2, '{}', true),
			('s3', 't1', 'one', 'p3', 1, '{}', true),
			('s4', 't1', 'gone', 'p4', 1, '{}', false), ('s5', 't1', 'gone', 'p5', 2, '{}', true),
			('s6', 't1', 'legacy', 'p6', 1, '{}', true)`,
		`UPDATE runs SET snapshot_id = CASE id WHEN 'two' THEN 's2' WHEN 'one' THEN 's3' WHEN 'gone' THEN 's5' END`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT id FROM runs WHERE snapshots_superseded ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"legacy", "two"}; !slices.Equal(flagged, want) {
		t.Fatalf("flagged %v, want %v", flagged, want)
	}
}
