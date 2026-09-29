package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 038 moves hosts, host tokens, Runs and hourly costs from a pool's name
// (and owner) to its id: each row takes the id of its owner's pool of that
// name, a static pool known only by name becomes a row, and a Run takes
// the pool its pool_owner named.
func TestPoolIDMigration(t *testing.T) {
	owner, appDSN := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "037_snapshot_records"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p-t1', 't1', 'burst', 'ec2'), ('p-plat', NULL, 'burst', 'static');
		INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('tk1', 't1', 'burst', 'a'), ('tk2', NULL, 'gpu', 'b');
		INSERT INTO hosts (id, tenant_id, pool, name, state) VALUES ('h1', 't1', 'burst', 'h1', 'ready'), ('h2', NULL, 'burst', 'h2', 'ready'), ('h3', NULL, 'gpu', 'h3', 'ready');
		INSERT INTO runs (id, tenant_id, spec, state, pool_owner) VALUES
			('r-plat', 't1', '{"placement":{"pool":"burst"}}', 'submitted', ''),
			('r-t1', 't1', '{"placement":{"pool":"burst"}}', 'submitted', 't1'),
			('r-legacy', 't1', '{"placement":{"pool":"burst"}}', 'submitted', NULL),
			('r-none', 't1', '{"placement":{"pool":"nowhere"}}', 'submitted', NULL);
		INSERT INTO cost_hourly (hour, host_id, pool, source, family, currency) VALUES (date_trunc('hour', now()), 'h2', 'burst', 'compute', 'compute', 'USD')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var gpu string
	if err := conn.QueryRow(ctx, `SELECT id FROM pools WHERE tenant_id IS NULL AND name = 'gpu' AND provider = 'static'`).Scan(&gpu); err != nil {
		t.Fatalf("the static pool known only by name: %v", err)
	}
	want := map[string]string{
		"host_tokens/tk1": "p-t1", "host_tokens/tk2": gpu,
		"hosts/h1": "p-t1", "hosts/h2": "p-plat", "hosts/h3": gpu,
		"runs/r-plat": "p-plat", "runs/r-t1": "p-t1", "runs/r-legacy": "p-t1", "runs/r-none": "<nil>",
	}
	for key, w := range want {
		table, id, _ := cut(key)
		var got string
		if err := conn.QueryRow(ctx, `SELECT coalesce(pool_id, '<nil>') FROM `+table+` WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: pool_id %q, want %q", key, got, w)
		}
	}
	var costPool string
	if err := conn.QueryRow(ctx, `SELECT pool_id FROM cost_hourly WHERE host_id = 'h2'`).Scan(&costPool); err != nil || costPool != "p-plat" {
		t.Errorf("cost_hourly pool_id %q (%v), want p-plat", costPool, err)
	}

	// lux_pool_id: the tenant's pool shadows the platform's; lux_app's only.
	var public bool
	if err := conn.QueryRow(ctx, `SELECT has_function_privilege('public', 'lux_pool_id(text)', 'EXECUTE')`).Scan(&public); err != nil || public {
		t.Fatalf("lux_pool_id callable by public: %v (%v)", public, err)
	}
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, w := range map[string]string{"burst": "p-t1", "gpu": gpu, "none": "<nil>"} {
		var got string
		if err := db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce((lux_pool_id($1)).pool_id, '<nil>')`, name).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("lux_pool_id(%q) = %q, want %q", name, got, w)
		}
	}
	// A tenant reads a platform pool's name, never writes it.
	if err := db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pools WHERE id = 'p-plat'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("tenant sees %d platform pools (%v)", n, err)
		}
		tag, err := tx.Exec(ctx, `UPDATE pools SET name = 'x' WHERE id = 'p-plat'`)
		if err == nil && tag.RowsAffected() != 0 {
			t.Error("a tenant renamed a platform pool")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func cut(key string) (string, string, bool) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}
