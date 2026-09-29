package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 035: snapshots, output and artifact blobs recorded before it own no
// records (owns_records false, snapshot_id NULL), so luxd compares their
// redelivered reports by manifest only.
func TestSnapshotOwnerUpgrade(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "030_servers"); err != nil {
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
		`INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location) VALUES
			('b-out', 't1', 'r1', 1, 'output', 'output', 'host'), ('b-art', 't1', 'r1', 1, 'artifact', '/a', 'host')`,
		`INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id) VALUES ('a1', 't1', 'r1', 1, '/a', 'b-art')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var owns bool
	var owned int
	if err := conn.QueryRow(ctx, `SELECT owns_records,
			(SELECT count(*) FROM blobs WHERE snapshot_id IS NOT NULL) + (SELECT count(*) FROM artifacts WHERE snapshot_id IS NOT NULL)
		FROM snapshots WHERE id = 's1'`).Scan(&owns, &owned); err != nil {
		t.Fatal(err)
	}
	if owns || owned != 0 {
		t.Fatalf("after 035: owns_records %v, %d rows with a snapshot_id; want false, 0", owns, owned)
	}
}
