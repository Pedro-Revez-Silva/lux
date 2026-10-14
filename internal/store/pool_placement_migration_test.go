package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

// 075: a pool from before it has no placement (its default); spread and
// pack round-trip; any other value is refused by the CHECK.
func TestPoolPlacementMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "074_capacity_age_transitions"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool_old', 'old', 'static')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var placement *string
	if err := conn.QueryRow(ctx, `SELECT placement FROM pools WHERE id = 'pool_old'`).Scan(&placement); err != nil || placement != nil {
		t.Fatalf("legacy pool: placement %v (%v), want NULL", placement, err)
	}
	for _, v := range []string{"spread", "pack"} {
		if _, err := conn.Exec(ctx, `UPDATE pools SET placement = $1 WHERE id = 'pool_old'`, v); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		var got string
		if err := conn.QueryRow(ctx, `SELECT placement FROM pools WHERE id = 'pool_old'`).Scan(&got); err != nil || got != v {
			t.Fatalf("round trip %s: %q %v", v, got, err)
		}
	}
	_, err = conn.Exec(ctx, `UPDATE pools SET placement = 'random' WHERE id = 'pool_old'`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("placement 'random': %v, want a check violation", err)
	}
}

// 076: a Run from before it has no raise: its three raise columns are NULL.
func TestRunSizeRaiseMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "075_pool_placement"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state, resolved_size) VALUES ('run_old', 't1', '{}', 'stopped', 'm-2-arm64')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var from, raisedFor, to *string
	if err := conn.QueryRow(ctx, `SELECT size_raised_from, size_raised_for, size_raised_to FROM runs WHERE id = 'run_old'`).
		Scan(&from, &raisedFor, &to); err != nil || from != nil || raisedFor != nil || to != nil {
		t.Fatalf("legacy run: raised from %v for %v to %v (%v), want all NULL", from, raisedFor, to, err)
	}
}
