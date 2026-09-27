package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

func TestFinalCostHoursUpgradeDefersSeeding(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "026_cost_host_refresh"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	from := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'succeeded'), ('r2', 't1', '{}', 'failed');
		INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES
			('r1', 't1', 'ledger', 'final'), ('r1', 't1', 'compute', 'final'), ('r2', 't1', 'ledger', 'ok')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO cost_lines (tenant_id, run_id, source, item, family, amount, currency, period_from, period_to, final) VALUES
			('t1', 'r1', 'ledger', 'a', 'ai', 2, 'USD', $1, $2, true),
			('t1', 'r1', 'compute', 'a', 'compute', 3, 'USD', $1, $2, true),
			('t1', 'r2', 'ledger', 'a', 'ai', 4, 'USD', $1, $2, false)`, from, from.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM cost_final_hour_backfill`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("upgrade synchronously seeded %d cursors: %v", count, err)
	}
}
