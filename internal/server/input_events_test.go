package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Each input's first answer and its later progress become one event each,
// however often the runner reports them (it re-reports what it tails after
// a restart or reconnect); a report from an older runner, with no phase,
// reads as it always did.
func TestInputPhasesRecordedOnce(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	consumed := &proto.InputProgress{RequestID: "req-1", Phase: proto.InputConsumed}
	dropped := &proto.InputProgress{RequestID: "req-3", Phase: proto.InputFailed, Error: "the Run stopped before the agent read it"}
	reports := []proto.AdapterEvent{
		{InputAck: "req-1", InputPhase: proto.InputAccepted, InputText: "hi", InputLands: "next_step", InputReceipt: true},
		{InputAck: "req-1", InputPhase: proto.InputAccepted, InputText: "hi", InputLands: "next_step", InputReceipt: true},
		{InputProgress: consumed},
		{InputProgress: consumed},
		{InputAck: "req-2", InputPhase: proto.InputFailed, InputError: "refused"},
		{InputAck: "req-3", InputPhase: proto.InputAccepted, InputText: "x", InputLands: "next_step", InputReceipt: true},
		{InputProgress: dropped},
		{InputProgress: dropped},
		{InputAck: "old", InputText: "x"},
		{InputAck: "old-bad", InputError: "workload not reachable"},
	}
	for _, ev := range reports {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applyAdapterEvent(ctx, tx, "t1", "r1", 2, ev)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		Type string
		Data map[string]any
	}
	var got []row
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT type, data FROM run_events WHERE run_id = 'r1' AND type LIKE 'input.%' ORDER BY id`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `[{"Type":"input.delivered","Data":{"lands":"next_step","phase":"accepted","receipt":true,"requestId":"req-1","text":"hi"}},` +
		`{"Type":"input.consumed","Data":{"requestId":"req-1"}},` +
		`{"Type":"input.failed","Data":{"error":"refused","phase":"failed","requestId":"req-2"}},` +
		`{"Type":"input.delivered","Data":{"lands":"next_step","phase":"accepted","receipt":true,"requestId":"req-3","text":"x"}},` +
		`{"Type":"input.failed","Data":{"error":"the Run stopped before the agent read it","requestId":"req-3"}},` +
		`{"Type":"input.delivered","Data":{"phase":"accepted","requestId":"old","text":"x"}},` +
		`{"Type":"input.failed","Data":{"error":"workload not reachable","phase":"failed","requestId":"old-bad"}}]`
	if string(b) != want {
		t.Fatalf("events\n got %s\nwant %s", b, want)
	}
}

// A luxd that predates InputProgress (it decodes AdapterEvent with
// encoding/json, internal/server/runner.go:429, and knew only InputAck)
// sees a progress report as an empty event: no second delivery.
func TestInputProgressInvisibleToOldDecoders(t *testing.T) {
	b, _ := json.Marshal(proto.AdapterEvent{InputProgress: &proto.InputProgress{RequestID: "req-1", Phase: proto.InputConsumed}})
	var old struct {
		SessionID  string `json:"sessionId,omitempty"`
		Activity   string `json:"activity,omitempty"`
		InputAck   string `json:"inputAck,omitempty"`
		InputError string `json:"inputError,omitempty"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.InputAck != "" || old.SessionID != "" || old.Activity != "" {
		t.Fatalf("an old luxd reads %s as %+v (%v)", b, old, err)
	}
}

// costTx counts the buffer pages each statement run through it touches:
// every statement is first run under EXPLAIN (ANALYZE, BUFFERS) in a
// savepoint that is rolled back, then run for real.
type costTx struct {
	pgx.Tx
	t      *testing.T
	blocks int
	plans  []string
}

func (c *costTx) measure(ctx context.Context, sql string, args ...any) {
	sp, err := c.Tx.Begin(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	defer sp.Rollback(ctx)
	var out []map[string]any
	if err := sp.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&out); err != nil {
		c.t.Fatal(err)
	}
	p := out[0]["Plan"].(map[string]any)
	c.blocks += int(p["Shared Hit Blocks"].(float64) + p["Shared Read Blocks"].(float64))
	b, _ := json.Marshal(out)
	c.plans = append(c.plans, string(b))
}

func (c *costTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.measure(ctx, sql, args...)
	return c.Tx.Exec(ctx, sql, args...)
}

func (c *costTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.measure(ctx, sql, args...)
	return c.Tx.QueryRow(ctx, sql, args...)
}

// inputEventCost is how many buffer pages luxd touches recording one more
// input event on a Run with events of every kind in its history.
func inputEventCost(t *testing.T, s *Server, ctx context.Context, runID string, events int) (blocks int, plans []string) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ($1, 't1', '{}', 'running', 1)`, runID)
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data)
		SELECT 't1', $1, 1, (ARRAY['activity', 'session', 'input.delivered', 'input.consumed'])[1 + i % 4],
			jsonb_build_object('requestId', 'req-' || i, 'activity', 'busy', 'pad', repeat('x', 200))
		FROM generate_series(1, $2::int) i`, runID, events)
	for _, typ := range []string{"input.delivered", "input.consumed"} {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applyAdapterEvent(ctx, tx, "t1", runID, 1, proto.AdapterEvent{InputAck: "seed-" + typ, InputPhase: proto.InputAccepted})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, s, ctx, `ANALYZE`)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		c := &costTx{Tx: tx, t: t}
		err := s.applyAdapterEvent(ctx, c, "t1", runID, 1, proto.AdapterEvent{InputProgress: &proto.InputProgress{RequestID: "req-new", Phase: proto.InputConsumed}})
		blocks, plans = c.blocks, c.plans
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return blocks, plans
}

// Recording an input event once does not read the Run's history: the
// pages it touches do not grow with it.
func TestInputEventDedupeIsAKeyLookup(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	small, _ := inputEventCost(t, s, ctx, "r-small", 100)
	big, plans := inputEventCost(t, s, ctx, "r-big", 50000)
	t.Logf("100 events: %d pages; 50000 events: %d pages", small, big)
	for _, p := range plans {
		t.Log(p)
	}
	if big > small+10 {
		t.Fatalf("recording an input event touched %d pages on a Run with 50000 events, %d with 100", big, small)
	}
}
