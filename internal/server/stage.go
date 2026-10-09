package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// A Run's stages: where it is on its way to running, or out of it. The
// start stages follow the runner's marks (docs/concepts.md#stages).
const (
	StageWaiting      = "waiting"      // for a host: queued, or assigned and not yet accepted
	StageImage        = "image"        // pulling or building the image
	StageVolumes      = "volumes"      // restoring the state volumes
	StageRepositories = "repositories" // cloning repositories, fetching a sync
	StageContainer    = "container"    // creating and starting the container
	StageRunning      = "running"
	StageStopping     = "stopping" // stopping, snapshotting, reporting (stageReason: the stop's)
)

// stageInputs is what a Run's stage is derived from: the Run and its
// current placement (p*, all nil or empty without one).
type stageInputs struct {
	State          string
	StateChangedAt time.Time
	CreatedAt      time.Time
	NeedsHostSince *time.Time
	// WaitingSince: a requeued Run's wait, begun before the placement it
	// gave up (runs.waiting_since).
	WaitingSince *time.Time
	// LastEnded: the latest end of any of its placements.
	LastEnded *time.Time

	PState            string
	PNeededSince      *time.Time
	PAccepted         *time.Time
	PImageReady       *time.Time
	PVolumesRestored  *time.Time
	PReposReady       *time.Time
	PContainerStarted *time.Time
	PStarted          *time.Time
	PStopRequested    *time.Time
	PStopReason       string
}

// runStage is a Run's stage, when it began and, for stopping, why.
type runStage struct {
	Stage  string    `json:"stage"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason,omitempty"`
}

// stageColumns are stageInputs' columns over runsFrom (r, rp, rpt), in its
// field order.
const stageColumns = `r.state_changed_at, r.needs_host_since, r.waiting_since, rpt.last_ended,
	coalesce(rp.state, ''), rp.needed_since, rp.accepted_at, rp.image_ready_at, rp.volumes_restored_at, rp.repos_ready_at,
	rp.container_started_at, rp.started_at, rp.stop_requested_at, coalesce(rp.stop_reason, '')`

func (in *stageInputs) scanTargets() []any {
	return []any{&in.StateChangedAt, &in.NeedsHostSince, &in.WaitingSince, &in.LastEnded,
		&in.PState, &in.PNeededSince, &in.PAccepted, &in.PImageReady, &in.PVolumesRestored, &in.PReposReady,
		&in.PContainerStarted, &in.PStarted, &in.PStopRequested, &in.PStopReason}
}

// deriveStage is the stage of a Run as its inputs record it. Each since is
// one recorded time: the runner's marks on its host's clock, the rest
// luxd's.
func deriveStage(in stageInputs) runStage {
	switch {
	case resumable(in.State) || ended(in.State):
		// Entered in the transaction that ended its placement, when one did.
		return runStage{Stage: in.State, Since: in.StateChangedAt}
	}
	livePlacement := in.PState == "assigned" || in.PState == "starting" || in.PState == "running" || in.PState == "stopping"
	switch {
	case livePlacement && in.PStopRequested != nil:
		return runStage{Stage: StageStopping, Since: *in.PStopRequested, Reason: in.PStopReason}
	case in.State == StateStopping:
		return runStage{Stage: StageStopping, Since: in.StateChangedAt}
	case !livePlacement || in.State == StateSubmitted || in.State == StateResuming || in.State == StateProvisioning:
		return runStage{Stage: StageWaiting, Since: firstOf(in.WaitingSince, in.NeedsHostSince, in.LastEnded, &in.CreatedAt)}
	case in.PContainerStarted != nil:
		return runStage{Stage: StageRunning, Since: *in.PContainerStarted}
	case in.PStarted != nil:
		// A runner that reported no containerStarted: luxd saw it running.
		return runStage{Stage: StageRunning, Since: *in.PStarted}
	case in.PReposReady != nil:
		return runStage{Stage: StageContainer, Since: *in.PReposReady}
	case in.PVolumesRestored != nil:
		return runStage{Stage: StageRepositories, Since: *in.PVolumesRestored}
	case in.PImageReady != nil:
		return runStage{Stage: StageVolumes, Since: *in.PImageReady}
	case in.PAccepted != nil:
		return runStage{Stage: StageImage, Since: *in.PAccepted}
	}
	// Assigned, not yet accepted: still waiting, since it began needing
	// this host (its previous placement's end has ended_at; this one not).
	return runStage{Stage: StageWaiting, Since: firstOf(in.WaitingSince, in.PNeededSince, in.LastEnded, &in.CreatedAt)}
}

func firstOf(ts ...*time.Time) time.Time {
	for _, t := range ts {
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

// noteStage has the Run's stage announced at the end of the transaction,
// after its last change: called after each change that can move it. A
// transaction that changes the stage twice (a move's stopped, then
// waiting) announces only where it leaves it, and one that rolls back
// announces nothing. Without a Store.Tx transaction to defer to, it
// announces at once. The caller's transaction must already hold the Run's
// row (infraevents.go lock order, rule 5).
func noteStage(ctx context.Context, tx pgx.Tx, runID string) error {
	if store.BeforeCommit(tx, "stage:"+runID, func() error { return announceStage(ctx, tx, runID) }) {
		return nil
	}
	return announceStage(ctx, tx, runID)
}

// announceStage emits a stage event when the Run's stage differs from the
// one last announced, and records it, so a report that changes nothing
// (redelivered, late) announces nothing.
func announceStage(ctx context.Context, tx pgx.Tx, runID string) error {
	in := stageInputs{}
	var tenantID string
	var epoch int
	var announced []byte
	targets := append([]any{&tenantID, &epoch, &announced, &in.State, &in.CreatedAt}, in.scanTargets()...)
	err := tx.QueryRow(ctx, `SELECT r.tenant_id, r.current_epoch, r.stage_announced, r.state, r.created_at, `+stageColumns+`
		FROM runs r`+runHostJoin+runPlacementJoin("now()")+` WHERE r.id = $1 FOR UPDATE OF r`, runID).Scan(targets...)
	if err != nil {
		return err
	}
	st := deriveStage(in)
	st.Since = st.Since.Truncate(time.Microsecond) // as stored: compared after a JSON round trip
	var prev runStage
	if announced != nil && json.Unmarshal(announced, &prev) == nil && prev.Stage == st.Stage && prev.Since.Equal(st.Since) && prev.Reason == st.Reason {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET stage_announced = $2 WHERE id = $1`, runID, st); err != nil {
		return err
	}
	data := map[string]any{"stage": st.Stage, "since": st.Since.UTC(), "epoch": epoch}
	if st.Reason != "" {
		data["reason"] = st.Reason
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, "stage", data)
}
