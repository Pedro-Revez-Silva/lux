package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// steerableFixture is policyFixture's running Run r1 on h1 (h2 ready too),
// an opencode agent whose spec has workload.steerable steerable ("" for
// none).
func steerableFixture(t *testing.T, steerable string) (*Server, context.Context) {
	t.Helper()
	s, ctx := policyFixture(t, "")
	sp := `{"placement": {"pool": "default"}, "workload": {"adapter": "opencode"}}`
	if steerable != "" {
		sp = `{"placement": {"pool": "default"}, "workload": {"adapter": "opencode", "steerable": ` + steerable + `}}`
	}
	execSQL(t, s, ctx, `UPDATE runs SET spec = $1 WHERE id = 'r1'`, sp)
	return s, ctx
}

func wantNotSteerable(t *testing.T, err error) {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_steerable" {
		t.Fatalf("got %v, want 409 not_steerable", err)
	}
}

// inputsQueued counts the input and interrupt messages queued for r1's
// hosts, and its input events.
func inputsQueued(t *testing.T, s *Server) (messages, events int) {
	t.Helper()
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE run_id = 'r1' AND type IN ($1, $2)`,
		[]any{proto.MsgInput, proto.MsgInterrupt}, &messages)
	systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = 'r1' AND type = 'input'`, nil, &events)
	return
}

// No one steers a Run with workload.steerable false, an operator included:
// text and a bare interrupt are refused while it runs, and still refused
// once it is stopped, before resume is suggested. Nothing reaches its host.
func TestUnsteerableRefusesInput(t *testing.T) {
	s, _ := steerableFixture(t, "false")
	for _, c := range []struct {
		name string
		ctx  context.Context
		body inputRequest
	}{
		{"text", tenantCtx("t1"), inputRequest{Text: "approve this"}},
		{"interrupt", tenantCtx("t1"), inputRequest{Interrupt: true}},
		{"operator", operatorCtx(), inputRequest{Text: "approve this", Interrupt: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.postInput(c.ctx, &postInputInput{RunPath: RunPath{ID: "r1"}, Body: c.body})
			wantNotSteerable(t, err)
		})
	}
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	_, err := s.postInput(tenantCtx("t1"), &postInputInput{RunPath: RunPath{ID: "r1"}, Body: inputRequest{Text: "approve this"}})
	wantNotSteerable(t, err)
	if m, e := inputsQueued(t, s); m != 0 || e != 0 {
		t.Fatalf("%d input messages and %d input events, want none", m, e)
	}
}

// workload.steerable omitted or true: input is accepted as ever.
func TestSteerableTakesInput(t *testing.T) {
	for _, steerable := range []string{"", "true"} {
		t.Run("steerable="+steerable, func(t *testing.T) {
			s, _ := steerableFixture(t, steerable)
			if _, err := s.postInput(tenantCtx("t1"), &postInputInput{RunPath: RunPath{ID: "r1"}, Body: inputRequest{Text: "look again"}}); err != nil {
				t.Fatal(err)
			}
			if m, e := inputsQueued(t, s); m != 1 || e != 1 {
				t.Fatalf("%d input messages and %d input events, want one each", m, e)
			}
		})
	}
}

// A resume with input is refused, validateOnly too, and leaves the Run
// stopped; without input it resumes.
func TestUnsteerableResumeInput(t *testing.T) {
	s, ctx := steerableFixture(t, "false")
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
	for _, validateOnly := range []bool{true, false} {
		_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"},
			Body: &resumeRequest{Input: &resumeInput{Text: "approve this"}, ValidateOnly: validateOnly}})
		wantNotSteerable(t, err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
	if state != StateStopped {
		t.Fatalf("after refused resumes: %q, want stopped", state)
	}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}, Body: &resumeRequest{Input: &resumeInput{}}}); err != nil {
		t.Fatalf("resume with empty input: %v", err)
	}
	var pending bool
	systemScan(t, s, `SELECT state, pending_input IS NOT NULL FROM runs WHERE id = 'r1'`, nil, &state, &pending)
	if state != StateResuming || pending {
		t.Fatalf("after resume: %q, pending input %v; want resuming with none", state, pending)
	}
}

// An operator's migrate with input is refused and leaves the Run running
// where it is; without input it moves.
func TestUnsteerableMigrateInput(t *testing.T) {
	s, _ := steerableFixture(t, "false")
	_, err := s.migrateRun(operatorCtx(), &migrateRunInput{RunPath: RunPath{ID: "r1"}, Body: &migrateRequest{Input: &resumeInput{Text: "approve this"}}})
	wantNotSteerable(t, err)
	var stopRequested bool
	systemScan(t, s, `SELECT p.stop_requested_at IS NOT NULL FROM placements p WHERE p.run_id = 'r1'`, nil, &stopRequested)
	if stopRequested {
		t.Fatal("refused migrate stopped the Run")
	}
	if _, err := s.migrateRun(operatorCtx(), &migrateRunInput{RunPath: RunPath{ID: "r1"}, Body: &migrateRequest{To: "h2"}}); err != nil {
		t.Fatalf("migrate without input: %v", err)
	}
	systemScan(t, s, `SELECT p.stop_requested_at IS NOT NULL FROM placements p WHERE p.run_id = 'r1'`, nil, &stopRequested)
	if !stopRequested {
		t.Fatal("migrate without input did not stop the Run to move it")
	}
}

// Through the API as a client meets it: whoami says luxd takes the field,
// a submit keeps it, GET answers it, and input is refused by its code.
func TestSteerableThroughTheAPI(t *testing.T) {
	s := testServer(t)
	key := serversFixture(t, s, context.Background())
	capability := apiCall(t, s, key, http.MethodGet, "/v1/whoami", nil)
	var who Whoami
	if err := json.Unmarshal(capability.Body.Bytes(), &who); err != nil || capability.Code != http.StatusOK || !who.Steerable {
		t.Fatalf("capability: %d %s (%v)", capability.Code, capability.Body, err)
	}
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", map[string]any{
		"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"sleep", "infinity"}, "steerable": false}})
	if w.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	got := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+run.ID, nil)
	var read Run
	if err := json.Unmarshal(got.Body.Bytes(), &read); err != nil || read.Spec.Workload.Steerable == nil || *read.Spec.Workload.Steerable {
		t.Fatalf("GET: %d %s (%v)", got.Code, got.Body, err)
	}
	in := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+run.ID+"/input", map[string]any{"text": "approve this"})
	var out map[string]any
	_ = json.Unmarshal(in.Body.Bytes(), &out)
	if code, _ := errCode(out); in.Code != http.StatusConflict || code != "not_steerable" {
		t.Fatalf("input: %d %s", in.Code, in.Body)
	}
}
