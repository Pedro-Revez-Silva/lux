package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 017 fills run_sessions from what luxd recorded before it: session events,
// snapshot manifests and runs.session_id, one row per (run, epoch, id) with
// the earliest and latest time it was seen.
func TestRunSessionsBackfill(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "016_control_samples"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`,
		`INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`,
		// r1: epoch 1 had "a" (reported twice), epoch 2 "b"; a snapshot of
		// epoch 2 carries "c" (never an event). runs.session_id is "b".
		`INSERT INTO runs (id, tenant_id, spec, state, current_epoch, session_id) VALUES
			('r1', 't1', '{}', 'running', 2, 'b'),
			('r2', 't2', '{}', 'stopped', 1, 'only-on-run'),
			('r3', 't1', '{}', 'submitted', 0, '')`,
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
			('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'running')`,
		fmt.Sprintf(`INSERT INTO run_events (tenant_id, run_id, epoch, type, data, created_at) VALUES
			('t1', 'r1', 1, 'session', '{"sessionId": "a"}', '%[1]s'),
			('t1', 'r1', 1, 'session', '{"sessionId": "a"}', '%[2]s'),
			('t1', 'r1', 2, 'session', '{"sessionId": "b"}', '%[3]s'),
			('t1', 'r1', 2, 'activity', '{"activity": "busy"}', '%[3]s')`,
			t0.Format(time.RFC3339), t0.Add(time.Minute).Format(time.RFC3339), t0.Add(time.Hour).Format(time.RFC3339)),
		fmt.Sprintf(`INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, created_at) VALUES
			('s1', 't1', 'r1', 'p2', 2, '{"sessionId": "c"}', '%[1]s'),
			('s0', 't1', 'r1', 'p1', 1, '{"sessionId": ""}', '%[1]s')`, t0.Add(2*time.Hour).Format(time.RFC3339)),
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	rows, _ := conn.Query(ctx, `SELECT tenant_id, run_id, epoch, session_id, first_seen, last_seen FROM run_sessions ORDER BY run_id, epoch, session_id`)
	var got []string
	var tenant, run, id string
	var epoch int
	var first, last time.Time
	_, err = pgx.ForEachRow(rows, []any{&tenant, &run, &epoch, &id, &first, &last}, func() error {
		got = append(got, fmt.Sprintf("%s/%s/%d/%s", tenant, run, epoch, id))
		if run == "r1" && id == "a" && (!first.Equal(t0) || !last.Equal(t0.Add(time.Minute))) {
			return fmt.Errorf("a seen %v..%v, want %v..%v", first, last, t0, t0.Add(time.Minute))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[t1/r1/1/a t1/r1/2/b t1/r1/2/c t2/r2/1/only-on-run]"
	if fmt.Sprint(got) != want {
		t.Fatalf("backfill:\n got %v\nwant %s", got, want)
	}
}
