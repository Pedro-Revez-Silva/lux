package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Run states. See docs/lifecycle.md.
const (
	StateSubmitted    = "submitted"
	StateScheduled    = "scheduled"
	StateProvisioning = "provisioning"
	StateStarting     = "starting"
	StateRunning      = "running"
	StateStopping     = "stopping"
	StateStopped      = "stopped"
	StateResuming     = "resuming"
	StateSucceeded    = "succeeded"
	StateFailed       = "failed"
	StateTerminated   = "terminated"
	StateLost         = "lost"
)

// livePlacementStates, for SQL: a placement that is (or is about to be)
// running on its host.
const livePlacementStates = "('assigned', 'starting', 'running', 'stopping')"

// queuedRunStates, for SQL: Runs waiting for a host.
const queuedRunStates = "('submitted', 'resuming', 'provisioning')"

// resumableRunStates, for SQL: Runs resume accepts: every end but terminated.
const resumableRunStates = "('stopped', 'lost', 'failed', 'succeeded')"

// endedRunStates, for SQL: ended(state).
const endedRunStates = "('succeeded', 'failed', 'terminated')"

// restingRunStates, for SQL: Runs expiry terminates once they have rested
// longer than their tenant's expire_after_days (reapExpiry): every
// resumable state. Each must be in the runs_resting index's predicate.
const restingRunStates = resumableRunStates

// refusedWithoutSnapshot, for SQL over runs (as r): the current placement's
// latest report was refused and the Run has no snapshot to restore. Exempt
// is a restart Run with no session either: it restores nothing, so resuming
// it is a first placement. One with a session would resume that session on
// empty volumes, so it stays refused.
const refusedWithoutSnapshot = `(r.snapshot_id IS NULL AND NOT ` + restartsFromScratchSQL + ` AND EXISTS (
	SELECT 1 FROM placements p WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.snapshot_refused))`

// noSnapshotReason explains why resume refuses such a Run.
const noSnapshotReason = "its only snapshot report was refused, so there is no snapshot to restore"

// resumableSQL, for SQL over runs (as r): POST /v1/runs/{id}/resume
// without fromSnapshot would accept the Run. The one definition behind
// both Run.resumable (runColumns) and GET /v1/runs?resumable=true.
const resumableSQL = `(r.state IN ` + resumableRunStates + ` AND NOT ` + refusesResumeSQL + ` AND NOT ` + refusedWithoutSnapshot + `)`

// movedStops: stop reasons that move a Run rather than stop it; it is
// placed again as soon as it has stopped (resumed, or restarted from
// scratch), unless its spec's resumePolicy is manual or never
// (spec.FailsOnMove), when it ends instead: failed, or terminated (never).
var movedStops = []string{"drain", "preempt", "migrate"}

// stopTerminate is the stop reason of a terminate request.
const stopTerminate = "terminate"

// notResumedReason is the state_reason of a Run a move (stop) ended
// because of its resumePolicy: failed (manual) or terminated (never).
func notResumedReason(stop, policy string) string {
	return stop + ": not resumed (resumePolicy " + policy + ")"
}

// neverResumedEnd is how a Run whose resumePolicy is never ends: an end
// that would leave it resumable (stopped, lost, succeeded, failed)
// terminates it instead, as nothing can resume it, its outcome kept in the
// reason ("succeeded; resumePolicy never", "host lost: missed heartbeats;
// resumePolicy never"). Any other state, or any other policy, is returned
// unchanged. endRun applies it before setRunState, so that a Run's side
// effects (secrets, servers, events) follow the state written.
func neverResumedEnd(policy, state, reason string) (string, string) {
	if !spec.RefusesResume(policy) || !resumable(state) {
		return state, reason
	}
	return StateTerminated, cmp.Or(reason, state) + "; resumePolicy " + spec.ResumeNever
}

// runResumePolicy is a Run's spec's resumePolicy, "" when unset.
func runResumePolicy(ctx context.Context, tx pgx.Tx, runID string) (string, error) {
	var policy string
	err := tx.QueryRow(ctx, `SELECT `+runResumePolicySQL+` FROM runs r WHERE r.id = $1`, runID).Scan(&policy)
	return policy, err
}

// runResumePolicySQL, for SQL over runs (as r): its spec's resumePolicy,
// "" when unset (auto). Every SQL reading of the policy goes through it.
const runResumePolicySQL = `coalesce(r.spec->>'resumePolicy', '')`

// failsOnMoveSQL, for SQL over runs (as r): spec.FailsOnMove of its policy.
const failsOnMoveSQL = runResumePolicySQL + ` IN ('` + spec.ResumeManual + `', '` + spec.ResumeNever + `')`

// refusesResumeSQL, for SQL over runs (as r): spec.RefusesResume of its policy.
const refusesResumeSQL = runResumePolicySQL + ` = '` + spec.ResumeNever + `'`

// restartsFromScratchSQL, for SQL over runs (as r): resumePolicy restart and
// no session, so a resume restores nothing (assign sends no ResumeInfo).
const restartsFromScratchSQL = `(` + runResumePolicySQL + ` = '` + spec.ResumeRestart + `' AND r.session_id = '')`

// neverResumableReason is why a Run whose resumePolicy is never is not
// resumed: the 409's message and its resumability blocker.
const neverResumableReason = "resumePolicy never: this Run cannot be resumed"

// errNeverResumable refuses any requested resume of a Run whose
// resumePolicy is never.
func errNeverResumable() error {
	return errf(http.StatusConflict, "not_resumable", "%s", neverResumableReason)
}

// forgetRestoredState makes a Run's next placement a first one: with no
// snapshot and no session, assign sends no ResumeInfo, so the runner
// starts it on empty state volumes through the adapter's start path. Its
// snapshots stay (resume --from-snapshot); flagged superseded, retention
// deletes them once the next placement's snapshot is uploaded.
func forgetRestoredState(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE runs SET snapshots_superseded = snapshots_superseded OR snapshot_id IS NOT NULL,
			snapshot_id = NULL, session_id = '' WHERE id = $1`, runID)
	return err
}

// placementRef names one placement.
type placementRef struct {
	RunID    string
	TenantID string
	Epoch    int
}

// livePlacements lists live placements matching a condition on the
// placements table (aliased p).
func livePlacements(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]placementRef, error) {
	rows, err := tx.Query(ctx, `SELECT p.run_id, p.tenant_id, p.epoch FROM placements p
		WHERE p.state IN `+livePlacementStates+` AND (`+where+`)`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[placementRef])
}

// terminal: a Run in this state has ended for good: it is terminated, and
// nothing ever runs or resumes it again.
func terminal(state string) bool { return state == StateTerminated }

// Ended: a Run in this state has no placement and has ended its last one
// on its own or for good (succeeded, failed, terminated). Every one but
// terminated can still be resumed.
func Ended(state string) bool { return ended(state) }

func ended(state string) bool {
	return state == StateSucceeded || state == StateFailed || state == StateTerminated
}

// resumable: resume accepts a Run in this state (resumableRunStates),
// whatever else may refuse it.
func resumable(state string) bool {
	return state == StateStopped || state == StateLost || state == StateFailed || state == StateSucceeded
}

// live: a placement exists and is (or is about to be) running.
func live(state string) bool {
	switch state {
	case StateScheduled, StateStarting, StateRunning, StateStopping:
		return true
	}
	return false
}

// runEnd is how a Run's placement, or its wait for one, ended: the state
// and reason it would rest in under any resumePolicy but never.
type runEnd struct {
	tenantID, runID string
	epoch           int
	policy          string // the Run's resumePolicy, "" for auto
	state, reason   string
	// note is added to the reason after neverResumedEnd's tag.
	note string
	// requeued: the caller places the Run again in the same transaction,
	// so this is not how it ends: written unmapped, its secrets kept.
	requeued bool
}

// endRun writes a Run's end: neverResumedEnd maps it by policy, setRunState
// writes it, and once written an ended Run's held secrets go (they serve a
// running Run and an operator's resume of a stopped or lost one; an ended
// Run is resumed with its secrets supplied again). It returns the state
// written. Every site that ends a Run goes through it except reapExpiry,
// which only ever writes terminated and drops secrets after its commit.
// The drop here is not deferred to commit: a rolled-back batch keeps the
// Run as it was but its secrets gone, which is the safe direction.
func (s *Server) endRun(ctx context.Context, tx pgx.Tx, e runEnd) (string, error) {
	state, reason := e.state, e.reason
	if !e.requeued {
		state, reason = neverResumedEnd(e.policy, state, reason)
	}
	if e.note != "" {
		reason = strings.TrimPrefix(reason+"; "+e.note, "; ")
	}
	if err := setRunState(ctx, tx, e.tenantID, e.runID, state, reason, e.epoch); err != nil {
		return "", err
	}
	if ended(state) && !e.requeued {
		s.secrets.drop(e.runID)
	}
	return state, nil
}

// setRunState moves a Run to state, as given. A Run's end goes through
// endRun (or reapExpiry) instead, which maps it by resumePolicy and drops
// its secrets.
func setRunState(ctx context.Context, tx pgx.Tx, tenantID, runID, state, reason string, epoch int) error {
	if ended(state) {
		// An ended Run begins a new settlement epoch after a stopped/lost one.
		if _, err := tx.Exec(ctx, `UPDATE cost_sources SET settles_left = NULL, next_at = NULL, attempts = 0
			WHERE run_id = $1 AND source <> 'compute' AND EXISTS
			(SELECT 1 FROM runs WHERE id = $1 AND state IN ('stopped', 'lost'))`, runID); err != nil {
			return err
		}
	}
	// prompt_attachments: only a terminated Run is never placed again. A
	// succeeded or failed one can be resumed, as a first placement when it
	// has no session and no snapshot, so it keeps them.
	// finished_at: when its last placement ended. A Run terminated once it
	// had ended (succeeded, failed: nothing ran since) keeps it.
	// terminated_at: retention's clock (reapRetention).
	// state_changed_at is expiry's clock (reapExpiry): moved only by a
	// change of state, so a repeated stop does not restart it.
	_, err := tx.Exec(ctx, `UPDATE runs SET state = $2, state_reason = $3, updated_at = now(),
			state_changed_at = CASE WHEN state <> $2 THEN now() ELSE state_changed_at END,
			finished_at = CASE WHEN $2 IN ('succeeded', 'failed') THEN now()
				WHEN $2 = 'terminated' AND state IN ('succeeded', 'failed') THEN finished_at
				WHEN $2 = 'terminated' THEN now() ELSE finished_at END,
			terminated_at = CASE WHEN $2 = 'terminated' THEN coalesce(terminated_at, now()) ELSE terminated_at END,
			prompt_attachments = CASE WHEN $2 = 'terminated' THEN NULL ELSE prompt_attachments END,
			activity = CASE WHEN $2 IN ('running') THEN activity ELSE '' END
		WHERE id = $1`, runID, state, reason)
	if err != nil {
		return err
	}
	if costStates[state] {
		if err := enqueueCost(ctx, tx, runID, "state:"+state); err != nil {
			return err
		}
	}
	data := map[string]any{"state": state}
	if reason != "" {
		data["reason"] = reason
	}
	if err := addEvent(ctx, tx, tenantID, runID, epoch, "state", data); err != nil {
		return err
	}
	return endServers(ctx, tx, tenantID, runID, state)
}

// costStates: a Run entering one of these has its costs evaluated at once
// (docs/costs.md, section 5), queued with the state change itself.
var costStates = map[string]bool{
	StateStopping: true, StateStopped: true, StateLost: true,
	StateSucceeded: true, StateFailed: true, StateTerminated: true,
}

// enqueueCost queues a Run's costs as due now, merged with any row it
// already has, in the caller's transaction (a tenant's scope too:
// lux_cost_enqueue, migration 022). Nothing is computed here.
func enqueueCost(ctx context.Context, tx pgx.Tx, runID, reason string) error {
	_, err := tx.Exec(ctx, `SELECT lux_cost_enqueue($1, $2)`, runID, reason)
	return err
}

// applyStatus moves a Run forward from what its runner reports.
func (s *Server) applyStatus(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, st proto.Status) error {
	t := st.Times
	_, err := tx.Exec(ctx, `UPDATE placements SET
			image_ready_at       = coalesce(image_ready_at, $3),
			volumes_restored_at  = coalesce(volumes_restored_at, $4),
			container_started_at = coalesce(container_started_at, $5),
			workload_started_at  = coalesce(workload_started_at, $6),
			exited_at            = coalesce(exited_at, $7),
			memory_limit         = coalesce(memory_limit, nullif($8::bigint, 0))
		WHERE run_id = $1 AND epoch = $2`, runID, epoch,
		msToTime(t["imageReady"]), msToTime(t["volumesRestored"]), msToTime(t["containerStarted"]),
		msToTime(t["workloadStarted"]), msToTime(t["exited"]), st.MemoryLimit)
	if err != nil {
		return err
	}
	if st.Usage != nil {
		if err := recordUsage(ctx, tx, runID, epoch, st.Usage); err != nil {
			return err
		}
	}

	var runState string
	var terminate bool
	if err := tx.QueryRow(ctx, `SELECT state, terminate_requested FROM runs WHERE id = $1`, runID).Scan(&runState, &terminate); err != nil {
		return err
	}
	// A report for a placement of a Run that has ended (redelivered or out
	// of order) moves nothing; a resume makes the Run resuming first.
	if ended(runState) {
		return nil
	}

	switch st.State {
	case "starting":
		if _, err := tx.Exec(ctx, `UPDATE placements SET state = 'starting' WHERE run_id = $1 AND epoch = $2 AND state = 'assigned'`, runID, epoch); err != nil {
			return err
		}
		if runState == StateScheduled || runState == StateResuming || runState == StateProvisioning {
			return setRunState(ctx, tx, tenantID, runID, StateStarting, "", epoch)
		}
	case "running":
		// Only the transition matters: a placement reports running more than
		// once (shim started, workload started), and reports are redelivered.
		var hostID string
		err := tx.QueryRow(ctx, `UPDATE placements SET state = 'running', started_at = coalesce(started_at, now())
			WHERE run_id = $1 AND epoch = $2 AND state IN ('assigned', 'starting')
			RETURNING host_id`, runID, epoch).Scan(&hostID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if hostID != "" {
			if _, err := tx.Exec(ctx, `UPDATE runs SET first_started_at = coalesce(first_started_at, now()) WHERE id = $1`, runID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE hosts SET first_placement_at = coalesce(first_placement_at, now()) WHERE id = $1`, hostID); err != nil {
				return err
			}
			// Running from its restored state: other hosts' copies of its
			// older snapshots are no longer needed. (Not earlier: a start
			// that fails can still fall back to them.)
			if err := discardOldCopies(ctx, tx, runID, epoch, hostID); err != nil {
				return err
			}
		}
		if runState != StateRunning && runState != StateStopping {
			return setRunState(ctx, tx, tenantID, runID, StateRunning, "", epoch)
		}
	case "stopping":
		if _, err := tx.Exec(ctx, `UPDATE placements SET state = 'stopping' WHERE run_id = $1 AND epoch = $2 AND state IN ('starting', 'running')`, runID, epoch); err != nil {
			return err
		}
	case "exited", "failed":
		return s.placementExited(ctx, tx, tenantID, runID, epoch, st, runState, terminate)
	}
	return nil
}

// placementExited records a placement's end and decides what the Run
// becomes. The snapshot arrives separately (snapshot.done); a stopped Run is
// resumable once it has.
func (s *Server) placementExited(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, st proto.Status, runState string, terminate bool) error {
	var stopReason, hostID string
	var snapshotRefused, hasSnapshot bool
	if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&hostID); err != nil {
		return err
	}
	if err := lockCostHost(ctx, tx, hostID); err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `UPDATE placements p SET state = 'exited', ended_at = now(),
			exited_at = coalesce(exited_at, now()), exit_code = $3, exit_reason = $4, output_seq = nullif($5::bigint, 0),
			lease_expires_at = NULL
		WHERE run_id = $1 AND epoch = $2 AND state <> 'exited'
		RETURNING stop_reason, host_id, snapshot_refused,
			(SELECT snapshot_id IS NOT NULL FROM runs WHERE id = p.run_id)`,
		runID, epoch, st.ExitCode, st.Reason, st.OutputSeq).Scan(&stopReason, &hostID, &snapshotRefused, &hasSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded: a redelivered report
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET last_placement_ended_at = now() WHERE id = $1`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET exit_code = $2 WHERE id = $1`, runID, st.ExitCode); err != nil {
		return err
	}
	addEvent(ctx, tx, tenantID, runID, epoch, "exited", map[string]any{"exitCode": st.ExitCode, "reason": st.Reason, "message": st.Message})

	moveStop := slices.Contains(movedStops, stopReason)
	policy, err := runResumePolicy(ctx, tx, runID)
	if err != nil {
		return err
	}
	failsOnMove := moveStop && spec.FailsOnMove(policy)
	restarts := policy == spec.ResumeRestart
	var next, reason string
	switch {
	case terminate || stopReason == stopTerminate:
		next, reason = StateTerminated, "terminated"
	case stopReason == "timeout":
		next, reason = StateFailed, "timeout"
	case stopReason == "disk":
		next, reason = StateFailed, "disk limit exceeded"
	case failsOnMove:
		// One-shot: what it was doing cannot continue on another host. The
		// reason names the policy already, so never terminates it here.
		next, reason = StateFailed, notResumedReason(stopReason, policy)
		if spec.RefusesResume(policy) {
			next = StateTerminated
		}
	case stopReason == "stop" || moveStop:
		// A requested stop: resumable (and a move resumed below).
		next, reason = StateStopped, stopReason
	case st.State == "failed":
		next, reason = StateFailed, st.Message
	case st.ExitCode != nil && *st.ExitCode == 0:
		next, reason = StateSucceeded, ""
	default:
		code := -1
		if st.ExitCode != nil {
			code = *st.ExitCode
		}
		next, reason = StateFailed, fmt.Sprintf("exit code %d", code)
	}
	var note string
	if snapshotRefused {
		note = refusedSnapshotReason
		if !hasSnapshot {
			note = refusedNoSnapshotReason
		}
	}
	next, err = s.endRun(ctx, tx, runEnd{tenantID: tenantID, runID: runID, epoch: epoch, policy: policy, state: next, reason: reason, note: note})
	if err != nil {
		return err
	}
	// A refused move cannot resume from its rejected snapshot; leave the
	// Run stopped for a person to decide what to restore. restart is
	// exempt: it restores nothing, and forgetRestoredState clears the
	// snapshot_id the refusal left. A terminate that raced the move wins:
	// a terminated Run is never placed again.
	moved := moveStop && !failsOnMove && next != StateTerminated && (!snapshotRefused || restarts)
	// Servers count as migrated only when the Run is resumed elsewhere.
	serverStop := endReason("stop")
	if moved {
		serverStop = endReason(stopReason)
	}
	if err := stopServersAtEnd(ctx, tx, tenantID, runID, epoch, serverStop); err != nil {
		return err
	}
	placementEnded := map[string]any{"run": runID, "epoch": epoch, "outcome": next}
	if st.ExitCode != nil {
		placementEnded["exitCode"] = *st.ExitCode
	}
	if stopReason != "" {
		placementEnded["stopReason"] = stopReason
	}
	if moved {
		// Moved, not stopped by a person: placed again automatically
		// (with the input a migration left, if any). restart: from
		// scratch, as a first placement, its snapshot ignored.
		why := "auto-resume after " + stopReason
		if restarts {
			if err := forgetRestoredState(ctx, tx, runID); err != nil {
				return err
			}
			why = "auto-restart after " + stopReason
		}
		if err := s.requestResume(ctx, tx, tenantID, runID, nil, why); err != nil {
			return err
		}
	}
	// Last: event streams come after every row lock (infraevents.go).
	return hostEvent(ctx, tx, hostID, evPlacementEnded, placementEnded)
}

// placementLost gives up on a placement: its host stopped answering (or came
// back without it). Its Run is lost and resumable only from the last
// snapshot taken before it. Its host.placement_ended goes to later, for
// the caller to write once it has locked every row it will (callers lose
// several placements in one transaction; event streams come last). A Run
// whose resumePolicy is never ends terminated instead (neverResumedEnd).
func (s *Server) placementLost(ctx context.Context, tx pgx.Tx, runID string, epoch int, why string, later *laterEvents) error {
	_, err := s.losePlacement(ctx, tx, runID, epoch, why, later, false)
	return err
}

// requeueUnstartedPlacement gives up on a placement whose assignment a
// never reached its runner and places its Run again, with a's input and sync:
// lost is not how the Run ends, so its resumePolicy is not applied. A Run
// with a terminate pending ends terminated instead.
func (s *Server) requeueUnstartedPlacement(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, why string, later *laterEvents, a proto.Assign) error {
	state, err := s.losePlacement(ctx, tx, runID, epoch, why, later, true)
	if err != nil || state != StateLost {
		return err
	}
	if err := s.requestResume(ctx, tx, tenantID, runID, a.Input, why); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET pending_sync = $2 WHERE id = $1`, runID, a.Sync)
	return err
}

// losePlacement is placementLost, returning the state it wrote ("" for
// none); requeued: the caller places the Run again in the same
// transaction, so lost is not how it ends, and it is written as is.
func (s *Server) losePlacement(ctx context.Context, tx pgx.Tx, runID string, epoch int, why string, later *laterEvents, requeued bool) (string, error) {
	var tenantID, runState string
	var current int
	if err := tx.QueryRow(ctx, `SELECT tenant_id, state, current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&tenantID, &runState, &current); err != nil {
		return "", err
	}
	var hostID string
	if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&hostID); err != nil {
		return "", err
	}
	if err := lockCostHost(ctx, tx, hostID); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE placements SET state = 'lost', ended_at = now(), exit_reason = $3, lease_expires_at = NULL
		WHERE run_id = $1 AND epoch = $2 AND state IN `+livePlacementStates+``, runID, epoch, why)
	if err != nil || tag.RowsAffected() == 0 {
		return "", err
	}
	later.host(ctx, tx, hostID, evPlacementEnded, map[string]any{"run": runID, "epoch": epoch, "outcome": "lost", "reason": why})
	if epoch != current || ended(runState) {
		return "", nil
	}
	if err := stopServersAtEnd(ctx, tx, tenantID, runID, epoch, "host lost"); err != nil {
		return "", err
	}
	var terminate bool
	_ = tx.QueryRow(ctx, `SELECT terminate_requested FROM runs WHERE id = $1`, runID).Scan(&terminate)
	end := runEnd{tenantID: tenantID, runID: runID, epoch: epoch, state: StateLost, reason: why, requeued: requeued}
	switch {
	case terminate:
		end.state, end.reason, end.requeued = StateTerminated, "terminated; host lost", false
	case !requeued:
		if end.policy, err = runResumePolicy(ctx, tx, runID); err != nil {
			return "", err
		}
	}
	return s.endRun(ctx, tx, end)
}

// foreignBlobsReason is the snapshot.failed error for a refused report.
const foreignBlobsReason = "the report does not match this Run's blob records"

// refusedSnapshotReason (or refusedNoSnapshotReason, for a Run without an
// earlier snapshot) is added to the state_reason of a Run whose placement's
// snapshot report was refused.
const (
	refusedSnapshotReason   = "the last snapshot was refused; the Run still has its previous snapshot"
	refusedNoSnapshotReason = "the last snapshot was refused; the Run has no earlier snapshot"
)

// foreignBlobError: a snapshot report does not match its Run's records: it
// names a blob or snapshot id recorded for another Run, or for this Run as
// something else.
type foreignBlobError struct{ id string }

func (e *foreignBlobError) Error() string { return "id " + e.id + " does not match this Run's records" }

// applySnapshotDone records a placement's final state. A report that does
// not match its Run's records is refused whole: none of its records are
// stored (not even snapshot_done_at), the placement is marked
// snapshot_refused, a snapshot.failed event says so, and the report is
// still acknowledged, as resending it cannot change the outcome. refused:
// the reported snapshot id is not one recorded for this placement, so the
// runner may delete its files.
func (s *Server) applySnapshotDone(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch, current int, sd proto.SnapshotDone) (refused bool, err error) {
	var placementID string
	if err := tx.QueryRow(ctx, `SELECT id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&placementID); err != nil {
		return false, err
	}
	// A savepoint, so that a refusal found halfway undoes what came before.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	if _, err := sp.Exec(ctx, `UPDATE placements SET snapshot_done_at = coalesce(snapshot_done_at, now()) WHERE id = $1`, placementID); err != nil {
		return false, err
	}
	if sd.Error != "" {
		if err := addEvent(ctx, sp, tenantID, runID, epoch, "snapshot.failed", map[string]any{"error": sd.Error}); err != nil {
			return false, err
		}
		return false, sp.Commit(ctx)
	}
	recorded, err := s.recordSnapshot(ctx, sp, tenantID, hostID, runID, placementID, epoch, current, sd)
	var foreign *foreignBlobError
	if errors.As(err, &foreign) {
		if err := sp.Rollback(ctx); err != nil {
			return false, err
		}
		s.log.Warn("snapshot report refused", "host", hostID, "run", runID, "epoch", epoch, "err", err)
		// A recorded snapshot keeps its files, but any refused latest report
		// prevents this placement from auto-resuming on exit.
		if err := tx.QueryRow(ctx, `UPDATE placements SET snapshot_refused = true
			WHERE id = $1 RETURNING NOT EXISTS (SELECT 1 FROM snapshots WHERE id = $2 AND placement_id = $1)`,
			placementID, sd.Manifest.SnapshotID).Scan(&refused); err != nil {
			return false, err
		}
		return refused, addEvent(ctx, tx, tenantID, runID, epoch, "snapshot.failed", map[string]any{"error": foreignBlobsReason})
	}
	if err != nil {
		return false, err
	}
	// A new report recorded after a refused one (a restarted runner
	// snapshots again): the placement's snapshot is the Run's after all. A
	// redelivered older report changes nothing, so a later refusal stands.
	if recorded {
		if _, err := sp.Exec(ctx, `UPDATE placements SET snapshot_refused = false WHERE id = $1 AND snapshot_refused`, placementID); err != nil {
			return false, err
		}
	}
	return false, sp.Commit(ctx)
}

// recordSnapshot stores a report. recorded: it was a new snapshot, rather
// than a redelivery of one already recorded (which stores nothing).
func (s *Server) recordSnapshot(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID, placementID string, epoch, current int, sd proto.SnapshotDone) (recorded bool, err error) {
	var snapRun, snapPlacement string
	var snapEpoch int
	var ownsRecords bool
	var stored proto.Manifest
	err = tx.QueryRow(ctx, `SELECT run_id, placement_id, epoch, manifest, owns_records FROM snapshots WHERE id = $1`,
		sd.Manifest.SnapshotID).Scan(&snapRun, &snapPlacement, &snapEpoch, &stored, &ownsRecords)
	switch {
	case err == nil && snapRun == runID && snapPlacement == placementID && snapEpoch == epoch && reflect.DeepEqual(stored, sd.Manifest):
		// Redelivered, if its output and artifacts are the recorded ones too.
		return false, recordedBlobsMatch(ctx, tx, runID, epoch, ownsRecords, sd)
	case err == nil:
		return false, &foreignBlobError{sd.Manifest.SnapshotID}
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	return true, insertSnapshot(ctx, tx, tenantID, hostID, runID, placementID, epoch, current, sd)
}

func insertSnapshot(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID, placementID string, epoch, current int, sd proto.SnapshotDone) error {
	var total int64
	for _, v := range sd.Manifest.Volumes {
		total += v.Size
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, v.BlobID, "volume", v.Name, v.Size, v.SHA256, ""); err != nil {
			return err
		}
	}
	if sd.Output != nil {
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, sd.Output.BlobID, "output", "output", sd.Output.Size, sd.Output.SHA256, sd.Manifest.SnapshotID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE placements SET output_blob_id = $3, output_seq = greatest(output_seq, nullif($4::bigint, 0))
			WHERE run_id = $1 AND epoch = $2`, runID, epoch, sd.Output.BlobID, sd.OutputSeq); err != nil {
			return err
		}
	}
	for _, a := range sd.Artifacts {
		// The latest version again: nothing is recorded, not even its blob,
		// whose upload luxd then answers 404 (the runner counts it done). A
		// blob id already recorded goes on to insertBlob's check.
		var blobKnown bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE id = $1)`, a.BlobID).Scan(&blobKnown); err != nil {
			return err
		}
		latestSHA, latest, err := latestArtifact(ctx, tx, runID, a.Path)
		if err != nil {
			return err
		}
		if !blobKnown && latest > 0 && latestSHA == a.FileSHA256 {
			continue
		}
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, a.BlobID, "artifact", a.Path, a.Size, a.SHA256, sd.Manifest.SnapshotID); err != nil {
			return err
		}
		if err := insertArtifact(ctx, tx, tenantID, runID, epoch, ids.New(ids.Artifact), sd.Manifest.SnapshotID, "", a); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE placements SET snapshot_bytes = $3 WHERE run_id = $1 AND epoch = $2`, runID, epoch, total); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, owns_records)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true)`, sd.Manifest.SnapshotID, tenantID, runID, placementID, epoch, sd.Manifest, hostID); err != nil {
		return err
	}
	// Recorded for any epoch, late ones too: the session ran in that
	// placement even when it no longer becomes the Run's.
	if sd.Manifest.SessionID != "" {
		if err := recordSession(ctx, tx, tenantID, runID, epoch, sd.Manifest.SessionID); err != nil {
			return err
		}
	}
	// Only the current placement's snapshot becomes the Run's: an old host
	// reporting late must not roll the Run back. Either way the Run may now
	// have a snapshot that is not its current one (reapSuperseded).
	if epoch == current {
		if _, err := tx.Exec(ctx, `UPDATE runs SET snapshot_id = $2,
				session_id = CASE WHEN $3 <> '' THEN $3 ELSE session_id END,
				snapshots_superseded = snapshots_superseded OR snapshot_id IS NOT NULL
			WHERE id = $1`, runID, sd.Manifest.SnapshotID, sd.Manifest.SessionID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE runs SET snapshots_superseded = true WHERE id = $1`, runID); err != nil {
		return err
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, "snapshot", map[string]any{"snapshotId": sd.Manifest.SnapshotID, "bytes": total, "volumes": len(sd.Manifest.Volumes)})
}

// recordedBlobsMatch compares a redelivered report's output and artifacts
// with those its snapshot recorded: the output blob and artifact rows whose
// snapshot_id is that snapshot, in full. Another report of the same
// placement recorded its own, so they are not compared. ownsRecords is the
// snapshot row's owns_records.
func recordedBlobsMatch(ctx context.Context, tx pgx.Tx, runID string, epoch int, ownsRecords bool, sd proto.SnapshotDone) error {
	snapID := sd.Manifest.SnapshotID
	// Recorded before output and artifact rows carried their snapshot_id:
	// there is nothing to tell its rows from another report's, so only the
	// manifest (already compared) is checked.
	if !ownsRecords {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id, size, sha256 FROM blobs
		WHERE snapshot_id = $1 AND run_id = $2 AND epoch = $3 AND kind = 'output'`, snapID, runID, epoch)
	if err != nil {
		return err
	}
	outputs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[proto.BlobInfo])
	if err != nil {
		return err
	}
	if len(outputs) > 1 || (len(outputs) == 1) != (sd.Output != nil) || sd.Output != nil && outputs[0] != *sd.Output {
		return &foreignBlobError{snapID}
	}
	rows, err = tx.Query(ctx, `SELECT a.blob_id, b.size, b.sha256, a.path, a.content_type, a.size, a.sha256
		FROM artifacts a JOIN blobs b ON b.id = a.blob_id
		WHERE a.snapshot_id = $1 AND a.run_id = $2 AND a.epoch = $3 AND b.run_id = $2 AND b.epoch = $3 AND b.kind = 'artifact'`,
		snapID, runID, epoch)
	if err != nil {
		return err
	}
	defer rows.Close()
	stored := map[proto.Artifact]int{}
	count := 0
	for rows.Next() {
		var a proto.Artifact
		if err := rows.Scan(&a.BlobID, &a.Size, &a.SHA256, &a.Path, &a.ContentType, &a.FileSize, &a.FileSHA256); err != nil {
			return err
		}
		stored[a]++
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range sd.Artifacts {
		if stored[a] > 0 {
			stored[a]--
			count--
			continue
		}
		// Not recorded, as the same content as its path's latest version
		// was not: its blob must be unknown and that content recorded.
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE id = $1)
			OR NOT EXISTS (SELECT 1 FROM artifacts WHERE run_id = $2 AND md5(path) = md5($3) AND path = $3 AND sha256 = $4)`,
			a.BlobID, runID, a.Path, a.FileSHA256).Scan(&known); err != nil {
			return err
		}
		if known {
			return &foreignBlobError{a.BlobID}
		}
	}
	if count != 0 {
		return &foreignBlobError{snapID}
	}
	return nil
}

// recordSession notes that a Run's placement epoch had session id: one row
// per (run, epoch, id), its last_seen moved on a repeat.
func recordSession(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, epoch, session_id) DO UPDATE SET last_seen = now()`, tenantID, runID, epoch, id)
	return err
}

// insertBlob records a reported blob. The runner names a new blob id in
// every report, so an id already recorded is accepted only as the same
// blob of the same placement (a report written again); anything else is a
// *foreignBlobError. snapshotID names the report that owns an output or
// artifact blob ("" for a volume); a blob recorded before keeps its owner.
func insertBlob(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, hostID, blobID, kind, name string, size int64, sha, snapshotID string) error {
	var gotTenant, gotRun, gotKind, gotSHA string
	var gotSnapshotID *string
	var gotEpoch int
	var gotSize int64
	// DO UPDATE (a no-op) rather than DO NOTHING, so the existing row is
	// returned, locked, to compare.
	err := tx.QueryRow(ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, sha256, location, host_id, snapshot_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'host', $9, nullif($10, '')) ON CONFLICT (id) DO UPDATE SET id = blobs.id
		RETURNING tenant_id, run_id, epoch, kind, size, sha256, snapshot_id`,
		blobID, tenantID, runID, epoch, kind, name, size, sha, hostID, snapshotID).Scan(&gotTenant, &gotRun, &gotEpoch, &gotKind, &gotSize, &gotSHA, &gotSnapshotID)
	if err != nil {
		return err
	}
	if gotTenant != tenantID || gotRun != runID || gotEpoch != epoch || gotKind != kind || gotSize != size || gotSHA != sha ||
		(snapshotID != "" && gotSnapshotID != nil && *gotSnapshotID != snapshotID) {
		return &foreignBlobError{blobID}
	}
	return nil
}

func (s *Server) applyAdapterEvent(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, ev proto.AdapterEvent) error {
	if ev.SessionID != "" {
		if _, err := tx.Exec(ctx, `UPDATE runs SET session_id = $2 WHERE id = $1`, runID, ev.SessionID); err != nil {
			return err
		}
		addEvent(ctx, tx, tenantID, runID, epoch, "session", map[string]any{"sessionId": ev.SessionID})
		if err := recordSession(ctx, tx, tenantID, runID, epoch, ev.SessionID); err != nil {
			return err
		}
	}
	if ev.Activity != "" {
		if _, err := tx.Exec(ctx, `UPDATE runs SET activity = $2 WHERE id = $1 AND state = 'running'`, runID, ev.Activity); err != nil {
			return err
		}
		addEvent(ctx, tx, tenantID, runID, epoch, "activity", map[string]any{"activity": ev.Activity})
	}
	if ev.InputAck != "" {
		if err := applyInputEvent(ctx, tx, tenantID, runID, epoch, ev); err != nil {
			return err
		}
	}
	if p := ev.InputProgress; p != nil && p.RequestID != "" {
		return applyInputProgress(ctx, tx, tenantID, runID, epoch, *p)
	}
	return nil
}

// applyInputEvent records an input's first answer (lux.input) as
// input.delivered (accepted) or input.failed.
func applyInputEvent(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, ev proto.AdapterEvent) error {
	phase := ev.InputPhase
	if phase == "" {
		phase = proto.InputAccepted
		if ev.InputError != "" {
			phase = proto.InputFailed
		}
	}
	d := map[string]any{"requestId": ev.InputAck, "phase": phase}
	typ := "input.delivered"
	switch phase {
	case proto.InputFailed:
		typ, d["error"] = "input.failed", ev.InputError
	case proto.InputAccepted:
		if ev.InputLands != "" {
			d["lands"], d["receipt"] = ev.InputLands, ev.InputReceipt
		}
	default:
		return nil
	}
	if ev.InputText != "" {
		d["text"] = ev.InputText
	}
	if ev.InputTruncated {
		d["truncated"] = true
	}
	if len(ev.InputAttachments) > 0 {
		d["attachments"] = ev.InputAttachments
	}
	return addInputEvent(ctx, tx, tenantID, runID, epoch, typ, ev.InputAck, d)
}

// applyInputProgress records what happened to an accepted input
// (lux.input.consumed, lux.input.failed) as input.consumed or input.failed.
func applyInputProgress(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, p proto.InputProgress) error {
	switch p.Phase {
	case proto.InputConsumed:
		return addInputEvent(ctx, tx, tenantID, runID, epoch, "input.consumed", p.RequestID, map[string]any{"requestId": p.RequestID})
	case proto.InputFailed:
		d := map[string]any{"requestId": p.RequestID, "error": p.Error}
		if len(p.Attachments) > 0 {
			d["attachments"] = p.Attachments
		}
		return addInputEvent(ctx, tx, tenantID, runID, epoch, "input.failed", p.RequestID, d)
	}
	return nil
}

// addInputEvent adds an input event once per request id and type: the
// runner re-reports what it tails after a restart or reconnect. The key is
// claimed in run_input_events first, a primary-key insert.
func addInputEvent(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, typ, requestID string, d map[string]any) error {
	tag, err := tx.Exec(ctx, `INSERT INTO run_input_events (tenant_id, run_id, type, request_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, tenantID, runID, typ, requestID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, typ, d)
}

// discardOldCopies tells every other host holding a local copy of a Run's
// older snapshots to delete it: the Run now runs elsewhere, from a copy in
// S3 (or its own). Only copies already uploaded are discarded; a copy that
// is the only one stays until its upload finishes (the runner will not drop
// a pending upload).
func discardOldCopies(ctx context.Context, tx pgx.Tx, runID string, epoch int, newHost string) error {
	rows, err := tx.Query(ctx, `UPDATE snapshots SET host_copy = false
		WHERE run_id = $1 AND epoch < $2 AND host_copy AND uploaded AND host_id IS NOT NULL AND host_id <> $3
		RETURNING host_id`, runID, epoch, newHost)
	if err != nil {
		return err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	slices.Sort(hosts)
	for _, h := range slices.Compact(hosts) {
		// Not urgent: the hub's delivery sweep sends it after commit.
		if err := enqueue(ctx, tx, h, runID, 0, proto.MsgSnapshotDiscard, map[string]any{"runId": runID, "beforeEpoch": epoch}); err != nil {
			return err
		}
	}
	return nil
}
