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

// Input events recorded before 043 are known to it: a repeat of one is
// still a conflict after the upgrade. Events without a request id, or
// with a JSON null one, have no key.
func TestRunInputEventsBackfill(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "042_dashboard_indexes"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`,
		`INSERT INTO run_events (tenant_id, run_id, epoch, type, data) VALUES
			('t1', 'r1', 1, 'input.delivered', '{"requestId":"a"}'), ('t1', 'r1', 1, 'input.delivered', '{"requestId":"a"}'),
			('t1', 'r1', 1, 'input.failed', '{"requestId":"b"}'), ('t1', 'r1', 1, 'activity', '{"activity":"idle"}'),
			('t1', 'r1', 1, 'input.delivered', '{"requestId":null}'), ('t1', 'r1', 1, 'input.failed', '{"error":"x"}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, err := conn.Query(ctx, `SELECT type || ' ' || request_id FROM run_input_events ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "input.delivered a" || keys[1] != "input.failed b" {
		t.Fatalf("backfilled %q", keys)
	}
}

// The backfill of run_input_events does not hold locks that block writes
// to runs: while it waits on run_events (held here by a lock it cannot
// take), a Run is still created and updated.
func TestRunInputEventsBackfillLetsRunsBeWritten(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "042_dashboard_indexes"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE run_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	migrated := make(chan error, 1)
	go func() {
		_, err := store.Migrate(ctx, owner, "lux_app")
		migrated <- err
	}()
	// The migrator is waiting for run_events.
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted AND relation = 'run_events'::regclass)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the migrator never waited for run_events")
		}
	}
	var held []string
	rows, err := conn.Query(ctx, `SELECT l.relation::regclass::text || ' ' || l.mode FROM pg_locks l
		WHERE l.granted AND l.relation IN ('runs'::regclass, 'tenants'::regclass)
		AND l.pid IN (SELECT pid FROM pg_locks WHERE NOT granted AND relation = 'run_events'::regclass) ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if held, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	t.Logf("the waiting migrator holds on runs/tenants: %q", held)
	_, werr := conn.Exec(ctx, `SET lock_timeout = '2s';
		INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r2', 't1', '{}', 'running', 1);
		UPDATE runs SET state = 'stopped' WHERE id = 'r1';
		SET lock_timeout = 0`)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	var pe *pgconn.PgError
	if errors.As(werr, &pe) && pe.Code == "55P03" {
		t.Fatalf("writing runs during the backfill: %v (the migrator held %q)", werr, held)
	} else if werr != nil {
		t.Fatal(werr)
	}
}
