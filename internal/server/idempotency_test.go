package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// submitWithKey submits a minimal Run as t1 with an idempotency key and
// the secret TOKEN=value.
func submitWithKey(ctx context.Context, s *Server, key, value string) (*submitRunOutput, error) {
	return s.submitRun(asTenant(ctx, "t1"), &submitRunInput{IdempotencyKey: key, Body: spec.RunSpec{
		Image:    spec.Image{Ref: "alpine"},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Secrets:  []spec.Secret{{Name: "TOKEN", Value: value}},
	}})
}

// cachedSecrets is every Run id luxd holds secret values for.
func cachedSecrets(s *Server) []string {
	s.secrets.mu.Lock()
	defer s.secrets.mu.Unlock()
	return slices.Collect(maps.Keys(s.secrets.m))
}

// checkOneRun fails unless every submit returned runID without a 5xx (or
// any error), exactly created of them with 201, and the tenant has
// exactly one Run with key.
func checkOneRun(t *testing.T, s *Server, key, runID string, created int, outs []*submitRunOutput, errs []error) {
	t.Helper()
	got := 0
	for i, err := range errs {
		if err != nil {
			var he *HTTPError
			if errors.As(err, &he) && he.Status >= 500 {
				t.Errorf("submit %d: %d %v", i, he.Status, err)
			} else {
				t.Errorf("submit %d: %v", i, err)
			}
			continue
		}
		if outs[i].Body.ID != runID {
			t.Errorf("submit %d: Run %s, want %s", i, outs[i].Body.ID, runID)
		}
		switch outs[i].Status {
		case http.StatusCreated:
			got++
		case http.StatusOK:
		default:
			t.Errorf("submit %d: status %d", i, outs[i].Status)
		}
	}
	if got != created {
		t.Errorf("%d submits created a Run, want %d", got, created)
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM runs WHERE tenant_id = 't1' AND idempotency_key = $1`, []any{key}, &n)
	if n != 1 {
		t.Errorf("%d Runs with key %s, want 1", n, key)
	}
}

// Submits with one key that all miss the replay SELECT, because the first
// Run's INSERT has not committed, wait on its unique index and then
// return that Run (200), not the unique violation. The first Run keeps
// its secret values; no loser's are cached under any id.
func TestConcurrentIdempotentSubmitsBlockedOnInsert(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	n := min(8, int(s.db.Pool.Config().MaxConns)-2)

	// The first Run, inserted and not yet committed.
	holder, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state, idempotency_key)
		VALUES ('run_first', 't1', '{"image": {"ref": "alpine"}, "workload": {"adapter": "generic", "command": ["true"]}}', 'submitted', 'k')`); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	// What the first Run's own submit caches before it commits.
	s.secrets.put("run_first", map[string]string{"TOKEN": "first"})

	outs := make([]*submitRunOutput, n)
	errs := make([]error, n)
	done := make(chan error, n)
	for i := range n {
		go func() {
			outs[i], errs[i] = submitWithKey(ctx, s, "k", fmt.Sprintf("loser-%d", i))
			done <- nil
		}()
	}
	// Every submit is past its SELECT and waits on the holder's row.
	for {
		var blocked int
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked)
		}); err != nil {
			t.Fatal(err)
		}
		if blocked == n {
			break
		}
		select {
		case <-done:
			t.Fatalf("a submit finished before the first Run committed")
		case <-ctx.Done():
			t.Fatalf("%d of %d submits blocked at the deadline", blocked, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, done, n)
	checkOneRun(t, s, "k", "run_first", 0, outs, errs)
	for i, out := range outs {
		if errs[i] == nil && out.Status != http.StatusOK {
			t.Errorf("submit %d: status %d, want 200 for the first Run", i, out.Status)
		}
	}
	if v, ok := s.secrets.get("run_first"); !ok || v["TOKEN"] != "first" {
		t.Errorf("the first Run's secrets: %v %v, want TOKEN=first", v, ok)
	}
	if ids := cachedSecrets(s); !slices.Equal(ids, []string{"run_first"}) {
		t.Errorf("secrets cached for %v, want only run_first", ids)
	}
}

// N submits with one key at once, none held. Whether or not any of them
// races the INSERT, exactly one creates the Run (201) and every other
// returns it (200).
func TestConcurrentIdempotentSubmits(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	n := min(8, int(s.db.Pool.Config().MaxConns)-1)
	outs := make([]*submitRunOutput, n)
	errs := make([]error, n)
	start := make(chan struct{})
	done := make(chan error, n)
	for i := range n {
		go func() {
			<-start
			outs[i], errs[i] = submitWithKey(ctx, s, "same", "v")
			done <- nil
		}()
	}
	close(start)
	await(t, ctx, done, n)
	var id string
	systemScan(t, s, `SELECT id FROM runs WHERE idempotency_key = 'same'`, nil, &id)
	checkOneRun(t, s, "same", id, 1, outs, errs)
}

// Only a unique violation of the idempotency constraint is a replay: not
// another constraint's, nor another error on it.
func TestIsUniqueViolation(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("tx: %w", &pgconn.PgError{Code: "23505", ConstraintName: runsIdempotencyKey}), true},
		{&pgconn.PgError{Code: "23505", ConstraintName: "runs_pkey"}, false},
		{&pgconn.PgError{Code: "23503", ConstraintName: runsIdempotencyKey}, false},
		{errors.New("23505 " + runsIdempotencyKey), false},
		{nil, false},
	} {
		if got := isUniqueViolation(c.err, runsIdempotencyKey); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

// The constraint submitRun recognises exists on runs under that name.
func TestIdempotencyConstraintExists(t *testing.T) {
	s := testServer(t)
	var n int
	systemScan(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'runs'::regclass AND contype = 'u'`,
		[]any{runsIdempotencyKey}, &n)
	if n != 1 {
		t.Fatalf("%d unique constraints %s on runs, want 1", n, runsIdempotencyKey)
	}
}
