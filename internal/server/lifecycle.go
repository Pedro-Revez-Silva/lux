package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
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
	StateCancelled    = "cancelled"
	StateLost         = "lost"
)

// livePlacementStates, for SQL: a placement that is (or is about to be)
// running on its host.
const livePlacementStates = "('assigned', 'starting', 'running', 'stopping')"

// queuedRunStates, for SQL: Runs waiting for a host.
const queuedRunStates = "('submitted', 'resuming', 'provisioning')"

// resumableRunStates, for SQL: Runs resume accepts.
const resumableRunStates = "('stopped', 'lost', 'failed')"

// movedStops: stop reasons that move a Run rather than stop it; it is
// resumed elsewhere as soon as it has stopped.
var movedStops = []string{"drain", "preempt", "migrate"}

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

// Terminal: a Run in this state has ended for good.
func Terminal(state string) bool { return terminal(state) }

func terminal(state string) bool {
	return state == StateSucceeded || state == StateFailed || state == StateCancelled
}

// live: a placement exists and is (or is about to be) running.
func live(state string) bool {
	switch state {
	case StateScheduled, StateStarting, StateRunning, StateStopping:
		return true
	}
	return false
}

func setRunState(ctx context.Context, tx pgx.Tx, tenantID, runID, state, reason string, epoch int) error {
	if terminal(state) {
		// A terminal Run begins a new settlement epoch after a stopped/lost one.
		if _, err := tx.Exec(ctx, `UPDATE cost_sources SET settles_left = NULL, next_at = NULL, attempts = 0
			WHERE run_id = $1 AND source <> 'compute' AND EXISTS
			(SELECT 1 FROM runs WHERE id = $1 AND state IN ('stopped', 'lost'))`, runID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE runs SET state = $2, state_reason = $3, updated_at = now(),
			finished_at = CASE WHEN $2 IN ('succeeded', 'failed', 'cancelled') THEN now() ELSE finished_at END,
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
	return addEvent(ctx, tx, tenantID, runID, epoch, "state", data)
}

// costStates: a Run entering one of these has its costs evaluated at once
// (docs/costs.md, section 5), queued with the state change itself.
var costStates = map[string]bool{
	StateStopping: true, StateStopped: true, StateLost: true,
	StateSucceeded: true, StateFailed: true, StateCancelled: true,
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
			exited_at            = coalesce(exited_at, $7)
		WHERE run_id = $1 AND epoch = $2`, runID, epoch,
		msToTime(t["imageReady"]), msToTime(t["volumesRestored"]), msToTime(t["containerStarted"]),
		msToTime(t["workloadStarted"]), msToTime(t["exited"]))
	if err != nil {
		return err
	}
	if st.Usage != nil {
		if err := recordUsage(ctx, tx, runID, epoch, st.Usage); err != nil {
			return err
		}
	}

	var runState string
	var cancel bool
	if err := tx.QueryRow(ctx, `SELECT state, cancel_requested FROM runs WHERE id = $1`, runID).Scan(&runState, &cancel); err != nil {
		return err
	}
	if terminal(runState) {
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
		return s.placementExited(ctx, tx, tenantID, runID, epoch, st, runState, cancel)
	}
	return nil
}

// placementExited records a placement's end and decides what the Run
// becomes. The snapshot arrives separately (snapshot.done); a stopped Run is
// resumable once it has.
func (s *Server) placementExited(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, st proto.Status, runState string, cancel bool) error {
	var stopReason, hostID string
	if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&hostID); err != nil {
		return err
	}
	if err := lockCostHost(ctx, tx, hostID); err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `UPDATE placements SET state = 'exited', ended_at = now(),
			exited_at = coalesce(exited_at, now()), exit_code = $3, exit_reason = $4, output_seq = nullif($5, 0),
			lease_expires_at = NULL
		WHERE run_id = $1 AND epoch = $2 AND state <> 'exited'
		RETURNING stop_reason, host_id`, runID, epoch, st.ExitCode, st.Reason, st.OutputSeq).Scan(&stopReason, &hostID)
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

	var next, reason string
	switch {
	case cancel || stopReason == "cancel":
		next, reason = StateCancelled, "cancelled"
	case stopReason == "timeout":
		next, reason = StateFailed, "timeout"
	case stopReason == "disk":
		next, reason = StateFailed, "disk limit exceeded"
	case stopReason == "stop" || slices.Contains(movedStops, stopReason):
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
	if err := setRunState(ctx, tx, tenantID, runID, next, reason, epoch); err != nil {
		return err
	}
	ended := map[string]any{"run": runID, "epoch": epoch, "outcome": next}
	if st.ExitCode != nil {
		ended["exitCode"] = *st.ExitCode
	}
	if stopReason != "" {
		ended["stopReason"] = stopReason
	}
	if err := hostEvent(ctx, tx, hostID, evPlacementEnded, ended); err != nil {
		return err
	}
	if slices.Contains(movedStops, stopReason) {
		// Moved, not stopped by a person: resume elsewhere automatically
		// (with the input a migration left, if any).
		return s.requestResume(ctx, tx, tenantID, runID, nil, "auto-resume after "+stopReason)
	}
	if terminal(next) {
		s.secrets.drop(runID)
	}
	return nil
}

// placementLost gives up on a placement: its host stopped answering (or came
// back without it). Its Run is lost and resumable only from the last
// snapshot taken before it.
func (s *Server) placementLost(ctx context.Context, tx pgx.Tx, runID string, epoch int, why string) error {
	var tenantID, runState string
	var current int
	if err := tx.QueryRow(ctx, `SELECT tenant_id, state, current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&tenantID, &runState, &current); err != nil {
		return err
	}
	var hostID string
	if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, runID, epoch).Scan(&hostID); err != nil {
		return err
	}
	if err := lockCostHost(ctx, tx, hostID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE placements SET state = 'lost', ended_at = now(), exit_reason = $3, lease_expires_at = NULL
		WHERE run_id = $1 AND epoch = $2 AND state IN `+livePlacementStates+``, runID, epoch, why)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if err := hostEvent(ctx, tx, hostID, evPlacementEnded, map[string]any{"run": runID, "epoch": epoch, "outcome": "lost", "reason": why}); err != nil {
		return err
	}
	if epoch != current || terminal(runState) {
		return nil
	}
	var cancel bool
	_ = tx.QueryRow(ctx, `SELECT cancel_requested FROM runs WHERE id = $1`, runID).Scan(&cancel)
	if cancel {
		return setRunState(ctx, tx, tenantID, runID, StateCancelled, "cancelled; host lost", epoch)
	}
	return setRunState(ctx, tx, tenantID, runID, StateLost, why, epoch)
}

// applySnapshotDone records a placement's final state.
func (s *Server) applySnapshotDone(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch, current int, sd proto.SnapshotDone) error {
	var placementID string
	if err := tx.QueryRow(ctx, `UPDATE placements SET snapshot_done_at = coalesce(snapshot_done_at, now())
		WHERE run_id = $1 AND epoch = $2 RETURNING id`, runID, epoch).Scan(&placementID); err != nil {
		return err
	}
	if sd.Error != "" {
		return addEvent(ctx, tx, tenantID, runID, epoch, "snapshot.failed", map[string]any{"error": sd.Error})
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM snapshots WHERE id = $1)`, sd.Manifest.SnapshotID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil // redelivered
	}
	var total int64
	for _, v := range sd.Manifest.Volumes {
		total += v.Size
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, v.BlobID, "volume", v.Name, v.Size, v.SHA256); err != nil {
			return err
		}
	}
	if sd.Output != nil {
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, sd.Output.BlobID, "output", "output", sd.Output.Size, sd.Output.SHA256); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE placements SET output_blob_id = $3, output_seq = greatest(output_seq, nullif($4, 0))
			WHERE run_id = $1 AND epoch = $2`, runID, epoch, sd.Output.BlobID, sd.OutputSeq); err != nil {
			return err
		}
	}
	for _, a := range sd.Artifacts {
		if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, a.BlobID, "artifact", a.Path, a.Size, a.SHA256); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id, content_type, size, sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
			ids.New(ids.Artifact), tenantID, runID, epoch, a.Path, a.BlobID, a.ContentType, a.FileSize, a.FileSHA256); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE placements SET snapshot_bytes = $3 WHERE run_id = $1 AND epoch = $2`, runID, epoch, total); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, sd.Manifest.SnapshotID, tenantID, runID, placementID, epoch, sd.Manifest, hostID); err != nil {
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
	// reporting late must not roll the Run back.
	if epoch == current {
		if _, err := tx.Exec(ctx, `UPDATE runs SET snapshot_id = $2,
				session_id = CASE WHEN $3 <> '' THEN $3 ELSE session_id END
			WHERE id = $1`, runID, sd.Manifest.SnapshotID, sd.Manifest.SessionID); err != nil {
			return err
		}
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, "snapshot", map[string]any{"snapshotId": sd.Manifest.SnapshotID, "bytes": total, "volumes": len(sd.Manifest.Volumes)})
}

// recordSession notes that a Run's placement epoch had session id: one row
// per (run, epoch, id), its last_seen moved on a repeat.
func recordSession(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, epoch, session_id) DO UPDATE SET last_seen = now()`, tenantID, runID, epoch, id)
	return err
}

func insertBlob(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, hostID, blobID, kind, name string, size int64, sha string) error {
	_, err := tx.Exec(ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, sha256, location, host_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'host', $9) ON CONFLICT (id) DO NOTHING`,
		blobID, tenantID, runID, epoch, kind, name, size, sha, hostID)
	return err
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
		d := map[string]any{"requestId": ev.InputAck}
		if ev.InputText != "" {
			d["text"] = ev.InputText
		}
		if ev.InputTruncated {
			d["truncated"] = true
		}
		typ := "input.delivered"
		if ev.InputError != "" {
			typ, d["error"] = "input.failed", ev.InputError
		}
		addEvent(ctx, tx, tenantID, runID, epoch, typ, d)
	}
	return nil
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
