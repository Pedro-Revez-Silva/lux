package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 058 numbers each (run, path)'s artifacts in the order they were made
// (epoch, then created_at, then id), and a second row with a taken number
// is refused.
func TestArtifactVersionsBackfill(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "057_runs_resting_succeeded"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'stopped', 3), ('r2', 't1', '{}', 'stopped', 1)`,
		`INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location)
			SELECT 'b' || n, 't1', 'r1', 1, 'artifact', '/a', 'host' FROM generate_series(1, 6) n`,
		// /a: epoch 2 made before epoch 1's rows (clock skew between luxds)
		// still comes after them; two at one instant go by id.
		`INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id, created_at) VALUES
			('a-e2', 't1', 'r1', 2, '/a', 'b1', '2026-01-01'),
			('a-e1y', 't1', 'r1', 1, '/a', 'b2', '2026-01-02'),
			('a-e1x', 't1', 'r1', 1, '/a', 'b3', '2026-01-02'),
			('a-e1-early', 't1', 'r1', 1, '/a', 'b4', '2025-12-31'),
			('b-1', 't1', 'r1', 3, '/b', 'b5', '2026-01-03'),
			('r2-a', 't1', 'r2', 1, '/a', 'b6', '2026-01-04')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT id || '=' || version || ':' || description FROM artifacts ORDER BY run_id, path, version`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a-e1-early=1:", "a-e1x=2:", "a-e1y=3:", "a-e2=4:", "b-1=1:", "r2-a=1:"}
	if !slices.Equal(got, want) {
		t.Fatalf("versions %v, want %v", got, want)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id, version) VALUES ('dup', 't1', 'r1', 3, '/a', 'b6', 4)`); err == nil {
		t.Fatal("a second version 4 of /a was accepted")
	}
}
