package gitws

import (
	"context"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/shim"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushCommit commits path=content on main in the remote, through a scratch
// clone; returns the new commit.
func pushCommit(t *testing.T, bare, path, content string) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w")
	gitRun(t, filepath.Dir(w), "clone", "-q", bare, w)
	os.WriteFile(filepath.Join(w, path), []byte(content), 0o644)
	gitRun(t, w, "add", "-A")
	gitRun(t, w, "commit", "-qm", "c")
	gitRun(t, w, "push", "-q", "origin", "HEAD:main")
	return gitRun(t, w, "rev-parse", "HEAD")
}

// The sync end to end, but for the container: the runner's bundle through
// the mirror, then lux-shim sync's rule on the checkout.
func TestSync(t *testing.T) {
	bare, base := remote(t)
	m := New(t.TempDir())
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repos", "r")
	r := Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}
	if _, err := m.Materialize(ctx, r, dir); err != nil {
		t.Fatal(err)
	}
	sync := func(ref string) proto.SyncResult {
		t.Helper()
		r.Ref = ref
		bundle := filepath.Join(t.TempDir(), "r.bundle")
		tg, err := m.SyncBundle(ctx, r, "", bundle)
		if err != nil {
			t.Fatal(err)
		}
		res := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: dir, Ref: ref, Commit: tg.Commit, Branch: tg.Branch, Bundle: bundle}}})
		return res[0]
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }

	// Nothing new: up to date.
	if res := sync("main"); res.Status != "up-to-date" || res.From != base || res.To != base {
		t.Fatalf("up to date: %+v", res)
	}
	// A tracked file changed, at the commit already: reset, the change
	// saved in refs/lux/pre-sync.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edit\n"), 0o644)
	if res := sync("main"); res.Status != "reset" || !res.Dirty || res.Diverged || res.From != base || res.To != base ||
		res.Saved != "refs/lux/pre-sync" {
		t.Fatalf("dirty at the commit: %+v", res)
	}
	if saved := gitRun(t, dir, "show", "refs/lux/pre-sync:a.txt"); saved != "edit" || read("a.txt") != "one\n" {
		t.Fatalf("dirty at the commit: saved %q, now %q", saved, read("a.txt"))
	}
	// A new commit, a clean checkout with untracked and ignored files:
	// fast-forward, the files kept.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("data/\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "data", "counter"), []byte("7"), 0o644)
	c2 := pushCommit(t, bare, "a.txt", "two\n")
	res := sync("main")
	if res.Status != "fast-forward" || res.From != base || res.To != c2 || res.Dirty || res.Diverged {
		t.Fatalf("fast-forward: %+v", res)
	}
	if read("a.txt") != "two\n" || read("data/counter") != "7" || read(".gitignore") != "data/\n" ||
		gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatalf("after fast-forward: %q %q", read("a.txt"), read("data/counter"))
	}
	// A tracked file changed: reset to the ref, the change saved in
	// refs/lux/pre-sync, untracked files kept.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local edit\n"), 0o644)
	c3 := pushCommit(t, bare, "b.txt", "bee\n")
	res = sync("main")
	if res.Status != "reset" || !res.Dirty || res.Diverged || res.To != c3 || res.Saved != "refs/lux/pre-sync" {
		t.Fatalf("dirty: %+v", res)
	}
	if read("a.txt") != "two\n" || read("b.txt") != "bee\n" || read("data/counter") != "7" {
		t.Fatalf("after reset: %q %q", read("a.txt"), read("b.txt"))
	}
	if saved := gitRun(t, dir, "show", "refs/lux/pre-sync:a.txt"); saved != "local edit" {
		t.Fatalf("saved: %q", saved)
	}
	// A local commit the remote does not have (diverged): reset; the
	// local commit is refs/lux/pre-sync.
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("mine\n"), 0o644)
	gitRun(t, dir, "add", "c.txt")
	gitRun(t, dir, "commit", "-qm", "local")
	local := gitRun(t, dir, "rev-parse", "HEAD")
	c4 := pushCommit(t, bare, "a.txt", "four\n")
	res = sync("main")
	if res.Status != "reset" || res.Dirty || !res.Diverged || res.From != local || res.To != c4 {
		t.Fatalf("diverged: %+v", res)
	}
	if gitRun(t, dir, "rev-parse", "refs/lux/pre-sync") != local || read("a.txt") != "four\n" {
		t.Fatalf("after diverged reset")
	}
	// A sha: detached at it.
	res = sync(c2)
	if res.Status != "reset" || res.To != c2 || gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatalf("to a sha: %+v", res)
	}
	// An unknown ref: the runner's bundle fails; nothing changes.
	r.Ref = "nope"
	if _, err := m.SyncBundle(ctx, r, "", filepath.Join(t.TempDir(), "x.bundle")); err == nil || !strings.Contains(err.Error(), `ref "nope" not found`) {
		t.Fatalf("unknown ref: %v", err)
	}
	// A bundle of another commit than announced, or no checkout: failed,
	// the checkout as it was.
	r.Ref = "main"
	bundle := filepath.Join(t.TempDir(), "r.bundle")
	if _, err := m.SyncBundle(ctx, r, "", bundle); err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	got := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{
		{Name: "r", Path: dir, Ref: "main", Commit: base, Branch: "main", Bundle: bundle},
		{Name: "gone", Path: filepath.Join(t.TempDir(), "none"), Ref: "main", Commit: base, Bundle: bundle},
	}})
	if got[0].Status != "failed" || !strings.Contains(got[0].Error, "the bundle has") || got[1].Status != "failed" ||
		!strings.Contains(got[1].Error, "has no checkout") || gitRun(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("failures: %+v", got)
	}
}

// dirSize is the bytes of the regular files under dir.
func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A checkout behind by a few commits gets a bundle of those commits only,
// with its base as the prerequisite; its .git grows by them, not by the
// repository. A checkout without the base fails the fetch as MissingBase,
// and the whole-history bundle then moves it.
func TestSyncBundleIsIncremental(t *testing.T) {
	bare, _ := remote(t)
	// History the checkout has: 2 MiB that does not compress.
	big := make([]byte, 2<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(rng.Uint32())
	}
	base := pushCommit(t, bare, "big.bin", string(big))
	m := New(t.TempDir())
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repos", "r")
	r := Repo{Tenant: "t", Name: "r", URL: bare, Ref: "main"}
	res, err := m.Materialize(ctx, r, dir)
	if err != nil || res.Base != base {
		t.Fatalf("materialize: %+v %v", res, err)
	}
	var head string
	for i := range 5 {
		head = pushCommit(t, bare, "n.txt", strings.Repeat("x", i+1)+"\n")
	}
	bundleDir := t.TempDir()
	fullBundle, incBundle := filepath.Join(bundleDir, "full.bundle"), filepath.Join(bundleDir, "inc.bundle")
	full, err := m.SyncBundle(ctx, r, "", fullBundle)
	if err != nil || full.Base != "" {
		t.Fatalf("full: %+v %v", full, err)
	}
	inc, err := m.SyncBundle(ctx, r, base, incBundle)
	if err != nil || inc.Base != base || inc.Commit != head {
		t.Fatalf("incremental: %+v %v", inc, err)
	}
	fullSize, incSize := dirSize(t, fullBundle), dirSize(t, incBundle)
	t.Logf("bundle bytes: full %d, incremental %d", fullSize, incSize)
	if fullSize < 2<<20 || incSize > 64<<10 {
		t.Fatalf("bundle sizes: full %d, incremental %d", fullSize, incSize)
	}
	if got := gitRun(t, bundleDir, "bundle", "list-heads", incBundle); got != head+" refs/lux/sync" {
		t.Fatalf("heads: %q", got)
	}
	// verify, in a repository without the base, names it as missing: the
	// bundle's one prerequisite.
	empty := filepath.Join(t.TempDir(), "e")
	gitRun(t, filepath.Dir(empty), "init", "-q", empty)
	cmd := exec.Command("git", "bundle", "verify", incBundle)
	cmd.Dir = empty
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "lacks these prerequisite commits") || !strings.Contains(string(out), base) {
		t.Fatalf("verify: %s", out)
	}

	before := dirSize(t, filepath.Join(dir, ".git"))
	got := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: dir, Ref: "main", Commit: inc.Commit,
		Branch: inc.Branch, Bundle: incBundle, Base: inc.Base}}})
	if got[0].Status != "fast-forward" || got[0].To != head {
		t.Fatalf("incremental sync: %+v", got[0])
	}
	grew := dirSize(t, filepath.Join(dir, ".git")) - before
	t.Logf(".git grew by %d bytes", grew)
	if grew > 256<<10 {
		t.Fatalf(".git grew by %d bytes", grew)
	}

	// Already at the commit: a bundle of the commit alone, still valid.
	sameBundle := filepath.Join(bundleDir, "same.bundle")
	same, err := m.SyncBundle(ctx, r, head, sameBundle)
	if err != nil || same.Base != head {
		t.Fatalf("same: %+v %v", same, err)
	}
	got = shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: dir, Ref: "main", Commit: same.Commit,
		Branch: same.Branch, Bundle: sameBundle, Base: same.Base}}})
	if got[0].Status != "up-to-date" {
		t.Fatalf("same commit: %+v", got[0])
	}

	// A base the mirror does not have: the whole history.
	unknownBundle := filepath.Join(bundleDir, "unknown.bundle")
	unknown, err := m.SyncBundle(ctx, r, strings.Repeat("ab", 20), unknownBundle)
	if err != nil || unknown.Base != "" || dirSize(t, unknownBundle) < 2<<20 {
		t.Fatalf("unknown base: %+v %v", unknown, err)
	}

	// A checkout that lacks the base (a clone of the older history only):
	// failed, MissingBase, HEAD unchanged; the full bundle then moves it.
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, filepath.Dir(other), "clone", "-q", "--no-local", bare, other)
	gitRun(t, other, "reset", "-q", "--hard", base+"~1")
	gitRun(t, other, "reflog", "expire", "--expire=now", "--all")
	gitRun(t, other, "update-ref", "-d", "refs/remotes/origin/main")
	gitRun(t, other, "gc", "-q", "--prune=now")
	old := gitRun(t, other, "rev-parse", "HEAD")
	got = shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: other, Ref: "main", Commit: inc.Commit,
		Branch: inc.Branch, Bundle: incBundle, Base: inc.Base}}})
	if got[0].Status != "failed" || !got[0].MissingBase || gitRun(t, other, "rev-parse", "HEAD") != old {
		t.Fatalf("missing base: %+v", got[0])
	}
	if names := proto.MissingBase(got); len(names) != 1 || names[0] != "r" {
		t.Fatalf("MissingBase: %v", names)
	}
	retry := shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: other, Ref: "main", Commit: full.Commit,
		Branch: full.Branch, Bundle: fullBundle}}})
	merged := proto.MergeSyncRetry(got, retry)
	if len(merged) != 1 || merged[0].Status != "fast-forward" || !merged[0].FullBundle || merged[0].To != head ||
		gitRun(t, other, "rev-parse", "HEAD") != head {
		t.Fatalf("full retry: %+v", merged)
	}
	// Any other fetch failure is not MissingBase: no retry.
	got = shim.SyncRepos(ctx, proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: dir, Ref: "main", Commit: head,
		Bundle: filepath.Join(bundleDir, "none.bundle"), Base: head}}})
	if got[0].Status != "failed" || got[0].MissingBase {
		t.Fatalf("no bundle: %+v", got[0])
	}
}
