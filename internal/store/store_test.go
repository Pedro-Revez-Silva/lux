package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// These tests need a Postgres to create databases in:
//
//	LUX_TEST_PG=postgres://lux:lux@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/store
//
// (tests/run_tests.py starts one on that port.) They are skipped otherwise.
func testDB(t *testing.T) (owner, app string) {
	t.Helper()
	owner, app = emptyDB(t)
	if _, err := store.Migrate(context.Background(), owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	return owner, app
}

// emptyDB is a new database with no migrations applied.
func emptyDB(t *testing.T) (owner, app string) {
	t.Helper()
	admin := os.Getenv("LUX_TEST_PG")
	if admin == "" {
		t.Skip("LUX_TEST_PG not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	name := "lux_unit_" + ids.New("")[1:]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		conn.Exec(ctx, "DROP DATABASE "+name)
		conn.Close(ctx)
	})
	cfg := conn.Config()
	owner = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	app = fmt.Sprintf("postgres://lux_app:lux_app@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, name)
	return owner, app
}

// Row-level security is a real boundary: the app role sees nothing without
// a scope, only its own tenant's rows with one, and cannot bypass it.
func TestRLS(t *testing.T) {
	_, appDSN := testDB(t)
	ctx := context.Background()
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var bypass bool
	if err := db.Pool.QueryRow(ctx, "SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&bypass); err != nil || bypass {
		t.Fatalf("app role can bypass RLS: %v %v", bypass, err)
	}

	mk := func(name string) string {
		id := ids.New(ids.Tenant)
		err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO tenants (id, name) VALUES ($1, $2)", id, name); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, $2, '{}', 'submitted')`, ids.New(ids.Run), id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := mk("a"), mk("b")

	var n int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM runs").Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped query saw %d runs (%v)", n, err)
	}
	err = db.Tx(ctx, store.Tenant(a), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM runs").Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("tenant a sees %d runs", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Writing a row for another tenant is refused.
	err = db.Tx(ctx, store.Tenant(a), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, $2, '{}', 'submitted')`, ids.New(ids.Run), b)
		return err
	})
	if err == nil {
		t.Fatal("tenant a wrote a run for tenant b")
	}
	// Events are append-only for the app.
	err = db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM run_events")
		return err
	})
	if err == nil {
		t.Fatal("app role could delete events")
	}
}

// Migrating twice is a no-op.
func TestMigrateIdempotent(t *testing.T) {
	owner, _ := testDB(t)
	applied, err := store.Migrate(context.Background(), owner, "lux_app")
	if err != nil || len(applied) != 0 {
		t.Fatalf("second migrate applied %v (%v)", applied, err)
	}
}

// lux_cost_enqueue runs as its owner (it queues from a tenant's scope):
// only lux_app may call it, not every role, and only with a reason the
// queue knows.
func TestCostEnqueueGrant(t *testing.T) {
	owner, appDSN := testDB(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for role, want := range map[string]bool{"public": false, "lux_app": true} {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT has_function_privilege($1, 'lux_cost_enqueue(text, text)', 'EXECUTE')`, role).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s may call lux_cost_enqueue: %v, want %v", role, got, want)
		}
	}

	app, err := pgx.Connect(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if _, err := app.Exec(ctx, `SELECT set_config('lux.system', 'on', false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'stopped')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `SELECT lux_cost_enqueue('r1', 'state:stopped')`); err != nil {
		t.Errorf("lux_app queueing: %v", err)
	}
	if _, err := app.Exec(ctx, `SELECT lux_cost_enqueue('r1', 'anything')`); err == nil {
		t.Error("queued with an unknown reason")
	}
}
