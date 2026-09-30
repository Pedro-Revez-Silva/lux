package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 040 records a launch outcome only where history is unambiguous: a
// terminated, provisioned host whose reason says its launch failed and that
// never had an instance or registered is failed (its reason kept); one with
// an instance is launched; a self-registered host has none.
func TestLaunchOutcomeMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "039_luxd_alive"); err != nil {
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
	// Runs queued now: since their last placement's end, else creation.
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO runs (id, tenant_id, spec, state, created_at) VALUES
			('queued-again', 't1', '{}', 'provisioning', '2026-01-01 00:00:00+00'),
			('submitted', 't1', '{}', 'submitted', '2026-01-02 00:00:00+00'),
			('resuming', 't1', '{}', 'resuming', '2026-01-03 00:00:00+00'),
			('running', 't1', '{}', 'running', '2026-01-04 00:00:00+00');
		INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, ended_at) VALUES
			('p1', 't1', 'queued-again', 'never-registered', 1, 'exited', '2026-01-01 01:00:00+00'),
			('p2', 't1', 'queued-again', 'never-registered', 2, 'lost', '2026-01-01 02:00:00+00'),
			('p3', 't1', 'running', 'never-registered', 1, 'running', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	for id, w := range map[string]string{
		"queued-again": "2026-01-01 02:00:00+00",
		"submitted":    "2026-01-02 00:00:00+00",
		"resuming":     "2026-01-03 00:00:00+00",
		"running":      "<nil>",
	} {
		var got string
		if err := conn.QueryRow(ctx, `SELECT coalesce(to_char(needs_host_since AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00', '<nil>') FROM runs WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("run %s: needs_host_since %s, want %s", id, got, w)
		}
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
