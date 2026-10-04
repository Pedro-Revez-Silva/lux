package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 052 fills the stored bytes of samples written before 051 from the blobs'
// upload and deletion times: per tenant and kind, and for the whole system
// (the empty tenant id); a blob counts from its upload (inclusive) to its
// deletion (exclusive), and one never uploaded not at all.
func TestStoredBytesBackfill(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "051_stored_bytes"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// t1: a volume uploaded at 10:00 and deleted at 12:00, an output
	// uploaded at 11:00; t2: a context uploaded at 11:30, an artifact
	// uploaded at 11:00 and deleted at 12:00, an artifact never uploaded
	// (still on its host) and a volume deleted from the host.
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2');
		INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'succeeded'), ('r2', 't2', '{}', 'stopped');
		INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, location, created_at, uploaded_at, deleted_at) VALUES
			('v1', 't1', 'r1', 1, 'volume', 'v', 100, 'deleted', '2026-01-01 09:59Z', '2026-01-01 10:00Z', '2026-01-01 12:00Z'),
			('o1', 't1', 'r1', 1, 'output', 'o', 7, 's3', '2026-01-01 10:59Z', '2026-01-01 11:00Z', NULL),
			('c2', 't2', 'r2', 1, 'context', 'c', 40, 's3', '2026-01-01 11:29Z', '2026-01-01 11:30Z', NULL),
			('a2', 't2', 'r2', 1, 'artifact', 'a', 5000, 'host', '2026-01-01 09:00Z', NULL, NULL),
			('a3', 't2', 'r2', 1, 'artifact', 'b', 13, 'deleted', '2026-01-01 10:59Z', '2026-01-01 11:00Z', '2026-01-01 12:00Z'),
			('v2', 't2', 'r2', 1, 'volume', 'v', 3000, 'deleted', '2026-01-01 09:00Z', NULL, '2026-01-01 11:00Z');
		INSERT INTO system_samples (tenant_id, res, at)
			SELECT id, res, at FROM (VALUES (''), ('t1'), ('t2')) k(id),
				(VALUES (0), (60)) r(res),
				unnest(ARRAY['2026-01-01 09:59:59Z', '2026-01-01 10:00Z', '2026-01-01 11:00Z', '2026-01-01 11:45Z',
					'2026-01-01 11:59:59Z', '2026-01-01 12:00Z']::timestamptz[]) at;
		INSERT INTO system_samples (tenant_id, res, at) VALUES ('t1', 3600, '2026-01-01 11:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT tenant_id, res, to_char(at AT TIME ZONE 'UTC', 'HH24:MI:SS'),
		stored_volume, stored_output, stored_artifact, stored_context FROM system_samples ORDER BY tenant_id, res, at`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var id, at string
	var res int
	var v [4]int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &res, &at, &v[0], &v[1], &v[2], &v[3]}, func() error {
		got[fmt.Sprintf("%s/%d/%s", id, res, at)] = fmt.Sprint(v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// volume, output, artifact, context
	want := map[string]string{}
	for _, res := range []int{0, 60} {
		for at, w := range map[string][3]string{
			"09:59:59": {"[0 0 0 0]", "[0 0 0 0]", "[0 0 0 0]"},
			"10:00:00": {"[100 0 0 0]", "[100 0 0 0]", "[0 0 0 0]"},
			"11:00:00": {"[100 7 13 0]", "[100 7 0 0]", "[0 0 13 0]"},
			"11:45:00": {"[100 7 13 40]", "[100 7 0 0]", "[0 0 13 40]"},
			"11:59:59": {"[100 7 13 40]", "[100 7 0 0]", "[0 0 13 40]"},
			"12:00:00": {"[0 7 0 40]", "[0 7 0 0]", "[0 0 0 40]"},
		} {
			want[fmt.Sprintf("/%d/%s", res, at)] = w[0]
			want[fmt.Sprintf("t1/%d/%s", res, at)] = w[1]
			want[fmt.Sprintf("t2/%d/%s", res, at)] = w[2]
		}
	}
	want["t1/3600/11:00:00"] = "[100 7 0 0]"
	if fmt.Sprint(got) != fmt.Sprint(want) {
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%s: %s, want %s", k, got[k], w)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%d rows, want %d", len(got), len(want))
		}
	}
}
