package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Reports recorded before 037 have no ownership data, whether the database
// was last migrated to 030 or to 035.
func TestSnapshotOwnerUpgrade(t *testing.T) {
	for _, from := range []string{"030_servers", "035_snapshot_refused"} {
		t.Run(from, func(t *testing.T) {
			owner, _ := emptyDB(t)
			ctx := context.Background()
			if _, err := store.MigrateTo(ctx, owner, "lux_app", from); err != nil {
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
			if from == "035_snapshot_refused" {
				var refused bool
				if err := conn.QueryRow(ctx, `SELECT snapshot_refused FROM placements WHERE id = 'p1'`).Scan(&refused); err != nil || refused {
					t.Fatalf("035 placement refusal: %v, %v", refused, err)
				}
			}
			done, err := store.Migrate(ctx, owner, "lux_app")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"037_snapshot_records", "038_pool_id", "039_luxd_alive", "040_launch_outcome", "041_pool_samples", "042_dashboard_indexes", "043_run_input_events", "044_run_input_events_backfill"}
			if from == "030_servers" {
				want = []string{"031_pool_template_tags", "032_default_pool", "033_run_pool_owner", "034_pool_host_events", "035_snapshot_refused", "037_snapshot_records", "038_pool_id", "039_luxd_alive", "040_launch_outcome", "041_pool_samples", "042_dashboard_indexes", "043_run_input_events", "044_run_input_events_backfill"}
			}
			if !slices.Equal(done, want) {
				t.Fatalf("migration order from %s: got %v, want %v", from, done, want)
			}
			var owns bool
			var owned int
			if err := conn.QueryRow(ctx, `SELECT owns_records,
					(SELECT count(*) FROM blobs WHERE snapshot_id IS NOT NULL) + (SELECT count(*) FROM artifacts WHERE snapshot_id IS NOT NULL)
				FROM snapshots WHERE id = 's1'`).Scan(&owns, &owned); err != nil {
				t.Fatal(err)
			}
			if owns || owned != 0 {
				t.Fatalf("after 037: owns_records %v, %d rows with a snapshot_id; want false, 0", owns, owned)
			}
		})
	}
}
