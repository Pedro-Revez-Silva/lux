package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 053 backfills state_changed_at from each Run's latest state event, else
// its updated_at, and gives existing tenants expire_after_days 90.
func TestRunExpiryMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "050_prompt_attachments"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch, updated_at) VALUES
			('evented', 't1', '{}', 'stopped', 1, '2026-09-01T00:00:00Z'),
			('bare', 't1', '{}', 'lost', 1, '2026-08-01T00:00:00Z')`,
		`INSERT INTO run_events (tenant_id, run_id, type, data, created_at) VALUES
			('t1', 'evented', 'state', '{"state":"running"}', '2026-05-01T00:00:00Z'),
			('t1', 'evented', 'state', '{"state":"stopped"}', '2026-06-01T00:00:00Z'),
			('t1', 'evented', 'output', '{}', '2026-07-01T00:00:00Z')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"evented": "2026-06-01T00:00:00Z", "bare": "2026-08-01T00:00:00Z"} {
		var got time.Time
		if err := conn.QueryRow(ctx, `SELECT state_changed_at FROM runs WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if w, _ := time.Parse(time.RFC3339, want); !got.Equal(w) {
			t.Errorf("%s: state_changed_at %s, want %s", id, got.UTC(), want)
		}
	}
	var days int
	if err := conn.QueryRow(ctx, `SELECT expire_after_days FROM tenants WHERE id = 't1'`).Scan(&days); err != nil || days != 90 {
		t.Fatalf("expire_after_days %d (%v), want 90", days, err)
	}
}
