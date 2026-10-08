package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// POST /v1/runs/{id}/terminate and its deprecated alias /cancel both end a
// Run terminated, through the HTTP routes: a live one is asked to stop
// (a terminate message, reason terminate) and ends terminated once its
// placement exits; a stopped one ends terminated at once; one whose host is
// lost before its placement exits ends terminated too. Either way its held
// secrets go, and the request is recorded as terminate.requested.
func TestTerminateRoutes(t *testing.T) {
	for _, path := range []string{"terminate", "cancel"} {
		t.Run(path+"/live", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			postR1(t, s, path)
			var msgType, stopReason, runState, runReason string
			var requested bool
			systemScan(t, s, `SELECT (SELECT type FROM host_messages WHERE run_id = 'r1' AND type IN ($1, $2)),
					p.stop_reason, r.state, r.state_reason, r.terminate_requested
				FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
				[]any{proto.MsgStop, proto.MsgTerminate}, &msgType, &stopReason, &runState, &runReason, &requested)
			if msgType != proto.MsgTerminate || stopReason != "terminate" || runState != StateStopping || runReason != "terminate" || !requested {
				t.Fatalf("after %s: message %q stop %q state %q %q requested %v", path, msgType, stopReason, runState, runReason, requested)
			}
			exitR1(t, s)
			assertTerminated(t, s, "terminated")
		})
		t.Run(path+"/stopped", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			exitR1(t, s)
			s.secrets.put("r1", map[string]string{"TOKEN": "jit"})
			postR1(t, s, path)
			assertTerminated(t, s, "terminated")
			// Idempotent: a second terminate changes nothing.
			var before string
			systemScan(t, s, `SELECT updated_at::text FROM runs WHERE id = 'r1'`, nil, &before)
			postR1(t, s, path)
			var after string
			systemScan(t, s, `SELECT updated_at::text FROM runs WHERE id = 'r1'`, nil, &after)
			if after != before {
				t.Fatal("a second terminate wrote the Run")
			}
			assertTerminated(t, s, "terminated")
		})
		t.Run(path+"/unknown", func(t *testing.T) {
			s, ctx := policyFixture(t, "")
			execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
			for _, tenant := range []string{"t1", "t2"} {
				key := apiKey(t, s, new(tenant), "run")
				// t1: no such Run; t2: r1 is another tenant's.
				id := map[string]string{"t1": "nope", "t2": "r1"}[tenant]
				code, body := call(t, s, key, http.MethodPost, "/v1/runs/"+id+"/"+path, nil)
				if code != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
					t.Errorf("%s %s as %s: %d %s, want 404 not_found", path, id, tenant, code, body)
				}
			}
			var state string
			systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
			if state != StateRunning {
				t.Fatalf("r1 after another tenant's %s: %q", path, state)
			}
		})
		t.Run(path+"/twice-while-stopping", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			postR1(t, s, path)
			postR1(t, s, path)
			var state string
			var requests int
			systemScan(t, s, `SELECT state, (SELECT count(*) FROM run_events WHERE run_id = 'r1' AND type = 'terminate.requested')
				FROM runs WHERE id = 'r1'`, nil, &state, &requests)
			// Each request is recorded and asks the runner again; the Run
			// stays stopping until its placement exits.
			if state != StateStopping || requests != 2 {
				t.Fatalf("state %q terminate.requested %d, want stopping 2", state, requests)
			}
			exitR1(t, s)
			systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
			if state != StateTerminated {
				t.Fatalf("after exit: %q", state)
			}
		})
		t.Run(path+"/host-lost", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			postR1(t, s, path)
			// The host goes before the placement reports its exit.
			loseR1Lease(t, s)
			assertTerminated(t, s, "terminated; host lost")
		})
		t.Run(path+"/succeeded", func(t *testing.T) {
			s, _ := policyFixture(t, "")
			exitR1With(t, s, 0)
			postR1(t, s, path)
			assertTerminated(t, s, "terminated")
		})
	}
}

// postR1 posts POST /v1/runs/r1/<action> as t1 and expects 202.
func postR1(t *testing.T, s *Server, action string) {
	t.Helper()
	key := apiKey(t, s, new("t1"), "run")
	if code, body := call(t, s, key, http.MethodPost, "/v1/runs/r1/"+action, nil); code != http.StatusAccepted {
		t.Fatalf("%s: %d %s", action, code, body)
	}
}

// loseR1Lease expires p1's lease and reaps it: r1's host is lost.
func loseR1Lease(t *testing.T, s *Server) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE placements SET lease_expires_at = now() - interval '1 minute' WHERE id = 'p1'`)
	if err := s.reapLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// assertTerminated checks r1 ended terminated with reason, its secrets
// dropped and one terminate.requested event.
func assertTerminated(t *testing.T, s *Server, reason string) {
	t.Helper()
	var state, got string
	var requests int
	systemScan(t, s, `SELECT state, state_reason, (SELECT count(*) FROM run_events WHERE run_id = 'r1' AND type = 'terminate.requested')
		FROM runs WHERE id = 'r1'`, nil, &state, &got, &requests)
	if _, held := s.secrets.get("r1"); state != StateTerminated || got != reason || requests != 1 || held {
		t.Fatalf("state %q reason %q terminate.requested %d secrets held %v; want terminated %q 1 false", state, got, requests, held, reason)
	}
}
