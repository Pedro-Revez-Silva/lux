package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// policyFixture is a running Run r1 on h1 (h2 ready too) whose spec has
// resumePolicy policy ("" for none), with a server and secret values in
// luxd's cache.
func policyFixture(t *testing.T, policy string) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	namedPools(t, s, "default")
	ctx := context.Background()
	sp := `{"placement": {"pool": "default"}}`
	if policy != "" {
		sp = `{"placement": {"pool": "default"}, "resumePolicy": "` + policy + `"}`
	}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	readyHost(t, s, "h1", "default", "", false)
	readyHost(t, s, "h2", "default", "", false)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', $1, 'running', 1)`, sp)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)
	execSQL(t, s, ctx, `INSERT INTO run_servers (id, tenant_id, run_id, name, port, command, state, epoch)
		VALUES ('srv_webwebwebwebwebw', 't1', 'r1', 'web', 3000, '["serve"]', 'ready', 1)`)
	s.secrets.put("r1", map[string]string{"TOKEN": "jit"})
	return s, ctx
}

func operatorCtx() context.Context {
	return context.WithValue(context.Background(), principalKey, Principal{Operator: true, TenantID: "t1", Scopes: []string{"admin", "operator"}})
}

// moveStops stops r1 the way each move does.
var moveStops = map[string]func(t *testing.T, s *Server) error{
	"preempt": func(t *testing.T, s *Server) error {
		return s.hostEvicting(context.Background(), "h1", proto.Evicting{Reason: "spot"})
	},
	"drain": func(t *testing.T, s *Server) error {
		_, err := s.drainHost(operatorCtx(), &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}})
		return err
	},
	"migrate": func(t *testing.T, s *Server) error {
		_, err := s.migrateRun(operatorCtx(), &migrateRunInput{RunPath: RunPath{ID: "r1"}})
		return err
	},
}

// r1Moving is whether r1's server reads as moving.
func r1Moving(t *testing.T, s *Server) bool {
	t.Helper()
	var rows []serverRow
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		var err error
		rows, err = collectServerRows(tx.Query(context.Background(), serverSelect+`WHERE sv.run_id = 'r1'`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d servers", len(rows))
	}
	return rows[0].Moving
}

// exitR1 reports r1's placement exited as a stopped workload's does, after
// its final snapshot (snapR1, with an agent session) as the runner sends it.
func exitR1(t *testing.T, s *Server) {
	t.Helper()
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapR1", RunID: "r1", Epoch: 1, SessionID: "sess-1",
		Volumes: []proto.VolumeSnapshot{{Name: "work", Path: "/work", BlobID: "b-r1-vol", Size: 10, SHA256: "r1-vol"}}}}
	if got := reportSnapshot(t, s, "h1", "r1", 1, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
		t.Fatalf("snapshot report: %s %s", got.Type, got.Data)
	}
	code := 143
	f := proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "r1", Epoch: 1,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})}
	if got := s.handleReport(context.Background(), "h1", f); got.Type != proto.MsgAck {
		t.Fatalf("exit report: %s %s", got.Type, got.Data)
	}
}

// assignOf is the assignment luxd sent for r1's placement epoch.
func assignOf(t *testing.T, s *Server, epoch int) proto.Assign {
	t.Helper()
	var a proto.Assign
	systemScan(t, s, `SELECT payload FROM host_messages WHERE type = $1 AND run_id = 'r1' AND epoch = $2`,
		[]any{proto.MsgAssign, epoch}, &a)
	return a
}

// With resumePolicy manual or never, a move that stops the Run ends it failed, its
// reason naming the move, with no new placement, its secrets dropped and
// its servers stopped as for any end. With auto or unset it is resumed
// from its snapshot and session; with restart it is placed again as a
// first placement: no snapshot to restore, no session (the adapter's
// start path), its snapshot kept for a resume by hand.
func TestResumePolicyOnMove(t *testing.T) {
	for _, stop := range []string{"preempt", "drain", "migrate"} {
		for _, policy := range []string{"manual", "never", "restart", "auto", ""} {
			t.Run(stop+"/"+policy, func(t *testing.T) {
				failsOnMove := policy == "manual" || policy == "never"
				s, ctx := policyFixture(t, policy)
				err := moveStops[stop](t, s)
				if stop == "migrate" && failsOnMove {
					var he *HTTPError
					if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_movable" {
						t.Fatalf("migrate: %v, want 409 not_movable", err)
					}
					var state string
					var stopRequested bool
					systemScan(t, s, `SELECT r.state, p.stop_requested_at IS NOT NULL FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
						nil, &state, &stopRequested)
					if state != StateRunning || stopRequested {
						t.Fatalf("after a refused migrate: state %q stop requested %v, want running and none", state, stopRequested)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := r1Moving(t, s); got == failsOnMove {
					t.Errorf("stopping for %s: moving %v", stop, got)
				}
				exitR1(t, s)
				// Its upload finished, as the runner would report: a resume
				// elsewhere waits for nothing else.
				execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
				var svStop, queuedReason string
				systemScan(t, s, `SELECT (SELECT coalesce(stop_reason, '') FROM run_servers WHERE run_id = 'r1'), state_reason FROM runs WHERE id = 'r1'`,
					nil, &svStop, &queuedReason)
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}

				var state, reason string
				var placements int
				systemScan(t, s, `SELECT r.state, r.state_reason, (SELECT count(*) FROM placements WHERE run_id = r.id)
					FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &placements)
				_, cached := s.secrets.get("r1")
				if failsOnMove {
					want := stop + ": not resumed (resumePolicy " + policy + ")"
					if state != StateFailed || reason != want || placements != 1 || cached || svStop != "run stopped" {
						t.Errorf("state %q reason %q placements %d secrets cached %v server %q; want failed %q 1 false \"run stopped\"",
							state, reason, placements, cached, svStop, want)
					}
					return
				}
				if state != StateScheduled || placements != 2 || !cached || svStop != "migrated" {
					t.Errorf("state %q reason %q placements %d secrets cached %v server %q; want scheduled again, secrets kept, server migrated",
						state, reason, placements, cached, svStop)
				}
				a := assignOf(t, s, 2)
				if policy == "restart" {
					if queuedReason != "auto-restart after "+stop {
						t.Errorf("queued with reason %q", queuedReason)
					}
					if a.Resume != nil {
						t.Errorf("restart assigned a resume: %+v", a.Resume)
					}
					var available bool
					systemScan(t, s, `SELECT available FROM snapshots WHERE id = 'snapR1'`, nil, &available)
					if !available {
						t.Error("the snapshot a restart skipped was deleted at once")
					}
					return
				}
				if queuedReason != "auto-resume after "+stop {
					t.Errorf("queued with reason %q", queuedReason)
				}
				if a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" || a.Resume.SessionID != "sess-1" {
					t.Errorf("resume assigned %+v, want snapR1 and sess-1", a.Resume)
				}
			})
		}
	}
}

// A restart Run stopped by a person and resumed by hand restores its
// snapshot and session: the policy covers only moves.
func TestResumePolicyRestartResumeByHandRestores(t *testing.T) {
	s, ctx := policyFixture(t, "restart")
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	if a := assignOf(t, s, 2); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" || a.Resume.SessionID != "sess-1" {
		t.Fatalf("resume by hand assigned %+v, want snapR1 and sess-1", a.Resume)
	}
}

// A cordon-only drain stops nothing, whatever the policy: the Run finishes
// where it is.
func TestResumePolicyCordonOnlyDrain(t *testing.T) {
	for _, policy := range []string{"manual", "never"} {
		t.Run(policy, func(t *testing.T) {
			s, _ := policyFixture(t, policy)
			if _, err := s.drainHost(operatorCtx(), &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
				t.Fatal(err)
			}
			var state string
			var stopRequested bool
			systemScan(t, s, `SELECT r.state, p.stop_requested_at IS NOT NULL FROM runs r JOIN placements p ON p.run_id = r.id WHERE r.id = 'r1'`,
				nil, &state, &stopRequested)
			if state != StateRunning || stopRequested {
				t.Fatalf("state %q stop requested %v, want running and none", state, stopRequested)
			}
		})
	}
}

// A resume a person asks for after a manual Run failed is accepted, and
// nothing listed blocks it beforehand.
func TestResumePolicyManualResumableByHand(t *testing.T) {
	s, ctx := policyFixture(t, "manual")
	if err := moveStops["preempt"](t, s); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
	run, err := s.loadRun(ctx, "t1", "r1", false)
	if err != nil {
		t.Fatal(err)
	}
	if rs, err := s.resumability(ctx, "t1", run); err != nil || len(rs.Blockers) != 0 {
		t.Fatalf("resumability %+v %v, want no blockers", rs, err)
	}
	// The fixture's Run declares no secrets, so the resume needs none.
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
	if state != StateResuming {
		t.Fatalf("after resume: %q", state)
	}
}

// A cancel that lands while the Run stops for a move wins over the move,
// whatever the policy: it ends cancelled.
func TestResumePolicyCancelDuringMove(t *testing.T) {
	for _, policy := range []string{"never", "manual", "restart"} {
		t.Run(policy, func(t *testing.T) {
			s, _ := policyFixture(t, policy)
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			if _, err := s.cancelRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			exitR1(t, s)
			var state, reason string
			var placements int
			systemScan(t, s, `SELECT state, state_reason, (SELECT count(*) FROM placements WHERE run_id = 'r1') FROM runs WHERE id = 'r1'`,
				nil, &state, &reason, &placements)
			if state != StateCancelled || reason != "cancelled" || placements != 1 {
				t.Fatalf("state %q reason %q placements %d, want cancelled \"cancelled\" 1", state, reason, placements)
			}
		})
	}
}

// After a restart's new placement uploads its own snapshot, the snapshot
// the restart skipped is reaped and the Run's superseded flag cleared.
func TestResumePolicyRestartSkippedSnapshotIsReaped(t *testing.T) {
	s, ctx := policyFixture(t, "restart")
	if err := moveStops["preempt"](t, s); err != nil {
		t.Fatal(err)
	}
	exitR1(t, s)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
	// testServer has no blob.Store: a blob already gone needs no delete.
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'deleted' WHERE id = 'b-r1-vol'`)
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	var host string
	systemScan(t, s, `SELECT host_id FROM placements WHERE run_id = 'r1' AND epoch = 2`, nil, &host)
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapR2", RunID: "r1", Epoch: 2,
		Volumes: []proto.VolumeSnapshot{{Name: "work", Path: "/work", BlobID: "b-r2-vol", Size: 10, SHA256: "r2-vol"}}}}
	if got := reportSnapshot(t, s, host, "r1", 2, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
		t.Fatalf("snapshot report: %s %s", got.Type, got.Data)
	}
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR2'`)
	if err := s.reapSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	var available, superseded bool
	systemScan(t, s, `SELECT (SELECT available FROM snapshots WHERE id = 'snapR1'), snapshots_superseded FROM runs WHERE id = 'r1'`,
		nil, &available, &superseded)
	if available || superseded {
		t.Fatalf("snapR1 available %v, Run superseded %v; want reaped and the flag cleared", available, superseded)
	}
}

// GET /v1/runs?resumable=true leaves out a failed never Run and lists a
// failed manual one.
func TestResumePolicyResumableList(t *testing.T) {
	for policy, listed := range map[string]bool{"never": false, "manual": true} {
		t.Run(policy, func(t *testing.T) {
			s, ctx := policyFixture(t, policy)
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			exitR1(t, s)
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
			out, err := s.listRuns(tenantCtx("t1"), &listRunsInput{Resumable: true})
			if err != nil {
				t.Fatal(err)
			}
			got := slices.ContainsFunc(out.Body.Runs, func(r *Run) bool { return r.ID == "r1" })
			if got != listed {
				t.Fatalf("r1 listed %v, want %v", got, listed)
			}
		})
	}
}

// Every requested resume of a never Run is refused, by its tenant or an
// operator, whether it failed after a move, was stopped by request or is
// still running (the refusal, not "stop it first"), and nothing changes:
// no state, no secrets or spec written, no placement.
func TestResumePolicyNeverRefusesResume(t *testing.T) {
	for _, how := range []string{"failed after preempt", "stopped by request", "running"} {
		for _, who := range []string{"tenant", "operator"} {
			t.Run(how+"/"+who, func(t *testing.T) {
				s, ctx := policyFixture(t, "never")
				switch how {
				case "stopped by request":
					if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
						t.Fatal(err)
					}
					exitR1(t, s)
				case "failed after preempt":
					if err := moveStops["preempt"](t, s); err != nil {
						t.Fatal(err)
					}
					exitR1(t, s)
				}
				var before string
				systemScan(t, s, `SELECT state || ' ' || updated_at::text || ' ' || secrets::text || ' ' || spec::text FROM runs WHERE id = 'r1'`, nil, &before)

				pctx := tenantCtx("t1")
				if who == "operator" {
					pctx = operatorCtx()
				}
				_, err := s.resumeRun(pctx, &resumeRunInput{RunPath: RunPath{ID: "r1"}, Body: &resumeRequest{
					Input: &resumeInput{Text: "go on"}}})
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "not_resumable" ||
					he.Message != neverResumableReason {
					t.Fatalf("resume: %v, want 409 not_resumable", err)
				}
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				var after string
				var placements, requested int
				systemScan(t, s, `SELECT state || ' ' || updated_at::text || ' ' || secrets::text || ' ' || spec::text,
						(SELECT count(*) FROM placements WHERE run_id = r.id),
						(SELECT count(*) FROM run_events WHERE run_id = r.id AND type = 'resume.requested')
					FROM runs r WHERE id = 'r1'`, nil, &after, &placements, &requested)
				if after != before || placements != 1 || requested != 0 {
					t.Fatalf("after a refused resume: %q (was %q), %d placements, %d resume.requested", after, before, placements, requested)
				}
				run, err := s.loadRun(context.Background(), "t1", "r1", false)
				if err != nil {
					t.Fatal(err)
				}
				rs, err := s.resumability(context.Background(), "t1", run)
				if err != nil || !slices.Contains(rs.Blockers, neverResumableReason) {
					t.Fatalf("resumability %+v %v", rs, err)
				}
			})
		}
	}
}
