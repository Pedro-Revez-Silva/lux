package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// expiryFixture: tenants t1 (default 90 days), never (0) and short (10),
// and no Runs.
func expiryFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name, expire_after_days) VALUES ('never', 'never', 0), ('short', 'short', 10)`)
	return s, ctx
}

// restingRun adds a Run in state that entered it days ago (an ended one
// finished 200 days ago).
func restingRun(t *testing.T, s *Server, ctx context.Context, id, tenant, state string, days float64) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, state_changed_at, finished_at)
		VALUES ($1, $2, '{}', $3, 1, now() - make_interval(secs => $4 * 86400), CASE WHEN $3 IN ('failed', 'succeeded') THEN now() - interval '200 days' END)`,
		id, tenant, state, days)
}

func runState(t *testing.T, s *Server, id string) (state, reason string) {
	t.Helper()
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = $1`, []any{id}, &state, &reason)
	return state, reason
}

// A Run resting (stopped, lost, failed or succeeded) longer than its
// tenant's limit is terminated, saying why, with a state event and
// terminated_at now (retention's clock); an ended one keeps its
// finished_at, a stopped or lost one gets it now. One just under the
// limit, a live one, and one of a tenant whose Runs never expire are not.
func TestReapExpiry(t *testing.T) {
	s, ctx := expiryFixture(t)
	restingRun(t, s, ctx, "stopped-old", "t1", StateStopped, 90.01)
	restingRun(t, s, ctx, "lost-old", "t1", StateLost, 91)
	restingRun(t, s, ctx, "failed-old", "t1", StateFailed, 120)
	restingRun(t, s, ctx, "succeeded-old", "t1", StateSucceeded, 95)
	restingRun(t, s, ctx, "stopped-young", "t1", StateStopped, 89.99)
	restingRun(t, s, ctx, "succeeded-young", "t1", StateSucceeded, 89.99)
	restingRun(t, s, ctx, "running-old", "t1", StateRunning, 200)
	restingRun(t, s, ctx, "never-old", "never", StateStopped, 1000)
	restingRun(t, s, ctx, "short-old", "short", StateLost, 10.01)
	restingRun(t, s, ctx, "short-young", "short", StateStopped, 9.99)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"stopped-old":   "expired: stopped for 90 days",
		"lost-old":      "expired: lost for 90 days",
		"failed-old":    "expired: failed for 90 days",
		"succeeded-old": "expired: succeeded for 90 days",
		"short-old":     "expired: lost for 10 days",
	} {
		state, reason := runState(t, s, id)
		if state != StateTerminated || reason != want {
			t.Errorf("%s: %s %q, want terminated %q", id, state, reason, want)
		}
		var events int
		var finishedNow, terminatedNow bool
		systemScan(t, s, `SELECT (SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'state'
				AND data->>'state' = 'terminated' AND data->>'reason' = $2),
			finished_at > now() - interval '1 minute', terminated_at > now() - interval '1 minute' FROM runs WHERE id = $1`,
			[]any{id, want}, &events, &finishedNow, &terminatedNow)
		wantFinishedNow := id != "failed-old" && id != "succeeded-old"
		if events != 1 || finishedNow != wantFinishedNow || !terminatedNow {
			t.Errorf("%s: %d terminated state events, finished now %v (want %v), terminated now %v", id, events, finishedNow, wantFinishedNow, terminatedNow)
		}
	}
	for id, want := range map[string]string{"stopped-young": StateStopped, "succeeded-young": StateSucceeded, "running-old": StateRunning,
		"never-old": StateStopped, "short-young": StateStopped} {
		if state, _ := runState(t, s, id); state != want {
			t.Errorf("%s: %s, want %s", id, state, want)
		}
	}
}

// A resume holding the Run when the reaper looks: the Run is skipped and,
// once the resume commits, no longer resting, so it is never terminated.
// The other way round, a resume after the expiry is refused.
func TestReapExpiryRacesResume(t *testing.T) {
	s, ctx := expiryFixture(t)
	restingRun(t, s, ctx, "r", "t1", StateStopped, 100)
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'r' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			<-release
			return s.requestResume(ctx, tx, "t1", "r", nil, "resume requested")
		})
	}()
	<-locked
	// The reaper may skip the Run or wait for it; either way the resume
	// commits first.
	reaped := make(chan error, 1)
	go func() { reaped <- s.reapExpiry(ctx) }()
	select {
	case err := <-reaped:
		reaped <- err
	case <-time.After(time.Second):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-reaped; err != nil {
		t.Fatal(err)
	}
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := runState(t, s, "r"); state != StateResuming {
		t.Fatalf("resumed while the reaper looked: %s %q, want resuming", state, reason)
	}

	restingRun(t, s, ctx, "late", "t1", StateStopped, 100)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := s.resumeRun(asTenant(ctx, "t1"), &resumeRunInput{RunPath: RunPath{ID: "late"}})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("resume of an expired Run: %v, want 409", err)
	}
}

// GET /v1/tenants shows each tenant's expireAfterDays.
func TestTenantsShowExpiry(t *testing.T) {
	s, _ := expiryFixture(t)
	op := apiKey(t, s, nil, "operator")
	code, body := call(t, s, op, http.MethodGet, "/v1/tenants", nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var resp struct{ Tenants []Tenant }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, tn := range resp.Tenants {
		got[tn.ID] = tn.ExpireAfterDays
	}
	if got["t1"] != 90 || got["never"] != 0 || got["short"] != 10 || len(got) != 3 {
		t.Fatalf("expireAfterDays %v", got)
	}
}

// The clock is the time in the resting state: a Run stopped long ago, then
// resumed and stopped again, has rested only since the second stop. A
// write that is not a state change (updated_at) does not restart it.
func TestReapExpiryClockRestartsOnResume(t *testing.T) {
	s, ctx := expiryFixture(t)
	restingRun(t, s, ctx, "r", "t1", StateStopped, 100)
	restingRun(t, s, ctx, "touched", "t1", StateStopped, 100)
	execSQL(t, s, ctx, `UPDATE runs SET updated_at = now() WHERE id = 'touched'`)
	// Running for 100 days, stopped now: it has rested no time.
	restingRun(t, s, ctx, "ran-long", "t1", StateRunning, 100)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := s.requestResume(ctx, tx, "t1", "r", nil, "resume requested"); err != nil {
			return err
		}
		if err := setRunState(ctx, tx, "t1", "r", StateStopped, "stop", 1); err != nil {
			return err
		}
		return setRunState(ctx, tx, "t1", "ran-long", StateStopped, "stop", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := runState(t, s, "r"); state != StateStopped {
		t.Fatalf("resumed and stopped again: %s %q, want stopped", state, reason)
	}
	if state, _ := runState(t, s, "ran-long"); state != StateStopped {
		t.Fatalf("ran-long: %s, want stopped (the clock is the stop, not the start)", state)
	}
	if state, _ := runState(t, s, "touched"); state != StateTerminated {
		t.Fatalf("touched: %s, want terminated (updated_at is not the clock)", state)
	}
}

// Setting a Run's state to the one it is already in (a repeated stop) is
// not a change of state: the clock keeps running and the Run expires.
func TestReapExpiryClockKeptOnSameState(t *testing.T) {
	s, ctx := expiryFixture(t)
	restingRun(t, s, ctx, "repeated", "t1", StateStopped, 100)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return setRunState(ctx, tx, "t1", "repeated", StateStopped, "stop", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if state, reason := runState(t, s, "repeated"); state != StateTerminated {
		t.Fatalf("repeated stop restarted expiry: %s %q, want terminated", state, reason)
	}
}

// A pass terminates the oldest 20 due Runs across tenants: of t1's 21 (rested
// 100..120 days) and short's one (110.5 days), t1's two youngest wait for
// the next pass.
func TestReapExpiryGlobalBatch(t *testing.T) {
	s, ctx := expiryFixture(t)
	for i := 0; i < 21; i++ {
		restingRun(t, s, ctx, fmt.Sprintf("r%02d", i), "t1", StateStopped, float64(100+i))
	}
	restingRun(t, s, ctx, "short-due", "short", StateStopped, 110.5)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("r%02d", i)
		want := StateTerminated
		if i < 2 {
			want = StateStopped
		}
		if got, _ := runState(t, s, id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	if got, _ := runState(t, s, "short-due"); got != StateTerminated {
		t.Fatalf("short-due: %s, want terminated", got)
	}
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r00", "r01"} {
		if got, _ := runState(t, s, id); got != StateTerminated {
			t.Errorf("next pass %s: %s, want terminated", id, got)
		}
	}
}

// The oldest due Run held by another transaction (a resume or terminate in
// progress, here one that only holds the row) keeps its batch slot: the
// pass does not wait for it and terminates the other 19 of the oldest 20,
// leaving it and the youngest. Once released, the next pass terminates both.
func TestReapExpiryLockedOldest(t *testing.T) {
	s, ctx := expiryFixture(t)
	for i := 0; i < 21; i++ {
		restingRun(t, s, ctx, fmt.Sprintf("r%02d", i), "t1", StateStopped, float64(100+i))
	}
	held := hold(t, ctx, s, lockRow("runs", "r20", "FOR UPDATE"))
	if !held.settle(t, ctx, s) {
		t.Fatal("the holder is blocked")
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.reapExpiry(bounded); err != nil {
		t.Fatalf("pass with the oldest held: %v", err)
	}
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("r%02d", i)
		want := StateTerminated
		if i == 0 || i == 20 {
			want = StateStopped
		}
		if got, _ := runState(t, s, id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	release(t, held, nil)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r00", "r20"} {
		if got, _ := runState(t, s, id); got != StateTerminated {
			t.Errorf("next pass %s: %s, want terminated", id, got)
		}
	}
}
