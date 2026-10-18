package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// idleStep changes r1 before the reaper runs.
type idleStep func(*testing.T, *Server, context.Context)

// idleFixture is r1 running idle at epoch 1 for an hour with a 1m
// idleTimeout, its idle report two minutes old: due unless a step changes it.
func idleFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"workload":{"adapter":"acp","idleTimeout":"1m"}}', first_started_at = now() - interval '1 hour', activity = 'idle' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '1 hour' WHERE id = 'p1'`)
	idleActivity("idle")(t, s, ctx)
	idleAged(t, s, ctx)
	return s, ctx
}

// idleAged moves every r1 event two minutes back, past the 1m timeout.
func idleAged(t *testing.T, s *Server, _ context.Context) {
	t.Helper()
	ownerExec(t, s, `UPDATE run_events SET created_at = now() - interval '2 minutes' WHERE run_id = 'r1'`)
}

func idleAdapterEvent(epoch int, ev proto.AdapterEvent) idleStep {
	return func(t *testing.T, s *Server, ctx context.Context) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applyAdapterEvent(ctx, tx, "t1", "r1", epoch, ev)
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func idleActivity(a string) idleStep { return idleAdapterEvent(1, proto.AdapterEvent{Activity: a}) }

func idleDelivered(receipt bool) idleStep {
	return idleAdapterEvent(1, proto.AdapterEvent{InputAck: "req-1", InputPhase: proto.InputAccepted, InputLands: "next_step", InputReceipt: receipt})
}

func idleInput(epoch int, data map[string]any) idleStep {
	return func(t *testing.T, s *Server, ctx context.Context) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return addEvent(ctx, tx, "t1", "r1", epoch, "input", data)
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func idleProgress(phase string) idleStep {
	return func(t *testing.T, s *Server, ctx context.Context) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return applyInputProgress(ctx, tx, "t1", "r1", 1, proto.InputProgress{RequestID: "req-1", Phase: phase})
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func idlePost(body inputRequest) idleStep {
	return func(t *testing.T, s *Server, ctx context.Context) {
		t.Helper()
		actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"run"}})
		if _, err := s.postInput(actx, &postInputInput{RunPath: RunPath{ID: "r1"}, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
}

func idleSQL(q string) idleStep {
	return func(t *testing.T, s *Server, ctx context.Context) { t.Helper(); execSQL(t, s, ctx, q) }
}

type idleCase struct {
	name  string
	steps []idleStep
	stop  bool
}

// runIdleCases runs each case on a fresh idleFixture: setup, then its
// steps, then one reaper pass.
func runIdleCases(t *testing.T, setup []idleStep, cases []idleCase) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := idleFixture(t)
			for _, step := range append(append([]idleStep{}, setup...), c.steps...) {
				step(t, s, ctx)
			}
			assertIdleReap(t, s, ctx, c.stop)
		})
	}
}

// assertIdleReap runs the reaper and checks the stop of r1's current placement.
func assertIdleReap(t *testing.T, s *Server, ctx context.Context, stopped bool) {
	t.Helper()
	if err := s.reapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	var got bool
	var reason string
	systemScan(t, s, `SELECT p.stop_requested_at IS NOT NULL, p.stop_reason FROM placements p
		JOIN runs r ON r.id = p.run_id AND p.epoch = r.current_epoch WHERE r.id = 'r1'`, nil, &got, &reason)
	if got != stopped || (stopped && reason != "idle") {
		t.Fatalf("stop=%v reason=%q, want stop=%v idle", got, reason, stopped)
	}
}

// exitRun reports r1's placement exited and returns the Run's state and reason.
func idleExit(t *testing.T, s *Server, ctx context.Context) (state, reason string) {
	t.Helper()
	code := 0
	f := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, ID: 1, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code})})
	if f.Type != proto.MsgAck {
		t.Fatalf("exit: %s %s", f.Type, f.Data)
	}
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	return state, reason
}

func TestIdleReaperStopsOnceAndEndsStopped(t *testing.T) {
	s, ctx := idleFixture(t)
	assertIdleReap(t, s, ctx, true)
	assertIdleReap(t, s, ctx, true)
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_messages WHERE run_id = 'r1' AND type = 'stop'`); n != 1 {
		t.Fatalf("stop messages=%d", n)
	}
	if seconds := queryOne[float64](t, s, `SELECT (data->>'idleSeconds')::float8 FROM run_events WHERE run_id = 'r1' AND type = 'idle'`); seconds < 120 {
		t.Fatalf("idle seconds=%v", seconds)
	}
	if state, reason := idleExit(t, s, ctx); state != StateStopped || reason != "idle" || !resumable(state) {
		t.Fatalf("state=%s reason=%s", state, reason)
	}
	if r := queryOne[string](t, s, `SELECT data->>'reason' FROM run_events WHERE run_id = 'r1' AND type = 'state' AND data->>'state' = 'stopped'`); r != "idle" {
		t.Fatalf("stopped state event reason=%q", r)
	}
}

func TestIdleTimeoutGenericSubmission(t *testing.T) {
	s, _ := costFixture(t)
	key := apiKey(t, s, new("t1"), "run", "read")
	response := submitHTTP(t, s, key, "idle-generic", json.RawMessage(`{"image":{"ref":"alpine"},"workload":{"adapter":"generic","command":["true"],"idleTimeout":"1m"}}`))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_spec") || !strings.Contains(response.Body.String(), "workload.idleTimeout") {
		t.Fatalf("submission: %d %s", response.Code, response.Body)
	}
}

func TestRuntimeTimeoutStillFails(t *testing.T) {
	s, ctx := idleFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"timeout":"1m"}' WHERE id = 'r1'`)
	if err := s.reapTimeouts(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := idleExit(t, s, ctx); state != StateFailed || reason != "timeout" {
		t.Fatalf("state=%s reason=%s", state, reason)
	}
}

func TestIdleReaperClockAndGuards(t *testing.T) {
	runIdleCases(t, nil, []idleCase{
		{"due", nil, true},
		{"repeated idle", []idleStep{idleActivity("idle")}, true},
		{"busy", []idleStep{idleActivity("busy")}, false},
		{"busy resets", []idleStep{idleActivity("busy"), idleActivity("idle")}, false},
		{"no timeout", []idleStep{idleSQL(`UPDATE runs SET spec = '{}' WHERE id = 'r1'`)}, false},
		{"zero", []idleStep{idleSQL(`UPDATE runs SET spec = '{"workload":{"idleTimeout":"0s"}}' WHERE id = 'r1'`)}, false},
		{"fresh placement", []idleStep{idleSQL(`UPDATE placements SET started_at = now() WHERE id = 'p1'`)}, false},
		{"already stopping", []idleStep{idleSQL(`UPDATE runs SET state = 'stopping' WHERE id = 'r1'`)}, false},
		// The first prompt has no input event; its receipt alone keeps it pending.
		{"first prompt delivered", []idleStep{idleAdapterEvent(1, proto.AdapterEvent{InputAck: "prompt", InputPhase: proto.InputAccepted, InputLands: "next_step", InputReceipt: true}), idleAged}, false},
	})
}

// A stop requested before runs.state moves to stopping is not stopped again.
func TestIdleReaperSkipsRequestedStop(t *testing.T) {
	s, ctx := idleFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET stop_requested_at = now(), stop_reason = 'stop' WHERE id = 'p1'`)
	if err := s.reapIdle(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	var stops int
	systemScan(t, s, `SELECT r.state, p.stop_reason, (SELECT count(*) FROM host_messages WHERE run_id = 'r1' AND type = 'stop')
		FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`, nil, &state, &reason, &stops)
	if state != StateRunning || reason != "stop" || stops != 0 {
		t.Fatalf("state=%s reason=%s stop messages=%d", state, reason, stops)
	}
}

// A bare interrupt never reports an input phase; one carrying content does.
func TestIdleReaperInterruptInput(t *testing.T) {
	image := []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: b64(tinyPNG)}}
	runIdleCases(t, nil, []idleCase{
		{"interrupt only", []idleStep{idlePost(inputRequest{Interrupt: true}), idleAged}, true},
		{"interrupt only moves clock", []idleStep{idlePost(inputRequest{Interrupt: true})}, false},
		{"interrupt with text", []idleStep{idlePost(inputRequest{Interrupt: true, Text: "stop and look"}), idleAged}, false},
		{"interrupt with attachments", []idleStep{idlePost(inputRequest{Interrupt: true, Attachments: image}), idleAged}, false},
	})
}

// A resumed placement (epoch 2) is judged on its own events only.
func TestIdleReaperResumedEpoch(t *testing.T) {
	resumed := []idleStep{
		idleSQL(`UPDATE placements SET state = 'exited' WHERE id = 'p1'`),
		idleSQL(`INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at) VALUES ('p2', 't1', 'r1', 'h1', 2, 'running', now() - interval '1 hour')`),
		idleSQL(`UPDATE runs SET current_epoch = 2 WHERE id = 'r1'`),
		idleAdapterEvent(2, proto.AdapterEvent{Activity: "idle"}),
		idleAged,
	}
	runIdleCases(t, resumed, []idleCase{
		{"old undelivered input", []idleStep{idleInput(1, map[string]any{"requestId": "req-old"}), idleAged}, true},
		{"old input", []idleStep{idleInput(1, map[string]any{"requestId": "req-old", "interrupt": true, "text": "", "rawBytes": 0})}, true},
		{"old turn", []idleStep{idleActivity("busy"), idleActivity("idle")}, true},
		{"new turn", []idleStep{idleAdapterEvent(2, proto.AdapterEvent{Activity: "busy"}), idleAdapterEvent(2, proto.AdapterEvent{Activity: "idle"})}, false},
	})
}

func TestIdleReaperPendingInput(t *testing.T) {
	input := idleInput(1, map[string]any{"requestId": "req-1"})
	runIdleCases(t, nil, []idleCase{
		{"undelivered", []idleStep{input, idleAged}, false},
		{"undelivered turn", []idleStep{input, idleActivity("busy"), idleActivity("idle"), idleAged}, false},
		{"delivered", []idleStep{input, idleDelivered(true), idleAged}, false},
		{"consumed", []idleStep{input, idleDelivered(true), idleProgress("consumed"), idleAged}, true},
		{"failed", []idleStep{input, idleDelivered(true), idleProgress("failed"), idleAged}, true},
		{"no receipt", []idleStep{input, idleDelivered(false), idleAged}, false},
		{"no receipt busy", []idleStep{input, idleDelivered(false), idleActivity("busy"), idleActivity("idle"), idleAged}, true},
		// The steer joins a running turn: busy precedes the input and no busy follows it.
		{"no receipt steer", []idleStep{idleActivity("busy"), input, idleDelivered(false), idleActivity("idle"), idleAged}, true},
		// The turn ends before its receipt-free delivery is recorded.
		{"no receipt short turn", []idleStep{input, idleActivity("busy"), idleActivity("idle"), idleDelivered(false), idleAged}, true},
		{"new input", []idleStep{input}, false},
		{"new consumed input", []idleStep{input, idleDelivered(true), idleProgress("consumed")}, false},
	})
}
