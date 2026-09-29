package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 036 gives a lease held when it runs a token (0) and keeps it held, so a
// provisioner that upgrades mid-lease keeps its lease; hosts from before
// it are not confirmed to carry lux:pool-id (their pool is migrated by the
// provisioner); pools keep their names, never renamed.
func TestPoolRenameMigration(t *testing.T) {
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
		`INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', 'luxd-a', now() + interval '1 minute')`,
		`INSERT INTO pools (id, name, provider) VALUES ('p1', 'burst', 'ec2')`,
		`INSERT INTO hosts (id, name, pool, state, provider_id, provision_requested_at, tagged) VALUES ('h1', 'burst-1', 'burst', 'ready', 'i-1', now(), true)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "036_pool_rename"); err != nil {
		t.Fatal(err)
	}
	var holder string
	var token int64
	if err := conn.QueryRow(ctx, `SELECT holder, token FROM leases WHERE name = 'provisioner'`).Scan(&holder, &token); err != nil {
		t.Fatal(err)
	}
	if holder != "luxd-a" || token != 0 {
		t.Errorf("lease %s token %d, want luxd-a 0", holder, token)
	}
	var next int64
	if err := conn.QueryRow(ctx, `SELECT nextval('lease_tokens')`).Scan(&next); err != nil || next <= token {
		t.Errorf("lease_tokens next %d (%v): must exceed every token handed out", next, err)
	}
	var tagged bool
	if err := conn.QueryRow(ctx, `SELECT pool_id_tagged FROM hosts WHERE id = 'h1'`).Scan(&tagged); err != nil || tagged {
		t.Errorf("a host from before 036 confirmed to carry lux:pool-id: %v (%v)", tagged, err)
	}
	var name string
	var renamed, migrated bool
	var previous []string
	if err := conn.QueryRow(ctx, `SELECT name, renamed_at IS NOT NULL, id_migrated_at IS NOT NULL, previous_names FROM pools WHERE id = 'p1'`).Scan(&name, &renamed, &migrated, &previous); err != nil ||
		name != "burst" || renamed || migrated || len(previous) != 0 {
		t.Errorf("pool %s renamed %v previous %v (%v)", name, renamed, previous, err)
	}
}

// Row-level security on what 036 adds or changes: luxd_instances,
// pool_rename_fence and leases are luxd's own (no tenant scope reads or writes them); pools and
// hosts, with their new columns, stay each tenant's own; and
// lux_pool_renamed_to, which runs as the owner, answers a tenant only
// about pools its Runs may use, and only lux_app may call it.
func TestPoolRenameRLS(t *testing.T) {
	owner, appDSN := testDB(t)
	ctx := context.Background()
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sys := func(q string, args ...any) {
		t.Helper()
		if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	sys(`INSERT INTO tenants (id, name) VALUES ('t1', 'a'), ('t2', 'b')`)
	sys(`INSERT INTO luxd_instances (instance, version, capabilities, seen_at) VALUES ('luxd-a', 'v1', '{pool-id-discovery}', now())`)
	sys(`INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', 'luxd-a', now() + interval '1 minute')`)
	sys(`INSERT INTO pool_rename_fence (armed_at) VALUES (now())`)
	sys(`INSERT INTO pools (id, tenant_id, name, provider, renamed_at, previous_names) VALUES
		('p1', 't1', 'one', 'ec2', now(), '{old1}'), ('p2', 't2', 'two', 'ec2', now(), '{old2}'), ('pp', NULL, 'plat', 'static', now(), '{oldp}')`)
	sys(`INSERT INTO hosts (id, tenant_id, name, pool, state, pool_id_tagged) VALUES ('h1', 't1', 'h1', 'one', 'ready', true), ('h2', 't2', 'h2', 'two', 'ready', true)`)

	inTenant := func(tenant, q string, args ...any) (int, error) {
		var n int
		err := db.Tx(ctx, store.Tenant(tenant), func(tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) })
		return n, err
	}
	for _, q := range []string{`SELECT count(*) FROM luxd_instances`, `SELECT count(*) FROM leases`, `SELECT count(*) FROM pool_rename_fence`,
		`WITH d AS (DELETE FROM pool_rename_fence RETURNING 1) SELECT count(*) FROM d`} {
		if n, err := inTenant("t1", q); err != nil || n != 0 {
			t.Errorf("%s in a tenant scope: %d (%v)", q, n, err)
		}
	}
	for _, q := range []string{
		`UPDATE leases SET holder = 'luxd-t', expires_at = now() + interval '1 hour' WHERE name = 'provisioner' RETURNING 1`,
		`INSERT INTO luxd_instances (instance, version, capabilities, seen_at) VALUES ('luxd-t', 'v', '{pool-id-discovery}', now()) RETURNING 1`,
	} {
		if n, err := inTenant("t1", q); err == nil {
			t.Errorf("%s in a tenant scope: allowed (%d)", q, n)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM pools WHERE renamed_at IS NOT NULL AND tenant_id IS NOT NULL`,
		`SELECT count(*) FROM hosts WHERE pool_id_tagged`,
	} {
		if n, err := inTenant("t1", q); err != nil || n != 1 {
			t.Errorf("%s as t1: %d (%v), want its own row only", q, n, err)
		}
	}
	if n, err := inTenant("t1", `WITH u AS (UPDATE pools SET previous_names = '{}' WHERE id = 'p2' RETURNING 1) SELECT count(*) FROM u`); err != nil || n != 0 {
		t.Errorf("t1 updating t2's pool: %d (%v)", n, err)
	}

	renamedTo := func(tenant, name string) *string {
		t.Helper()
		var to *string
		if err := db.Tx(ctx, store.Tenant(tenant), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lux_pool_renamed_to($1)`, name).Scan(&to)
		}); err != nil {
			t.Fatal(err)
		}
		return to
	}
	if to := renamedTo("t1", "old1"); to == nil || *to != "one" {
		t.Errorf("t1's own renamed pool: %v", to)
	}
	if to := renamedTo("t1", "oldp"); to == nil || *to != "plat" {
		t.Errorf("a platform pool, as t1: %v", to)
	}
	if to := renamedTo("t1", "old2"); to != nil {
		t.Errorf("t2's pool's old name, as t1: %q", *to)
	}

	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for role, want := range map[string]bool{"public": false, "lux_app": true} {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT has_function_privilege($1, 'lux_pool_renamed_to(text)', 'EXECUTE')`, role).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s may call lux_pool_renamed_to: %v, want %v", role, got, want)
		}
	}
}

// A database that reached 037_snapshot_records without 036 (luxd from main
// before the rename shipped) gets 036 at the next start: the migrator
// applies every unrecorded version, in name order, not only those after
// the latest. Both schemas are then there, and the rows from before too.
func TestPoolRenameMigrationAfter037(t *testing.T) {
	owner, appDSN := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "035_snapshot_refused"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	body, err := store.Migration("037_snapshot_records")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		string(body),
		`INSERT INTO schema_migrations (version) VALUES ('037_snapshot_records')`,
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES ('p1', 't1', 'burst', 'ec2', true)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var has036 bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '036_pool_rename')`).Scan(&has036); err != nil || has036 {
		t.Fatalf("036 before the upgrade: %v (%v)", has036, err)
	}

	done, err := store.Migrate(ctx, owner, "lux_app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(done, "036_pool_rename") || slices.Contains(done, "037_snapshot_records") {
		t.Fatalf("applied %v, want 036_pool_rename and not 037 again", done)
	}
	for _, col := range []struct{ table, column string }{
		{"pools", "previous_names"}, {"pools", "id_migrated_at"}, {"hosts", "pool_id_tagged"}, {"leases", "token"},
		{"luxd_instances", "capabilities"}, {"pool_rename_fence", "armed_at"},
		{"blobs", "snapshot_id"}, {"artifacts", "snapshot_id"}, {"snapshots", "owns_records"},
		{"placements", "snapshot_refused"}, {"pools", "is_default"}, {"runs", "pool_owner"},
	} {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
			col.table, col.column).Scan(&ok); err != nil || !ok {
			t.Errorf("%s.%s missing after the upgrade (%v)", col.table, col.column, err)
		}
	}
	var name string
	var isDefault bool
	var previous []string
	if err := conn.QueryRow(ctx, `SELECT name, is_default, previous_names FROM pools WHERE id = 'p1'`).Scan(&name, &isDefault, &previous); err != nil ||
		name != "burst" || !isDefault || len(previous) != 0 {
		t.Errorf("pool from before the upgrade: %s default %v previous %v (%v)", name, isDefault, previous, err)
	}
	// 036's function, granted to lux_app by the same start.
	app, err := pgx.Connect(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	var renamedTo *string
	if err := app.QueryRow(ctx, `SELECT lux_pool_renamed_to('burst')`).Scan(&renamedTo); err != nil || renamedTo != nil {
		t.Errorf("lux_pool_renamed_to as lux_app: %v (%v)", renamedTo, err)
	}
}
