package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 031 gives a lease held when it runs a token (0) and keeps it held, so a
// provisioner that upgrades mid-lease keeps its lease; a pool keeps its
// name with no rename under way; luxd_instances is system-only.
func TestPoolRenameMigration(t *testing.T) {
	owner, app := emptyDB(t)
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
		`INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', 'luxd-a', now() + interval '1 minute')`,
		`INSERT INTO pools (id, name, provider) VALUES ('p1', 'burst', 'ec2')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "031_pool_rename"); err != nil {
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
	// luxd_instances is luxd's own, like control_samples: no tenant sees it.
	db, err := store.Open(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO luxd_instances (instance, version, capabilities, seen_at) VALUES ('luxd-a', 'v1', '{pool-rename}', now())`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM luxd_instances`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("a tenant scope sees %d luxd_instances rows (%v)", n, err)
	}
	var name string
	var renamed *string
	if err := conn.QueryRow(ctx, `SELECT name, renamed_from FROM pools WHERE id = 'p1'`).Scan(&name, &renamed); err != nil || name != "burst" || renamed != nil {
		t.Errorf("pool %s renamed_from %v (%v)", name, renamed, err)
	}
}
