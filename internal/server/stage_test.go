package server

import (
	"testing"
	"time"
)

// Every stage of the table in docs/concepts.md#stages, from what luxd
// records, a move included: its stopping placement, then the next one
// waiting, assigned, accepted.
func TestDeriveStage(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(s int) *time.Time { v := t0.Add(time.Duration(s) * time.Second); return &v }
	created := t0
	accepted := func(in stageInputs) stageInputs {
		in.State, in.PState, in.PAccepted = StateStarting, "starting", at(2)
		return in
	}
	for _, c := range []struct {
		name   string
		in     stageInputs
		stage  string
		since  time.Time
		reason string
	}{
		{"submitted: since needs_host_since", stageInputs{State: StateSubmitted, NeedsHostSince: at(3)}, StageWaiting, *at(3), ""},
		{"submitted before needs_host_since was kept", stageInputs{State: StateSubmitted}, StageWaiting, created, ""},
		{"provisioning", stageInputs{State: StateProvisioning, NeedsHostSince: at(3)}, StageWaiting, *at(3), ""},
		{"resumed before needs_host_since was kept: since its last placement's end", stageInputs{State: StateResuming, LastEnded: at(30), PState: "exited"}, StageWaiting, *at(30), ""},
		{"requeued: since the wait the given-up placement began", stageInputs{State: StateResuming, WaitingSince: at(3), NeedsHostSince: at(30), LastEnded: at(30), PState: "lost"}, StageWaiting, *at(3), ""},
		{"requeued, assigned again", stageInputs{State: StateScheduled, WaitingSince: at(3), LastEnded: at(30), PState: "assigned", PNeededSince: at(30)}, StageWaiting, *at(3), ""},
		{"assigned, not accepted", stageInputs{State: StateScheduled, PState: "assigned", PNeededSince: at(3)}, StageWaiting, *at(3), ""},
		{"assigned before needed_since was kept: since the last placement's end", stageInputs{State: StateScheduled, LastEnded: at(30), PState: "assigned"}, StageWaiting, *at(30), ""},
		{"accepted", accepted(stageInputs{}), StageImage, *at(2), ""},
		{"image ready", func() stageInputs { in := accepted(stageInputs{}); in.PImageReady = at(5); return in }(), StageVolumes, *at(5), ""},
		{"volumes restored", func() stageInputs {
			in := accepted(stageInputs{})
			in.PImageReady, in.PVolumesRestored = at(5), at(7)
			return in
		}(), StageRepositories, *at(7), ""},
		{"repositories ready", func() stageInputs {
			in := accepted(stageInputs{})
			in.PImageReady, in.PVolumesRestored, in.PReposReady = at(5), at(7), at(9)
			return in
		}(), StageContainer, *at(9), ""},
		{"no repositories: repositories ends as it begins", func() stageInputs {
			in := accepted(stageInputs{})
			in.PImageReady, in.PVolumesRestored, in.PReposReady = at(5), at(7), at(7)
			return in
		}(), StageContainer, *at(7), ""},
		{"container started, running not yet seen", func() stageInputs {
			in := accepted(stageInputs{})
			in.PImageReady, in.PVolumesRestored, in.PReposReady, in.PContainerStarted = at(5), at(7), at(9), at(11)
			return in
		}(), StageRunning, *at(11), ""},
		{"running", stageInputs{State: StateRunning, PState: "running", PAccepted: at(2), PContainerStarted: at(11), PStarted: at(12)}, StageRunning, *at(11), ""},
		{"running, from a runner without containerStarted", stageInputs{State: StateRunning, PState: "running", PAccepted: at(2), PStarted: at(12)}, StageRunning, *at(12), ""},
		{"stopping on its own, from a runner without containerStarted", stageInputs{State: StateRunning, PState: "stopping", PAccepted: at(2), PImageReady: at(5), PStarted: at(12)}, StageRunning, *at(12), ""},
		{"stopping without a stop request", stageInputs{State: StateStopping, StateChangedAt: *at(25), PState: "running", PContainerStarted: at(11)}, StageStopping, *at(25), ""},
		{"stopping", stageInputs{State: StateStopping, PState: "running", PContainerStarted: at(11), PStopRequested: at(20), PStopReason: "stop"}, StageStopping, *at(20), "stop"},
		{"a start stopped", stageInputs{State: StateStopping, PState: "starting", PAccepted: at(2), PImageReady: at(5), PStopRequested: at(6), PStopReason: "terminate"}, StageStopping, *at(6), "terminate"},
		{"a move: stopping", stageInputs{State: StateStopping, PState: "stopping", PContainerStarted: at(11), PStopRequested: at(20), PStopReason: "migrate"}, StageStopping, *at(20), "migrate"},
		{"a move: resuming once its placement ended", stageInputs{State: StateResuming, NeedsHostSince: at(30), LastEnded: at(30),
			PState: "exited", PContainerStarted: at(11), PStopRequested: at(20), PStopReason: "migrate"}, StageWaiting, *at(30), ""},
		{"a move: the next placement assigned", stageInputs{State: StateScheduled, LastEnded: at(30), PState: "assigned", PNeededSince: at(30)}, StageWaiting, *at(30), ""},
		{"a move: the next placement accepted", stageInputs{State: StateScheduled, LastEnded: at(30), PState: "assigned", PNeededSince: at(30), PAccepted: at(33)}, StageImage, *at(33), ""},
		{"stopped", stageInputs{State: StateStopped, StateChangedAt: *at(40), PState: "exited", PStopRequested: at(20), PStopReason: "stop"}, StateStopped, *at(40), ""},
		{"lost", stageInputs{State: StateLost, StateChangedAt: *at(40), PState: "lost"}, StateLost, *at(40), ""},
		{"succeeded", stageInputs{State: StateSucceeded, StateChangedAt: *at(40), PState: "exited"}, StateSucceeded, *at(40), ""},
		{"failed", stageInputs{State: StateFailed, StateChangedAt: *at(40), PState: "exited"}, StateFailed, *at(40), ""},
		{"terminated", stageInputs{State: StateTerminated, StateChangedAt: *at(40), PState: "exited"}, StateTerminated, *at(40), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.in.CreatedAt = created
			got := deriveStage(c.in)
			if got.Stage != c.stage || !got.Since.Equal(c.since) || got.Reason != c.reason {
				t.Errorf("got %s since %s (%q), want %s since %s (%q)", got.Stage, got.Since.Format(time.TimeOnly), got.Reason,
					c.stage, c.since.Format(time.TimeOnly), c.reason)
			}
		})
	}
}
