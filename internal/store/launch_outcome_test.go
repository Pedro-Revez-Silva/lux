package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 039 records a launch outcome only where history is unambiguous: a
// terminated, provisioned host whose reason says its launch failed and that
// never had an instance or registered is failed (its reason kept); one with
// an instance is launched; a self-registered host has none.
func TestLaunchOutcomeMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "038_pool_id"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO hosts (id, name, state, state_reason, provision_requested_at, provider_id, registered_at, terminated_at) VALUES
		('refused', 'a', 'terminated', 'launch failed: InsufficientInstanceCapacity', now() - interval '2 hours', NULL, NULL, now() - interval '2 hours'),
		('with-instance', 'b', 'terminated', 'launch failed: odd', now() - interval '2 hours', 'i-1', NULL, now()),
		('registered', 'c', 'terminated', 'launch failed: odd', now() - interval '2 hours', NULL, now(), now()),
		('never-registered', 'd', 'terminated', 'never registered', now() - interval '2 hours', 'i-2', NULL, now()),
		('in-flight', 'e', 'provisioning', '', now(), NULL, NULL, NULL),
		('static', 'f', 'terminated', 'launch failed: typed by hand', NULL, NULL, now(), now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"refused":          "failed|InsufficientInstanceCapacity|launch failed: InsufficientInstanceCapacity|true",
		"with-instance":    "launched||launch failed: odd|false",
		"registered":       "<nil>||launch failed: odd|false",
		"never-registered": "launched||never registered|false",
		"in-flight":        "requested|||false",
		"static":           "<nil>||launch failed: typed by hand|false",
	}
	for id, w := range want {
		var got string
		if err := conn.QueryRow(ctx, `SELECT coalesce(launch_outcome, '<nil>') || '|' || coalesce(launch_error, '') || '|' || state_reason || '|' ||
			(launch_finished_at IS NOT DISTINCT FROM terminated_at AND launch_finished_at IS NOT NULL)::text FROM hosts WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: %s, want %s", id, got, w)
		}
	}
}
