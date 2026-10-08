package server

import (
	"cmp"
	"testing"
)

// How each end leaves a Run, by resumePolicy: a never Run, which nothing
// can resume, ends terminated whenever it would otherwise rest resumable
// (success, a requested stop, a failure, a move), its outcome in the
// reason and its exit code kept; every other policy rests resumable. A
// terminate ends terminated, whatever the policy.
func TestNeverRunEndsTerminated(t *testing.T) {
	type end struct {
		name  string
		do    func(t *testing.T, s *Server)
		state map[string]string // policy ("" for auto) -> state
		// reason when it ends terminated by its policy never
		neverReason string
		exitCode    int // -1: none recorded
		// host.placement_ended's outcome for a never Run: "" for terminated
		outcome string
	}
	stopThen := func(code int) func(t *testing.T, s *Server) {
		return func(t *testing.T, s *Server) {
			if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			exitR1With(t, s, code)
		}
	}
	// rests: every policy but never rests in state.
	rests := func(state string) map[string]string {
		return map[string]string{"": state, "auto": state, "restart": state, "manual": state, "never": StateTerminated}
	}
	ends := []end{
		{"success", func(t *testing.T, s *Server) { exitR1With(t, s, 0) },
			rests(StateSucceeded), "succeeded; resumePolicy never", 0, ""},
		{"failure", func(t *testing.T, s *Server) { exitR1With(t, s, 1) },
			rests(StateFailed), "exit code 1; resumePolicy never", 1, ""},
		{"requested stop", stopThen(143),
			rests(StateStopped), "stop; resumePolicy never", 143, ""},
		{"preempt", func(t *testing.T, s *Server) {
			if err := moveStops["preempt"](t, s); err != nil {
				t.Fatal(err)
			}
			exitR1With(t, s, 143)
		},
			// auto and restart: resuming again at once.
			map[string]string{"": StateResuming, "auto": StateResuming, "restart": StateResuming, "manual": StateFailed, "never": StateTerminated},
			"preempt: not resumed (resumePolicy never)", 143, ""},
		{"host lost", loseR1Lease,
			rests(StateLost),
			// The placement is lost whatever becomes of the Run.
			"lease expired: host stopped heartbeating; resumePolicy never", -1, StateLost},
	}
	for _, e := range ends {
		for _, policy := range []string{"", "auto", "restart", "manual", "never"} {
			t.Run(e.name+"/"+policy, func(t *testing.T) {
				s, _ := policyFixture(t, policy)
				e.do(t, s)
				var state, reason string
				var exitCode *int
				var resumable, servers bool
				systemScan(t, s, `SELECT r.state, r.state_reason, r.exit_code, `+resumableSQL+`,
						EXISTS (SELECT 1 FROM run_servers WHERE run_id = r.id)
					FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &exitCode, &resumable, &servers)
				if want := e.state[policy]; state != want {
					t.Fatalf("state %q (%q), want %q", state, reason, want)
				}
				// A Run placed again starts its next placement without one.
				if e.exitCode < 0 {
					if exitCode != nil {
						t.Errorf("exit code %d, want none", *exitCode)
					}
				} else if state != StateResuming && (exitCode == nil || *exitCode != e.exitCode) {
					t.Errorf("exit code %v, want %d", exitCode, e.exitCode)
				}
				_, held := s.secrets.get("r1")
				if policy != "never" {
					// Every rest is resumable (a Run resuming is not at rest).
					if want := state != StateResuming; resumable != want {
						t.Errorf("resumable %v, want %v", resumable, want)
					}
					// Kept for an operator's resume while it is stopped or
					// lost, and for its next placement while it resumes.
					if want := state == StateStopped || state == StateLost || state == StateResuming; held != want {
						t.Errorf("secrets held %v for %s, want %v", held, state, want)
					}
					return
				}
				var ended string
				systemScan(t, s, `SELECT data->>'outcome' FROM host_events WHERE type = 'host.placement_ended' ORDER BY id DESC LIMIT 1`, nil, &ended)
				if reason != e.neverReason {
					t.Errorf("reason %q, want %q", reason, e.neverReason)
				}
				if resumable {
					t.Error("resumable")
				}
				if held {
					t.Error("secret values still held")
				}
				if servers {
					t.Error("servers left")
				}
				if want := cmp.Or(e.outcome, StateTerminated); ended != want {
					t.Errorf("host.placement_ended outcome %q, want %q", ended, want)
				}
			})
		}
	}
}

// A never Run stopped while it waits for a host (no placement) ends
// terminated at once too, its reason saying so.
func TestNeverRunStoppedQueuedEndsTerminated(t *testing.T) {
	s, ctx := policyFixture(t, "never")
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming' WHERE id = 'r1'`)
	if _, err := s.stopRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
	if state != StateTerminated || reason != "stop; resumePolicy never" {
		t.Fatalf("state %q reason %q", state, reason)
	}
	if _, held := s.secrets.get("r1"); held {
		t.Error("a terminated Run's secret values are still held")
	}
}

// The scheduler ends a queued Run it cannot place: one whose secrets no
// luxd holds past the grace period, whose snapshot does not match its blob
// records, or whose snapshot nothing holds any more. An auto Run rests
// resumable; a never Run ends terminated, its reason not asking for a
// resume it would refuse.
func TestSchedulerEndsNeverRunTerminated(t *testing.T) {
	// r1's snapshot, recorded but held by no host and no blob store.
	const snapshotGone = `WITH snap AS (INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, available)
		VALUES ('snap-gone', 't1', 'r1', 'p1', 1, '{}', false) RETURNING id)
		UPDATE runs SET snapshot_id = (SELECT id FROM snap) WHERE id = 'r1'`
	const withSecrets = `UPDATE runs SET secrets = '[{"name":"TOKEN"}]' WHERE id = 'r1'`
	const unrestorable = `UPDATE runs SET snapshot_id = 'snap-missing' WHERE id = 'r1'`
	cases := []struct {
		name, policy string
		// submitted: r1 has never been placed (its luxd restarted before
		// its first placement), rather than resuming after p1.
		submitted   bool
		setup       string
		dropSecrets bool
		state       string
		reason      string
	}{
		{"secrets lost/auto", "", false, withSecrets, true,
			StateStopped, "secrets must be supplied again: resume with them"},
		{"secrets lost/never", "never", false, withSecrets, true,
			StateTerminated, "its secrets are no longer held; resumePolicy never"},
		{"secrets lost/never/submitted", "never", true, withSecrets, true,
			StateTerminated, "its secrets are no longer held; resumePolicy never"},
		{"unrestorable/auto", "", false, unrestorable, false,
			StateFailed, "its snapshot does not match this Run's blob records"},
		{"unrestorable/never", "never", false, unrestorable, false,
			StateTerminated, "its snapshot does not match this Run's blob records; resumePolicy never"},
		{"snapshot unavailable/auto", "", false, snapshotGone, false,
			StateLost, "its snapshot is no longer available"},
		{"snapshot unavailable/never", "never", false, snapshotGone, false,
			StateTerminated, "its snapshot is no longer available; resumePolicy never"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := policyFixture(t, c.policy)
			if c.submitted {
				execSQL(t, s, ctx, `DELETE FROM run_servers WHERE run_id = 'r1'`)
				execSQL(t, s, ctx, `DELETE FROM placements WHERE id = 'p1'`)
				execSQL(t, s, ctx, `UPDATE runs SET state = 'submitted', current_epoch = 0, updated_at = now() - interval '1 hour' WHERE id = 'r1'`)
			} else {
				execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', ended_at = now() WHERE id = 'p1'`)
				execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming', updated_at = now() - interval '1 hour' WHERE id = 'r1'`)
			}
			execSQL(t, s, ctx, c.setup)
			if c.dropSecrets {
				s.secrets.drop("r1")
			}
			if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
				t.Fatal(err)
			}
			var state, reason string
			var resumable bool
			systemScan(t, s, `SELECT r.state, r.state_reason, `+resumableSQL+` FROM runs r WHERE r.id = 'r1'`, nil, &state, &reason, &resumable)
			if state != c.state {
				t.Errorf("state %q, want %q", state, c.state)
			}
			if reason != c.reason {
				t.Errorf("reason %q, want %q", reason, c.reason)
			}
			if want := c.state != StateTerminated; resumable != want {
				t.Errorf("resumable %v, want %v", resumable, want)
			}
			// A stopped or lost Run keeps them for a resume; an ended one
			// (or one whose values are gone already) holds none.
			want := (c.state == StateStopped || c.state == StateLost) && !c.dropSecrets
			if _, held := s.secrets.get("r1"); held != want {
				t.Errorf("secrets held %v, want %v", held, want)
			}
		})
	}
}

// A failed Run with no snapshot (its only report refused) is not
// terminated for that: it rests failed and resumable unless its policy is
// never, which terminates it as any of its ends; the refusal is still
// named, after the policy.
func TestRefusedSnapshotFailedRunRestsUnlessNever(t *testing.T) {
	for policy, want := range map[string]string{"": StateFailed, "never": StateTerminated} {
		t.Run(policy, func(t *testing.T) {
			s, ctx := policyFixture(t, policy)
			execSQL(t, s, ctx, `UPDATE placements SET snapshot_refused = true WHERE id = 'p1'`)
			reportR1Exit(t, s, 1, "exited")
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r1'`, nil, &state, &reason)
			wantReason := "exit code 1; " + refusedNoSnapshotReason
			if policy == "never" {
				wantReason = "exit code 1; resumePolicy never; " + refusedNoSnapshotReason
			}
			if state != want || reason != wantReason {
				t.Fatalf("state %q reason %q; want %q %q", state, reason, want, wantReason)
			}
		})
	}
}
