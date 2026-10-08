package server

import (
	"net/http"
	"testing"
)

// exitR1With reports r1's placement 1 exited with code after its final
// snapshot, as exitR1 does for a stopped one.
func exitR1With(t *testing.T, s *Server, code int) {
	t.Helper()
	snapshotR1(t, s)
	reportR1Exit(t, s, code, "exited")
}

// A succeeded Run is resumed as a stopped one is: resume accepts it, and
// its next placement resumes its snapshot and session through the
// adapter's resume path. Its servers stay attached, and it is listed by
// ?resumable=true.
func TestResumeSucceededRun(t *testing.T) {
	s, ctx := policyFixture(t, "")
	exitR1With(t, s, 0)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snapR1'`)
	var state string
	var servers int
	systemScan(t, s, `SELECT state, (SELECT count(*) FROM run_servers WHERE run_id = 'r1') FROM runs WHERE id = 'r1'`, nil, &state, &servers)
	if state != StateSucceeded || servers != 1 {
		t.Fatalf("after exit 0: %q, %d servers", state, servers)
	}
	out, err := s.listRuns(tenantCtx("t1"), &listRunsInput{Resumable: true})
	if err != nil || len(out.Body.Runs) != 1 || out.Body.Runs[0].ID != "r1" {
		t.Fatalf("resumable list: %+v %v", out, err)
	}
	run, err := s.getRun(tenantCtx("t1"), &RunPath{ID: "r1"})
	if err != nil || run.Body.Resume == nil || len(run.Body.Resume.Blockers) != 0 {
		t.Fatalf("GET: %+v %v, want resumability with no blockers", run, err)
	}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	if a := assignOf(t, s, 2); a.Resume == nil || a.Resume.Snapshot == nil || a.Resume.Snapshot.SnapshotID != "snapR1" || a.Resume.SessionID != "sess-1" {
		t.Fatalf("resume of a succeeded Run assigned %+v, want snapR1 and sess-1", a.Resume)
	}
}

// A succeeded Run that never took a snapshot (no snapshot report) is
// resumable too: its next placement is a first one, with no snapshot and
// no session to restore.
func TestResumeSucceededRunWithoutSnapshot(t *testing.T) {
	s, ctx := policyFixture(t, "")
	reportR1Exit(t, s, 0, "exited")
	var state string
	var resumable bool
	systemScan(t, s, `SELECT r.state, `+resumableSQL+` FROM runs r WHERE r.id = 'r1'`, nil, &state, &resumable)
	if state != StateSucceeded || !resumable {
		t.Fatalf("after exit 0 without a snapshot: %q resumable %v", state, resumable)
	}
	s.secrets.put("r1", map[string]string{"TOKEN": "jit"})
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	if a := assignOf(t, s, 2); a.Resume != nil {
		t.Fatalf("resume of a succeeded Run without a snapshot assigned %+v, want a first placement", a.Resume)
	}
}

// A succeeded Run can be terminated (by the API, at once), and then it is
// no longer resumable: 409, left out of ?resumable=true. A stop of a
// succeeded Run changes nothing.
func TestTerminateSucceededRun(t *testing.T) {
	s, _ := policyFixture(t, "")
	exitR1With(t, s, 0)
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r1'`, nil, &state)
	if state != StateSucceeded {
		t.Fatalf("after a stop of a succeeded Run: %q", state)
	}
	postR1(t, s, "terminate")
	assertTerminated(t, s, "terminated")
	_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}})
	refused(t, err, http.StatusConflict, "not_resumable", "resume of a terminated Run")
	out, err := s.listRuns(tenantCtx("t1"), &listRunsInput{Resumable: true})
	if err != nil || len(out.Body.Runs) != 0 {
		t.Fatalf("resumable list: %+v %v", out, err)
	}
	var servers int
	systemScan(t, s, `SELECT count(*) FROM run_servers WHERE run_id = 'r1'`, nil, &servers)
	if servers != 0 {
		t.Fatalf("%d lifetime-run servers left after terminate", servers)
	}
}
