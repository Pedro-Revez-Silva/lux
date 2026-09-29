package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

// 031 adds pools.is_default and marks no existing pool, a pool named
// "default" included: Runs naming no pool keep going to it by name.
func TestDefaultPoolMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "030"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p1', 't1', 'default', 'static'), ('p2', NULL, 'default', 'static'), ('p3', 't1', 'arm', 'static')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var marked int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pools WHERE is_default`).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 0 {
		t.Fatalf("migration marked %d pools as default", marked)
	}
	var public, app bool
	if err := conn.QueryRow(ctx, `SELECT has_function_privilege('public', 'lux_default_pool()', 'EXECUTE'),
		has_function_privilege('lux_app', 'lux_default_pool()', 'EXECUTE')`).Scan(&public, &app); err != nil {
		t.Fatal(err)
	}
	if public || !app {
		t.Fatalf("lux_default_pool callable by public: %v, by lux_app: %v", public, app)
	}
}

// 032 adds runs.pool_owner and leaves existing Runs' NULL: they keep
// matching hosts and pools by name alone. lux_pool_owner, like
// lux_default_pool, is lux_app's only, and prefers the tenant's pool.
func TestRunPoolOwnerMigration(t *testing.T) {
	owner, appDSN := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "031"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p1', 't1', 'burst', 'static'), ('p2', NULL, 'burst', 'static'), ('p3', NULL, 'x86', 'static');
		INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{"placement":{"pool":"burst"}}', 'submitted')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var legacy *string
	if err := conn.QueryRow(ctx, `SELECT pool_owner FROM runs WHERE id = 'r1'`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != nil {
		t.Fatalf("a Run from before 032 has pool_owner %q, want NULL", *legacy)
	}
	var public, app bool
	if err := conn.QueryRow(ctx, `SELECT has_function_privilege('public', 'lux_pool_owner(text)', 'EXECUTE'),
		has_function_privilege('lux_app', 'lux_pool_owner(text)', 'EXECUTE')`).Scan(&public, &app); err != nil {
		t.Fatal(err)
	}
	if public || !app {
		t.Fatalf("lux_pool_owner callable by public: %v, by lux_app: %v", public, app)
	}
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for pool, want := range map[string]string{"burst": "t1", "x86": "", "none": "<nil>"} {
		var got *string
		if err := db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce(lux_pool_owner($1), '<nil>')`, pool).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if *got != want {
			t.Errorf("lux_pool_owner(%q) = %q, want %q", pool, *got, want)
		}
	}
}

func defaultPoolFixture(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	_, appDSN := testDB(t)
	ctx := context.Background()
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	err = db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2');
			INSERT INTO pools (id, tenant_id, name, provider) VALUES
				('a', 't1', 'a', 'static'), ('b', 't1', 'b', 'static'), ('c', 't2', 'c', 'static'),
				('pa', NULL, 'pa', 'static'), ('pb', NULL, 'pb', 'static')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func isExclusion(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23P01"
}

// One default per tenant and one among platform pools, whatever order
// transactions commit in; moving the mark in one statement is allowed.
func TestOneDefaultPool(t *testing.T) {
	db, ctx := defaultPoolFixture(t)
	exec := func(sc store.Scope, q string) error {
		return db.Tx(ctx, sc, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
	}
	if err := exec(store.Tenant("t1"), `UPDATE pools SET is_default = true WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(store.Tenant("t1"), `UPDATE pools SET is_default = true WHERE name = 'b'`); !isExclusion(err) {
		t.Fatalf("a second default for t1: %v", err)
	}
	// Another tenant's default is its own.
	if err := exec(store.Tenant("t2"), `UPDATE pools SET is_default = true WHERE name = 'c'`); err != nil {
		t.Fatal(err)
	}
	// Moving: whichever row the statement reaches first.
	for _, to := range []string{"b", "a", "b"} {
		if err := exec(store.Tenant("t1"), `UPDATE pools SET is_default = (name = '`+to+`') WHERE tenant_id = 't1'`); err != nil {
			t.Fatalf("moving the mark to %s: %v", to, err)
		}
	}
	if err := exec(store.System(), `UPDATE pools SET is_default = true WHERE name = 'pa'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(store.System(), `UPDATE pools SET is_default = true WHERE name = 'pb'`); !isExclusion(err) {
		t.Fatalf("a second platform default: %v", err)
	}

	// Two transactions marking different pools at once: the second waits
	// for the first and then fails; t1 never has two defaults.
	if err := exec(store.Tenant("t1"), `UPDATE pools SET is_default = false`); err != nil {
		t.Fatal(err)
	}
	first, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if _, err := first.Exec(ctx, `SELECT set_config('lux.tenant_id', 't1', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `UPDATE pools SET is_default = true WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	var firstPID int
	if err := first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	// Bounded: were the constraint not to make it wait, or to wait on
	// something else for good, the test fails instead of hanging.
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	executing := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		second <- db.Tx(deadline, store.Tenant("t1"), func(tx pgx.Tx) error {
			close(executing)
			// Not the row first holds: only the constraint makes it wait.
			_, err := tx.Exec(deadline, `UPDATE pools SET is_default = true WHERE name = 'b'`)
			return err
		})
	}()
	select {
	case <-executing:
	case err := <-second:
		t.Fatalf("the second mark ended before its UPDATE: %v", err)
	case <-deadline.Done():
		t.Fatal("the second mark never began")
	}
	// It waits on the first transaction, not merely has yet to run.
	for waiting := false; !waiting; {
		if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))`, firstPID).Scan(&waiting)
		}); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-second:
			t.Fatalf("the second mark did not wait for the first: %v", err)
		case <-deadline.Done():
			t.Fatal("the second mark never waited for the first")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second:
		if !isExclusion(err) {
			t.Fatalf("the second mark, after the first committed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second mark still waits after the first committed")
	}
	var n int
	if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pools WHERE tenant_id = 't1' AND is_default`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("t1 has %d defaults (%v)", n, err)
	}
}

// pools' row-level security covers the mark: a tenant cannot mark another
// tenant's pool or a platform pool, and lux_default_pool reads only the
// caller's default and the platform's.
func TestDefaultPoolRLS(t *testing.T) {
	db, ctx := defaultPoolFixture(t)
	err := db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE pools SET is_default = true WHERE name IN ('c', 'pa')`)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("t1 marked %d pools that are not its own", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(tenant string) (pool, from *string) {
		t.Helper()
		if err := db.Tx(ctx, store.Tenant(tenant), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT pool, pool_from FROM lux_default_pool()`).Scan(&pool, &from)
		}); err != nil {
			t.Fatal(err)
		}
		return pool, from
	}
	if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pools SET is_default = true WHERE name IN ('c', 'pa')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if p, f := resolve("t1"); p == nil || *p != "pa" || *f != "platform-default" {
		t.Fatalf("t1 resolved %v %v, want the platform's pa", p, f)
	}
	if p, f := resolve("t2"); p == nil || *p != "c" || *f != "tenant-default" {
		t.Fatalf("t2 resolved %v %v, want its own c", p, f)
	}
}
