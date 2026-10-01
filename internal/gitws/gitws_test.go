package gitws

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A local "remote" (a bare repo on disk): exercises materialize and push
// without a network or credentials.
func remote(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "remote.git")
	os.MkdirAll(work, 0o755)
	gitRun(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", "one")
	gitRun(t, root, "clone", "-q", "--bare", work, bare)
	return bare, gitRun(t, work, "rev-parse", "HEAD")
}

func TestMaterializeIsIdempotentAndKeepsWork(t *testing.T) {
	bare, base := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "repos", "r")
	r := Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}
	res, err := m.Materialize(context.Background(), r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != base || res.Branch != "main" || !res.Cloned {
		t.Fatalf("got %+v", res)
	}
	// Work in the checkout survives a second Materialize (a resume).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\n"), 0o644)
	if res, err := m.Materialize(context.Background(), r, dir); err != nil || res.Cloned {
		t.Fatalf("second materialize: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "mine\n" {
		t.Fatalf("work lost: %q", b)
	}
	// The clone stands alone: no alternates pointing at the host's mirror.
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("the checkout depends on the mirror")
	}
}

func TestUnknownRef(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	_, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "nope"}, filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Fatalf("got %v", err)
	}
}

func TestScrub(t *testing.T) {
	if got := Scrub("https://user:tok@example.com/a.git"); got != "https://example.com/a.git" {
		t.Fatal(got)
	}
}

// A push reads only the bundle the workload made: a hostile checkout's
// hooks and config (which could run code or redirect the push) are never
// consulted, because the runner never runs git in it.
func TestPushIgnoresTheCheckoutsGitConfig(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(dir, ".git", "hooks")
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	cfg, _ := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	cfg.WriteString("[url \"file:///nonexistent/\"]\n\tinsteadOf = " + bare + "\n")
	cfg.Close()

	bundle := filepath.Join(t.TempDir(), "b.bundle")
	gitIn(t, dir, "bundle", "create", bundle, "HEAD")
	res := m.Push(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare}, bundle, "lux/x", "")
	if res.Status != "pushed" {
		t.Fatalf("push: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checkout's pre-push hook ran")
	}
	// Leased: a new commit pushed with a stale lease (the branch is not
	// where this Run left it) is rejected, and the branch keeps its commit.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qam", "two")
	bundle2 := filepath.Join(t.TempDir(), "b2.bundle")
	gitIn(t, dir, "bundle", "create", bundle2, "HEAD")
	if res := m.Push(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare}, bundle2, "lux/x", ""); res.Status != "rejected" {
		t.Fatalf("stale lease: %+v", res)
	}
	res2 := m.Push(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare}, bundle2, "lux/x", res.Commit)
	if res2.Status != "pushed" {
		t.Fatalf("correct lease: %+v", res2)
	}
	// Again, nothing new: the remote says so.
	if res := m.Push(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare}, bundle2, "lux/x", res2.Commit); res.Status != "up-to-date" {
		t.Fatalf("nothing new: %+v", res)
	}
	// The lease is the commit being pushed but the branch is elsewhere (a
	// caller's wrong expectation): the remote is asked, and refuses.
	if res := m.Push(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare}, bundle, "lux/x", res.Commit); res.Status != "rejected" {
		t.Fatalf("lease equal to head, branch elsewhere: %+v", res)
	}
	if got := gitOut(t, bare, "rev-parse", "refs/heads/lux/x"); got != res2.Commit {
		t.Fatalf("branch moved to %s", got)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// A clone that fails part way (here: an unknown ref) leaves nothing a
// later start would take for a checkout.
func TestFailedCloneLeavesNothing(t *testing.T) {
	bare, _ := remote(t)
	m := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "nope"}, dir); err == nil {
		t.Fatal("expected an error")
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if res, err := m.Materialize(context.Background(), Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}, dir); err != nil || !res.Cloned {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

// A mirror is one tenant's: another tenant naming the same URL, even at a
// commit the first one's mirror holds, never clones from it (and so never
// reads a private repository through it without its own credential).
func TestMirrorsAreNotSharedBetweenTenants(t *testing.T) {
	bare, sha := remote(t)
	m := New(t.TempDir())
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "a", Name: "r", URL: bare, Ref: "main"}, filepath.Join(t.TempDir(), "r")); err != nil {
		t.Fatal(err)
	}
	// B, for the same URL now unreachable (a private repo it has no access
	// to), must not be served A's mirror.
	os.RemoveAll(bare)
	if _, err := m.Materialize(context.Background(), Repo{Tenant: "b", Name: "r", URL: bare, Ref: sha}, filepath.Join(t.TempDir(), "r")); err == nil {
		t.Fatal("tenant b cloned from tenant a's mirror")
	}
}
