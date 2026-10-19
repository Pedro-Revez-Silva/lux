package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/spec"
)

// stoppedWithRepo is a stopped t1 Run (stoppedWith) with repository api at
// url, and an uploaded checkpoint of it, checkpointID.
func stoppedWithRepo(t *testing.T, s *Server, url, checkpointID string) string {
	t.Helper()
	sp := noSecretsSpec()
	sp.Git = &spec.Git{Repositories: []spec.Repository{{Name: "api", URL: url}}}
	id := stoppedWith(t, s, sp)
	uploadedCheckpointOf(t, s, "t1", id, checkpointID)
	return id
}

// uploadedCheckpointOf records checkpointID of run's repo api as uploaded
// to S3 and latest.
func uploadedCheckpointOf(t *testing.T, s *Server, tenant, run, checkpointID string) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key)
		VALUES ($1, $2, $3, 1, 'checkpoint', 'api', 's3', $3 || '/' || $1)`, checkpointID, tenant, run)
	execSQL(t, s, ctx, `INSERT INTO git_checkpoints (id, tenant_id, run_id, repo, head, branch, refs, size, epoch, uploaded, latest)
		VALUES ($1, $2, $3, 'api', repeat('a', 40), 'main', '{}', 1, 1, true, true)`, checkpointID, tenant, run)
}

func resumeAddingFrom(ctx context.Context, s *Server, id, name, source string) (*resumeOutput, error) {
	return resumeSecrets(ctx, s, id, resumeRequest{RequestID: "add-" + name,
		Git: &resumeGit{Repositories: []spec.Repository{{Name: name, From: &spec.RepositoryFrom{Run: source, Repo: "api"}}}}})
}

// waitLockWaiters returns once n backends of this database wait on a lock.
func waitLockWaiters(t *testing.T, ctx context.Context, s *Server, n int) {
	t.Helper()
	for {
		var got int
		systemScan(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`, nil, &got)
		if got >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d of %d backends waiting on a lock at the deadline", got, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Two resumes that name each other as a `from` source (A adds B's api, B
// adds A's) both wait on a transaction holding A and B, so both reach the
// lock step before either has a Run. Released together, both succeed: each
// pins the other's checkpoint. Neither deadlocks (40P01) or hangs.
func TestResumeReciprocalFromSources(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	a := stoppedWithRepo(t, s, "https://git.example.com/a.git", "checkpoint-a")
	b := stoppedWithRepo(t, s, "https://git.example.com/b.git", "checkpoint-b")

	holder := hold(t, ctx, s, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) FOR UPDATE`, []string{a, b})
		return err
	})
	if !holder.settle(t, ctx, s) {
		t.Fatal("the holder waited on a lock")
	}
	type result struct {
		out *resumeOutput
		err error
	}
	resumeA, resumeB := make(chan result, 1), make(chan result, 1)
	go func() {
		out, err := resumeAddingFrom(asTenant(ctx, "t1"), s, a, "copy-b", b)
		resumeA <- result{out, err}
	}()
	go func() {
		out, err := resumeAddingFrom(asTenant(ctx, "t1"), s, b, "copy-a", a)
		resumeB <- result{out, err}
	}()
	waitLockWaiters(t, ctx, s, 2)
	if err := holder.finishWith(t, ctx, nil); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		dst, repo, source, url, pin string
		got                         <-chan result
	}{
		{a, "copy-b", b, "https://git.example.com/b.git", "checkpoint-b", resumeA},
		{b, "copy-a", a, "https://git.example.com/a.git", "checkpoint-a", resumeB},
	} {
		var r result
		select {
		case r = <-c.got:
		case <-ctx.Done():
			t.Fatalf("resume of %s from %s still running at the deadline", c.dst, c.source)
		}
		var pe *pgconn.PgError
		if errors.As(r.err, &pe) {
			t.Fatalf("resume of %s from %s: SQLSTATE %s: %v", c.dst, c.source, pe.Code, r.err)
		}
		if r.err != nil {
			t.Fatalf("resume of %s from %s: %v", c.dst, c.source, r.err)
		}
		if r.out.Status != http.StatusAccepted || r.out.Body.Run.State != StateResuming {
			t.Fatalf("resume of %s: %d, state %s", c.dst, r.out.Status, r.out.Body.Run.State)
		}
		if pin := pinOf(t, s, c.dst, c.repo); pin != c.pin {
			t.Fatalf("%s pins %q for %s, want %s", c.dst, pin, c.repo, c.pin)
		}
		var stored spec.RunSpec
		systemScan(t, s, `SELECT spec FROM runs WHERE id = $1`, []any{c.dst}, &stored)
		if stored.Git == nil {
			t.Fatalf("%s has no git repositories", c.dst)
		}
		var added *spec.Repository
		for i, repo := range stored.Git.Repositories {
			if repo.Name == c.repo {
				added = &stored.Git.Repositories[i]
			}
		}
		if added == nil || added.From == nil || added.From.Checkpoint != c.pin || added.From.Run != c.source || added.URL != c.url {
			t.Fatalf("%s's stored repositories: %+v", c.dst, stored.Git.Repositories)
		}
	}
}

// A resume naming another tenant's Run as its source is refused as if the
// Run did not exist, and does not wait for that Run's lock: the lock step
// does not see it.
func TestResumeFromOtherTenantIsMissing(t *testing.T) {
	s := testServer(t)
	ctx := testDeadline(t)
	a := stoppedWithRepo(t, s, "https://git.example.com/a.git", "checkpoint-a")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t3', 't3')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch)
		VALUES ('rc', 't3', '{"git":{"repositories":[{"name":"api","url":"https://git.example.com/c.git"}]}}', 'stopped', 1)`)
	uploadedCheckpointOf(t, s, "t3", "rc", "checkpoint-c")

	holder := hold(t, ctx, s, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'rc' FOR UPDATE`)
		return err
	})
	if !holder.settle(t, ctx, s) {
		t.Fatal("the holder waited on a lock")
	}
	refusal := func(source string) *HTTPError {
		t.Helper()
		_, err := resumeAddingFrom(asTenant(ctx, "t1"), s, a, "copy", source)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" {
			t.Fatalf("resume from %s: %v, want 422 invalid_spec", source, err)
		}
		return he
	}
	cross, missing := refusal("rc"), refusal("rz")
	if cross.Message != "source Run rc does not exist" || missing.Message != "source Run rz does not exist" {
		t.Fatalf("refusals: %q / %q", cross.Message, missing.Message)
	}
	if err := holder.finishWith(t, ctx, nil); err != nil {
		t.Fatal(err)
	}
	if pin := pinOf(t, s, a, "copy"); pin != "" {
		t.Fatalf("refused resume pinned %q", pin)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{a}, &state)
	if state != StateStopped {
		t.Fatalf("refused resume left the Run %s", state)
	}
}
