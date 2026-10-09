package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// stageFixture is a Run (r1) scheduled on h1, its placement epoch 1
// assigned and accepted by its runner, as the stage tests start from.
func stageFixture(t *testing.T) (*Server, context.Context, string) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'ci', $1, ARRAY['run'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ($1, 't1', '{"workload": {"workdir": "/work"}}', 'scheduled', 1)`, r1)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, accepted_at, lease_expires_at)
		VALUES ('p1', 't1', $1, 'h1', 1, 'assigned', now(), now() + interval '1 hour')`, r1)
	return s, ctx, key
}

// reportStatus sends a runner's status for r1's epoch 1 from h1, as the hub
// hands it to luxd, and fails on a nack.
func reportStatus(t *testing.T, s *Server, ctx context.Context, st proto.Status) {
	t.Helper()
	reply := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, RunID: r1, Epoch: 1, Data: proto.Marshal(st)})
	if reply.Type != proto.MsgAck {
		t.Fatalf("report %+v: %s %s", st, reply.Type, reply.Data)
	}
}

func getRun(t *testing.T, s *Server, key string) Run {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", w.Code, w.Body)
	}
	var run Run
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// stageEvent is one stage event as the tests compare it.
type stageEvent struct {
	Stage, Reason string
	Since         time.Time
	Epoch         int
}

// stageEvents are r1's stage events, oldest first.
func stageEvents(t *testing.T, s *Server, ctx context.Context) []stageEvent {
	t.Helper()
	var out []stageEvent
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data->>'stage', coalesce(data->>'reason', ''), (data->>'since')::timestamptz, (data->>'epoch')::int
			FROM run_events WHERE run_id = $1 AND type = 'stage' ORDER BY id`, r1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (e stageEvent, err error) {
			return e, row.Scan(&e.Stage, &e.Reason, &e.Since, &e.Epoch)
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func stateEvents(t *testing.T, s *Server) int {
	t.Helper()
	return queryOne[int](t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'state'`, r1)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// A starting report with the marks so far records them while the Run
// starts, and the Run's stage follows: volumes once the image is ready,
// container once its repositories are.
func TestStartingReportRecordsPhaseMarks(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	run := getRun(t, s, key)
	if run.State != StateStarting || len(run.Placements) != 1 || run.Placements[0].State != "starting" {
		t.Fatalf("after the first starting report: run %s, placements %+v", run.State, run.Placements)
	}
	pl := run.Placements[0]
	if pl.ImageReadyAt == nil || !pl.ImageReadyAt.Equal(t0) || pl.VolumesRestoredAt != nil || pl.ReposReadyAt != nil {
		t.Fatalf("placement marks %+v, want only imageReady %s", pl, t0)
	}
	if run.Stage != StageVolumes || !run.StageSince.Equal(t0) {
		t.Fatalf("stage %s since %s, want volumes since %s", run.Stage, run.StageSince, t0)
	}

	t1, t2 := t0.Add(2*time.Second), t0.Add(3*time.Second)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t1), "reposReady": ms(t2)}})
	run = getRun(t, s, key)
	pl = run.Placements[0]
	if pl.VolumesRestoredAt == nil || !pl.VolumesRestoredAt.Equal(t1) || pl.ReposReadyAt == nil || !pl.ReposReadyAt.Equal(t2) || pl.ContainerStartedAt != nil {
		t.Fatalf("placement marks %+v, want volumesRestored %s and reposReady %s", pl, t1, t2)
	}
	if run.State != StateStarting || run.Stage != StageContainer || !run.StageSince.Equal(t2) {
		t.Fatalf("run %s, stage %s since %s; want starting, container since %s", run.State, run.Stage, run.StageSince, t2)
	}
	// The list carries the stage too.
	w := apiCall(t, s, key, http.MethodGet, "/v1/runs", nil)
	var list struct{ Runs []Run }
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Runs) != 1 || list.Runs[0].Stage != StageContainer {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

// A starting report that arrives after running (late, or redelivered)
// moves neither the Run nor its placement back, and its stage stays.
func TestLateStartingAfterRunningChangesNothing(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	marks := map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second)),
		"reposReady": ms(t0.Add(2 * time.Second)), "containerStarted": ms(t0.Add(3 * time.Second))}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: marks})
	before := getRun(t, s, key)
	states, stages := stateEvents(t, s), len(stageEvents(t, s, ctx))
	if before.State != StateRunning || before.Stage != StageRunning {
		t.Fatalf("after running: %s, stage %s", before.State, before.Stage)
	}

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: marks})
	after := getRun(t, s, key)
	if after.State != StateRunning || after.Placements[0].State != "running" {
		t.Fatalf("after a late starting: run %s, placement %s; want both running", after.State, after.Placements[0].State)
	}
	if after.Stage != before.Stage || !after.StageSince.Equal(before.StageSince) {
		t.Fatalf("stage moved: %s since %s, was %s since %s", after.Stage, after.StageSince, before.Stage, before.StageSince)
	}
	if n := stateEvents(t, s); n != states {
		t.Fatalf("%d state events, was %d", n, states)
	}
	if n := len(stageEvents(t, s, ctx)); n != stages {
		t.Fatalf("%d stage events, was %d", n, stages)
	}
}

// A stage event is emitted once per change of stage, in order, with its since
// and epoch, and never for a report that changes nothing.
func TestRunStageEmittedOncePerChange(t *testing.T) {
	s, ctx, _ := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	steps := []map[string]int64{
		{"imageReady": ms(t0)},
		{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second))},
		{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second)), "reposReady": ms(t0.Add(2 * time.Second))},
	}
	for _, times := range steps {
		reportStatus(t, s, ctx, proto.Status{State: "starting", Times: times})
		reportStatus(t, s, ctx, proto.Status{State: "starting", Times: times}) // redelivered
	}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: steps[0]}) // late
	running := map[string]int64{"containerStarted": ms(t0.Add(3 * time.Second))}
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: running})
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: running})

	n := newStageEvents(t, s, ctx, "starting and running reports", 0,
		stageEvent{StageVolumes, "", t0, 1}, stageEvent{StageRepositories, "", t0.Add(time.Second), 1},
		stageEvent{StageContainer, "", t0.Add(2 * time.Second), 1}, stageEvent{StageRunning, "", t0.Add(3 * time.Second), 1})

	// A stop moves it to stopping, with the stop's reason, once; the
	// runner's stopping report changes nothing more.
	stopRun(t, s, ctx, "migrate")
	stopRun(t, s, ctx, "migrate")
	reportStatus(t, s, ctx, proto.Status{State: "stopping"})
	stopAt := queryOne[time.Time](t, s, `SELECT stop_requested_at FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
	n = newStageEvents(t, s, ctx, "two stops", n, stageEvent{StageStopping, "migrate", stopAt, 1})
	// A terminate while it stops replaces the reason: announced, same since.
	stopRun(t, s, ctx, stopTerminate)
	newStageEvents(t, s, ctx, "terminate", n, stageEvent{StageStopping, stopTerminate, stopAt, 1})
}

// A placement assigned and not yet accepted is waiting; its runner's ack
// of the assignment moves the Run to image, since luxd recorded the ack.
func TestAcceptedAssignmentLeavesWaiting(t *testing.T) {
	s, ctx, key := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET accepted_at = NULL, needed_since = now() - interval '5 seconds' WHERE run_id = $1`, r1)
	execSQL(t, s, ctx, `INSERT INTO host_messages (id, host_id, run_id, epoch, type) VALUES (7, 'h1', $1, 1, $2)`, r1, proto.MsgAssign)
	if run := getRun(t, s, key); run.Stage != StageWaiting {
		t.Fatalf("assigned, not accepted: stage %s", run.Stage)
	}
	for range 2 { // the second ack is a redelivery: nothing to announce
		if err := s.ackMessage(ctx, "h1", 7); err != nil {
			t.Fatal(err)
		}
	}
	run := getRun(t, s, key)
	if run.Stage != StageImage || run.Placements[0].AcceptedAt == nil || !run.StageSince.Equal(*run.Placements[0].AcceptedAt) {
		t.Fatalf("accepted: stage %s since %s, placement %+v; want image since acceptedAt", run.Stage, run.StageSince, run.Placements[0])
	}
	newStageEvents(t, s, ctx, "accepted", 0, stageEvent{StageImage, "", *run.Placements[0].AcceptedAt, 1})
}

// stopRun asks r1's placement on h1 to stop for reason, in a transaction
// of its own.
func stopRun(t *testing.T, s *Server, ctx context.Context, reason string) {
	t.Helper()
	var host string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		host, err = s.requestStop(ctx, tx, "t1", r1, reason)
		return err
	}); err != nil || host != "h1" {
		t.Fatalf("stop: %q %v", host, err)
	}
}

// A starting report that arrives after a move's exit (sent before it,
// delivered after) leaves the Run resuming: its placement has ended, so
// nothing is starting.
func TestLateStartingAfterAMoveLeavesTheRunResuming(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	stopRun(t, s, ctx, "migrate")
	code := 0
	reportStatus(t, s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped", Times: map[string]int64{"imageReady": ms(t0)}})
	before := getRun(t, s, key)
	if before.State != StateResuming || before.Stage != StageWaiting {
		t.Fatalf("after the move's exit: %s, stage %s; want resuming, waiting", before.State, before.Stage)
	}
	stages := len(stageEvents(t, s, ctx))

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second))}})
	after := getRun(t, s, key)
	if after.State != StateResuming || after.Placements[0].State != "exited" {
		t.Fatalf("after a late starting: run %s, placement %s; want resuming, exited", after.State, after.Placements[0].State)
	}
	if after.Stage != StageWaiting || !after.StageSince.Equal(before.StageSince) {
		t.Fatalf("stage %s since %s, was waiting since %s", after.Stage, after.StageSince, before.StageSince)
	}
	if n := len(stageEvents(t, s, ctx)); n != stages {
		t.Fatalf("%d stage events, was %d", n, stages)
	}
}

// A starting report for a placement whose acceptance luxd never recorded
// (the ack lost across a reconnect) records it: the stage is image, not
// waiting.
func TestStartingReportRecordsAcceptance(t *testing.T) {
	s, ctx, key := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET accepted_at = NULL WHERE run_id = $1`, r1)
	reportStatus(t, s, ctx, proto.Status{State: "starting"})
	run := getRun(t, s, key)
	pl := run.Placements[0]
	if pl.State != "starting" || pl.AcceptedAt == nil || run.Stage != StageImage || !run.StageSince.Equal(*pl.AcceptedAt) {
		t.Fatalf("stage %s since %s, placement %+v; want image since acceptedAt", run.Stage, run.StageSince, pl)
	}
}

// newStageEvents fails unless the stage events after the first n are want,
// and returns how many there are now.
func newStageEvents(t *testing.T, s *Server, ctx context.Context, step string, n int, want ...stageEvent) int {
	t.Helper()
	got := stageEvents(t, s, ctx)
	if len(got) < n {
		t.Fatalf("%s: %d stage events, had %d", step, len(got), n)
	}
	got = got[n:]
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = got[i].Stage == want[i].Stage && got[i].Reason == want[i].Reason && got[i].Epoch == want[i].Epoch &&
			got[i].Since.Equal(want[i].Since.Truncate(time.Microsecond))
	}
	if !ok {
		t.Fatalf("%s: new stage events %+v, want %+v", step, got, want)
	}
	return n + len(got)
}

func wantStage(t *testing.T, step string, run Run, state, stage string, since time.Time, reason string) {
	t.Helper()
	if run.State != state || run.Stage != stage || !run.StageSince.Equal(since) || run.StageReason != reason {
		t.Fatalf("%s: GET says %s, stage %s since %s (%q); want %s, %s since %s (%q)", step,
			run.State, run.Stage, run.StageSince, run.StageReason, state, stage, since, reason)
	}
}

// A move announces stopping with its reason, then waiting from the old
// placement's end, then the next placement's start stages: never stopped,
// which no reader could see (the exit and the resume are one
// transaction). Its assignment changes nothing; its acceptance is image.
func TestAMoveIsAnnouncedStoppingThenWaiting(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	marks := map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second)),
		"reposReady": ms(t0.Add(2 * time.Second)), "containerStarted": ms(t0.Add(3 * time.Second))}
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: marks})
	wantStage(t, "running", getRun(t, s, key), StateRunning, StageRunning, t0.Add(3*time.Second), "")
	n := len(stageEvents(t, s, ctx))

	stopRun(t, s, ctx, "migrate")
	stopAt := queryOne[time.Time](t, s, `SELECT stop_requested_at FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
	wantStage(t, "stop", getRun(t, s, key), StateStopping, StageStopping, stopAt, "migrate")
	n = newStageEvents(t, s, ctx, "stop", n, stageEvent{StageStopping, "migrate", stopAt, 1})

	code := 0
	reportStatus(t, s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped", Times: marks})
	endedAt := queryOne[time.Time](t, s, `SELECT ended_at FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
	if needs := queryOne[time.Time](t, s, `SELECT needs_host_since FROM runs WHERE id = $1`, r1); !needs.Equal(endedAt) {
		t.Fatalf("needs_host_since %s, placement ended %s", needs, endedAt)
	}
	wantStage(t, "exit", getRun(t, s, key), StateResuming, StageWaiting, endedAt, "")
	n = newStageEvents(t, s, ctx, "exit", n, stageEvent{StageWaiting, "", endedAt, 1})

	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.assign(ctx, tx, pendingRun{ID: r1, TenantID: "t1", Epoch: 1}, &candidateHost{ID: "h1"})
	}); err != nil {
		t.Fatal(err)
	}
	wantStage(t, "assign", getRun(t, s, key), StateScheduled, StageWaiting, endedAt, "")
	n = newStageEvents(t, s, ctx, "assign", n)

	var msg int64
	systemScan(t, s, `SELECT id FROM host_messages WHERE run_id = $1 AND epoch = 2 AND type = 'assign'`, []any{r1}, &msg)
	if err := s.ackMessage(ctx, "h1", msg); err != nil {
		t.Fatal(err)
	}
	acceptedAt := queryOne[time.Time](t, s, `SELECT accepted_at FROM placements WHERE run_id = $1 AND epoch = 2`, r1)
	wantStage(t, "accept", getRun(t, s, key), StateScheduled, StageImage, acceptedAt, "")
	newStageEvents(t, s, ctx, "accept", n, stageEvent{StageImage, "", acceptedAt, 2})
}

// An assignment refused at delivery requeues its Run: never lost, as far
// as its stage goes, and still waiting since the wait that placement
// began, not since the requeue.
func TestARequeueKeepsTheWait(t *testing.T) {
	s, ctx, key := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET accepted_at = NULL, needed_since = now() - interval '40 seconds' WHERE run_id = $1`, r1)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"git": {"repositories": [{"name": "app", "url": "https://x/app.git", "path": "/w/app"}]}}' WHERE id = $1`, r1)
	waitedFrom := queryOne[time.Time](t, s, `SELECT needed_since FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
	a := proto.Assign{RunID: r1, TenantID: "t1", Epoch: 1, Sync: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFastForward}}}
	execSQL(t, s, ctx, `INSERT INTO host_messages (id, host_id, run_id, epoch, type, payload) VALUES (9, 'h1', $1, 1, 'assign', $2)`, r1, proto.Marshal(a))
	wantStage(t, "assigned", getRun(t, s, key), StateScheduled, StageWaiting, waitedFrom, "")
	execSQL(t, s, ctx, `UPDATE runs SET stage_announced = jsonb_build_object('stage', 'waiting', 'since', $2::timestamptz) WHERE id = $1`, r1, waitedFrom)
	n := len(stageEvents(t, s, ctx))

	requeue := func(epoch int) {
		t.Helper()
		var msg int64
		systemScan(t, s, `SELECT id FROM host_messages WHERE run_id = $1 AND epoch = $2 AND type = 'assign'`, []any{r1, epoch}, &msg)
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.refuseSync(ctx, tx, "h1", msg, r1) }); err != nil {
			t.Fatal(err)
		}
	}
	requeue(1)
	var placement string
	systemScan(t, s, `SELECT state FROM placements WHERE run_id = $1 AND epoch = 1`, []any{r1}, &placement)
	if placement != "lost" {
		t.Fatalf("placement %s, want lost", placement)
	}
	wantStage(t, "requeue", getRun(t, s, key), StateResuming, StageWaiting, waitedFrom, "")
	n = newStageEvents(t, s, ctx, "requeue", n)

	// Placed again and refused again: still the first wait.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.assign(ctx, tx, pendingRun{ID: r1, TenantID: "t1", Epoch: 1, PendingSync: a.Sync}, &candidateHost{ID: "h1"})
	}); err != nil {
		t.Fatal(err)
	}
	wantStage(t, "assigned again", getRun(t, s, key), StateScheduled, StageWaiting, waitedFrom, "")
	requeue(2)
	wantStage(t, "requeued again", getRun(t, s, key), StateResuming, StageWaiting, waitedFrom, "")
	newStageEvents(t, s, ctx, "requeued again", n)

	// An operator's resume later waits from its own request.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, r1)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.requestResume(ctx, tx, "t1", r1, nil, "resume requested")
	}); err != nil {
		t.Fatal(err)
	}
	needs := queryOne[time.Time](t, s, `SELECT needs_host_since FROM runs WHERE id = $1`, r1)
	wantStage(t, "resumed", getRun(t, s, key), StateResuming, StageWaiting, needs, "")
}

// A start that fails at its volumes announces failed once, since the
// Run's state changed; a starting report after it adds nothing.
func TestAFailedStartIsAnnouncedOnce(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	marks := map[string]int64{"imageReady": ms(t0)}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: marks})
	n := newStageEvents(t, s, ctx, "image ready", 0, stageEvent{StageVolumes, "", t0, 1})

	code := 125
	reportStatus(t, s, ctx, proto.Status{State: "failed", ExitCode: &code, Reason: "start-failed", Message: "volumes: no space", Times: marks})
	changed := queryOne[time.Time](t, s, `SELECT state_changed_at FROM runs WHERE id = $1`, r1)
	wantStage(t, "failed", getRun(t, s, key), StateFailed, StateFailed, changed, "")
	n = newStageEvents(t, s, ctx, "failed", n, stageEvent{StateFailed, "", changed, 1})

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second))}})
	run := getRun(t, s, key)
	wantStage(t, "late starting", run, StateFailed, StateFailed, changed, "")
	if run.Placements[0].State != "exited" {
		t.Fatalf("late starting: placement %s, want exited", run.Placements[0].State)
	}
	newStageEvents(t, s, ctx, "late starting", n)
}

// A placement luxd gives up on (its host lost) announces lost, since the
// Run's state changed: a change made by setRunState alone.
func TestALostPlacementIsAnnounced(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	n := newStageEvents(t, s, ctx, "image ready", 0, stageEvent{StageVolumes, "", t0, 1})
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var later laterEvents
		if err := s.placementLost(ctx, tx, r1, 1, "host lost", &later); err != nil {
			return err
		}
		return later.write()
	}); err != nil {
		t.Fatal(err)
	}
	changed := queryOne[time.Time](t, s, `SELECT state_changed_at FROM runs WHERE id = $1`, r1)
	wantStage(t, "lost", getRun(t, s, key), StateLost, StateLost, changed, "")
	newStageEvents(t, s, ctx, "lost", n, stageEvent{StateLost, "", changed, 1})
}

// A terminate while the Run starts is stopping (terminate) from the
// request, then terminated.
func TestATerminateDuringStartIsAnnounced(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	marks := map[string]int64{"imageReady": ms(t0)}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: marks})
	n := newStageEvents(t, s, ctx, "image ready", 0, stageEvent{StageVolumes, "", t0, 1})

	execSQL(t, s, ctx, `UPDATE runs SET terminate_requested = true WHERE id = $1`, r1)
	stopRun(t, s, ctx, stopTerminate)
	stopAt := queryOne[time.Time](t, s, `SELECT stop_requested_at FROM placements WHERE run_id = $1 AND epoch = 1`, r1)
	wantStage(t, "terminate", getRun(t, s, key), StateStopping, StageStopping, stopAt, stopTerminate)
	n = newStageEvents(t, s, ctx, "terminate", n, stageEvent{StageStopping, stopTerminate, stopAt, 1})

	code := 0
	reportStatus(t, s, ctx, proto.Status{State: "exited", ExitCode: &code, Reason: "stopped", Message: "stopped before start", Times: marks})
	changed := queryOne[time.Time](t, s, `SELECT state_changed_at FROM runs WHERE id = $1`, r1)
	wantStage(t, "exited", getRun(t, s, key), StateTerminated, StateTerminated, changed, "")
	newStageEvents(t, s, ctx, "exited", n, stageEvent{StateTerminated, "", changed, 1})
}

// Without repositories, reposReady is marked as the volumes are restored:
// one report with both moves the Run straight to container.
func TestNoRepositoriesGoesStraightToContainer(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	n := newStageEvents(t, s, ctx, "image ready", 0, stageEvent{StageVolumes, "", t0, 1})
	t1 := t0.Add(time.Second)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t1), "reposReady": ms(t1)}})
	wantStage(t, "volumes restored", getRun(t, s, key), StateStarting, StageContainer, t1, "")
	newStageEvents(t, s, ctx, "volumes restored", n, stageEvent{StageContainer, "", t1, 1})
}

// A starting report wakes the scheduler for nothing (it frees no capacity
// and queues no Run); running does.
func TestStartingReportDoesNotKickTheScheduler(t *testing.T) {
	s, ctx, _ := stageFixture(t)
	drain := func() {
		select {
		case <-s.kick:
		default:
		}
	}
	drain()
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(time.Now())}})
	if len(s.kick) != 0 {
		t.Fatal("a starting report kicked the scheduler")
	}
	reportStatus(t, s, ctx, proto.Status{State: "running"})
	if len(s.kick) != 1 {
		t.Fatal("a running report did not kick the scheduler")
	}
}
