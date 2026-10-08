package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 056: a Run cancelled before it becomes terminated, with its state
// reasons, stop reason, events, queued costs and detached servers; the
// column is terminate_requested; the cost queue refuses state:cancelled.
// Other Runs and reasons are untouched.
func TestRunTerminatedMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "055_host_capabilities"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`,
		`INSERT INTO runs (id, tenant_id, spec, state, state_reason, cancel_requested, current_epoch, state_changed_at) VALUES
			('r_gone', 't1', '{}', 'cancelled', 'cancelled', true, 1, '2026-04-01T00:00:00Z'),
			('r_lost', 't1', '{}', 'cancelled', 'cancelled; host lost', true, 1, '2026-04-02T00:00:00Z'),
			('r_queued', 't1', '{}', 'cancelled', 'cancel', true, 0, '2026-04-03T00:00:00Z'),
			('r_expired', 't1', '{}', 'cancelled', 'expired: stopped for 90 days', false, 1, '2026-04-04T00:00:00Z'),
			('r_stopping', 't1', '{}', 'stopping', 'cancel', true, 1, '2026-04-05T00:00:00Z'),
			('r_ok', 't1', '{}', 'failed', 'exit code 1', false, 1, '2026-04-06T00:00:00Z')`,
		// Cancelled Runs without a state event, and with a finished_at and
		// updated_at other than when they were cancelled: the clock is
		// state_changed_at alone.
		`INSERT INTO runs (id, tenant_id, spec, state, state_reason, cancel_requested, current_epoch, finished_at, updated_at, state_changed_at) VALUES
			('r_noevent', 't1', '{}', 'cancelled', 'cancelled', true, 1, '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z', '2026-01-15T00:00:00Z'),
			('r_bare', 't1', '{}', 'cancelled', 'cancelled', true, 0, NULL, '2026-03-01T00:00:00Z', '2026-02-15T00:00:00Z')`,
		`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, stop_reason) VALUES
			('p_gone', 't1', 'r_gone', 'h1', 1, 'exited', 'cancel'), ('p_ok', 't1', 'r_ok', 'h1', 1, 'exited', 'stop')`,
		`INSERT INTO run_events (tenant_id, run_id, type, data) VALUES
			('t1', 'r_gone', 'cancel.requested', '{"by": "ada"}'),
			('t1', 'r_gone', 'state', '{"state": "stopping", "reason": "cancel"}'),
			('t1', 'r_gone', 'state', '{"state": "cancelled", "reason": "cancelled"}'),
			('t1', 'r_lost', 'state', '{"state": "cancelled", "reason": "cancelled; host lost"}'),
			('t1', 'r_queued', 'state', '{"state": "cancelled", "reason": "cancel"}'),
			('t1', 'r_expired', 'state', '{"state": "cancelled", "reason": "expired: stopped for 90 days"}'),
			('t1', 'r_gone', 'server.detached', '{"reason": "run cancelled"}'),
			('t1', 'r_gone', 'server.deleted', '{"reason": "run cancelled"}'),
			('t1', 'r_ok', 'state', '{"state": "failed", "reason": "exit code 1"}')`,
		`INSERT INTO host_events (tenant_id, host_id, type, data) VALUES
			('t1', 'h1', 'host.placement_ended', '{"run": "r_gone", "outcome": "cancelled", "stopReason": "cancel"}'),
			('t1', 'h1', 'host.placement_ended', '{"run": "r_ok", "outcome": "stopped", "stopReason": "stop"}'),
			('t1', 'h1', 'host.placement_ended', '{"run": "r_ok", "outcome": "stopped", "stopReason": "cancel"}')`,
		`INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r_gone', now(), 'state:cancelled'), ('r_ok', now(), 'state:failed')`,
		`INSERT INTO run_servers (id, tenant_id, run_id, name, port, state, stop_reason) VALUES
			('srv_aaaaaaaaaaaaaaaa', 't1', NULL, 'web', 3000, 'stopped', 'run cancelled')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}

	rows, err := conn.Query(ctx, `SELECT id, state, state_reason, terminate_requested FROM runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type run struct {
		ID, State, Reason string
		Terminate         bool
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[run])
	if err != nil {
		t.Fatal(err)
	}
	want := []run{
		{"r_bare", "terminated", "terminated", true},
		{"r_expired", "terminated", "expired: stopped for 90 days", false},
		{"r_gone", "terminated", "terminated", true},
		{"r_lost", "terminated", "terminated; host lost", true},
		{"r_noevent", "terminated", "terminated", true},
		{"r_ok", "failed", "exit code 1", false},
		{"r_queued", "terminated", "terminated", true},
		{"r_stopping", "stopping", "terminate", true},
	}
	if len(got) != len(want) {
		t.Fatalf("runs: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("run %d: %+v, want %+v", i, got[i], want[i])
		}
	}

	text := func(q string) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	if stops := text(`SELECT string_agg(id || '=' || stop_reason, ' ' ORDER BY id) FROM placements`); stops != "p_gone=terminate p_ok=stop" {
		t.Errorf("stop reasons: %q", stops)
	}
	events := text(`SELECT string_agg(run_id || ' ' || type || ' ' || data::text, '; ' ORDER BY id) FROM run_events`)
	wantEvents := `r_gone terminate.requested {"by": "ada"}; ` +
		`r_gone state {"state": "stopping", "reason": "terminate"}; ` +
		`r_gone state {"state": "terminated", "reason": "terminated"}; ` +
		`r_lost state {"state": "terminated", "reason": "terminated; host lost"}; ` +
		`r_queued state {"state": "terminated", "reason": "terminated"}; ` +
		`r_expired state {"state": "terminated", "reason": "expired: stopped for 90 days"}; ` +
		`r_gone server.detached {"reason": "run terminated"}; ` +
		`r_gone server.deleted {"reason": "run terminated"}; ` +
		`r_ok state {"state": "failed", "reason": "exit code 1"}`
	if events != wantEvents {
		t.Errorf("run events:\n got %s\nwant %s", events, wantEvents)
	}
	if hostEvents := text(`SELECT string_agg((data->>'outcome') || '/' || (data->>'stopReason'), ' ' ORDER BY id) FROM host_events`); hostEvents != "terminated/terminate stopped/stop stopped/terminate" {
		t.Errorf("host events: %q", hostEvents)
	}
	if pending := text(`SELECT string_agg(run_id || '=' || reason, ' ' ORDER BY run_id) FROM cost_pending`); pending != "r_gone=state:terminated r_ok=state:failed" {
		t.Errorf("cost queue: %q", pending)
	}
	if serverStop := text(`SELECT stop_reason FROM run_servers WHERE id = 'srv_aaaaaaaaaaaaaaaa'`); serverStop != "run terminated" {
		t.Errorf("server stop reason: %q", serverStop)
	}
	// The queue takes the new reason and refuses the old one.
	if _, err := conn.Exec(ctx, `UPDATE cost_pending SET reason = 'state:terminated' WHERE run_id = 'r_ok'`); err != nil {
		t.Errorf("state:terminated refused: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE cost_pending SET reason = 'state:cancelled' WHERE run_id = 'r_ok'`); err == nil {
		t.Error("state:cancelled accepted")
	}
	// terminated_at: each formerly cancelled Run's state_changed_at, not
	// its finished_at or updated_at (r_noevent, r_bare); none for Runs
	// that are not terminated.
	clocks := text(`SELECT string_agg(id || '=' || coalesce(to_char(terminated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), '-'), ' ' ORDER BY id) FROM runs`)
	if want := "r_bare=2026-02-15 r_expired=2026-04-04 r_gone=2026-04-01 r_lost=2026-04-02 " +
		"r_noevent=2026-01-15 r_ok=- r_queued=2026-04-03 r_stopping=-"; clocks != want {
		t.Errorf("terminated_at:\n got %s\nwant %s", clocks, want)
	}
	if orphans := text(`SELECT count(*)::text FROM runs WHERE state = 'terminated' AND terminated_at IS NULL`); orphans != "0" {
		t.Errorf("%s terminated Runs without a retention clock", orphans)
	}
}
