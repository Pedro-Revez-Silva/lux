package shim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// modeRepo is an upstream (a working repository standing in for the
// host's mirror) and a checkout cloned from it, on branch main.
type modeRepo struct {
	t        *testing.T
	upstream string
	dir      string
}

func newModeRepo(t *testing.T) *modeRepo {
	t.Helper()
	up := filepath.Join(t.TempDir(), "up")
	gitT(t, filepath.Dir(up), "init", "-q", "-b", "main", up)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("one\n"), 0o644)
	gitT(t, up, "add", "-A")
	gitT(t, up, "commit", "-qm", "one")
	dir := filepath.Join(t.TempDir(), "co")
	gitT(t, filepath.Dir(dir), "clone", "-q", up, dir)
	return &modeRepo{t: t, upstream: up, dir: dir}
}

// push commits path=content upstream on main; returns the commit.
func (m *modeRepo) push(path, content string) string {
	m.t.Helper()
	os.WriteFile(filepath.Join(m.upstream, path), []byte(content), 0o644)
	gitT(m.t, m.upstream, "add", "-A")
	gitT(m.t, m.upstream, "commit", "-qm", "up "+path)
	return gitT(m.t, m.upstream, "rev-parse", "HEAD")
}

// commitLocal commits path=content in the checkout; returns the commit.
func (m *modeRepo) commitLocal(path, content string) string {
	m.t.Helper()
	m.write(path, content)
	gitT(m.t, m.dir, "add", path)
	gitT(m.t, m.dir, "commit", "-qm", "local "+path)
	return m.head()
}

func (m *modeRepo) write(path, content string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.dir, path), []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *modeRepo) read(path string) string {
	b, err := os.ReadFile(filepath.Join(m.dir, path))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (m *modeRepo) head() string { return gitT(m.t, m.dir, "rev-parse", "HEAD") }

// ref is a ref's commit in the checkout, "" when it does not exist.
func (m *modeRepo) ref(name string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", name)
	cmd.Dir = m.dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// sync bundles upstream's main as refs/lux/sync (the runner's part) and
// runs the shim's sync on the checkout in mode.
func (m *modeRepo) sync(mode string) proto.SyncResult { return m.syncAs(mode, "main") }

// syncAs is sync with the target's Branch: "" for a tag or sha.
func (m *modeRepo) syncAs(mode, branch string) proto.SyncResult {
	m.t.Helper()
	commit := gitT(m.t, m.upstream, "rev-parse", "main")
	gitT(m.t, m.upstream, "update-ref", "refs/lux/sync", commit)
	bundle := filepath.Join(m.t.TempDir(), "r.bundle")
	gitT(m.t, m.upstream, "bundle", "create", "-q", bundle, "refs/lux/sync")
	ref := commit
	if branch != "" {
		ref = branch
	}
	got := SyncRepos(context.Background(), proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: m.dir, Ref: ref, Mode: mode,
		Commit: commit, Branch: branch, Bundle: bundle}}})
	if len(got) != 1 {
		m.t.Fatalf("results: %+v", got)
	}
	return got[0]
}

// tree is every file of the working tree but .git, with its bytes.
func (m *modeRepo) tree() map[string]string {
	m.t.Helper()
	out := map[string]string{}
	err := filepath.Walk(m.dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() && fi.Name() == ".git" {
			return filepath.SkipDir
		}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			rel, _ := filepath.Rel(m.dir, p)
			out[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return out
}

func (m *modeRepo) index() string { return gitT(m.t, m.dir, "ls-files", "-s") }

func counts(r proto.SyncResult) (int, int) {
	if r.Ahead == nil || r.Behind == nil {
		return -1, -1
	}
	return *r.Ahead, *r.Behind
}

// snapshot is what a sync that does not move must leave as it was: HEAD,
// its branch, the index, refs/lux/pre-sync and the working tree's bytes.
type snapshot struct {
	head, branch, index, preSync string
	tree                         map[string]string
}

func (m *modeRepo) snap() snapshot {
	// "HEAD" when detached.
	branch := gitT(m.t, m.dir, "rev-parse", "--symbolic-full-name", "HEAD")
	return snapshot{m.head(), branch, m.index(), m.ref("refs/lux/pre-sync"), m.tree()}
}

func (m *modeRepo) unchanged(before snapshot) {
	m.t.Helper()
	after := m.snap()
	if after.head != before.head || after.branch != before.branch || after.index != before.index || after.preSync != before.preSync {
		m.t.Fatalf("moved: before %+v\nafter %+v", before, after)
	}
	if len(after.tree) != len(before.tree) {
		m.t.Fatalf("tree: before %v, after %v", before.tree, after.tree)
	}
	for p, b := range before.tree {
		if after.tree[p] != b {
			m.t.Fatalf("%s: %q, was %q", p, after.tree[p], b)
		}
	}
}

func TestSyncFastForward(t *testing.T) {
	t.Run("clean ancestor: moves", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		m.write("untracked.txt", "mine\n")
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "fast-forward" || res.Mode != proto.SyncFastForward || res.From != base || res.To != c2 ||
			res.Dirty || res.Diverged || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		if m.head() != c2 || m.ref("refs/heads/main") != c2 || m.read("a.txt") != "two\n" || m.read("untracked.txt") != "mine\n" {
			t.Fatalf("after: head %s, a.txt %q", m.head(), m.read("a.txt"))
		}
		if m.ref("refs/remotes/lux/main") != c2 || m.ref("refs/lux/sync") != c2 || m.ref("refs/lux/pre-sync") != "" {
			t.Fatalf("refs: lux/main %q, sync %q, pre-sync %q", m.ref("refs/remotes/lux/main"), m.ref("refs/lux/sync"), m.ref("refs/lux/pre-sync"))
		}
	})
	t.Run("dirty ancestor: kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		m.write("untracked.txt", "mine\n")
		m.write("staged.txt", "staged\n")
		gitT(t, m.dir, "add", "staged.txt")
		before := m.snap()
		c2 := m.push("b.txt", "bee\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Dirty || res.Diverged || res.To != c2 || res.Saved != "" || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.read("a.txt") != "local edit\n" || m.read("untracked.txt") != "mine\n" || m.read("staged.txt") != "staged\n" {
			t.Fatal("files changed")
		}
		if m.ref("refs/remotes/lux/main") != c2 || m.ref("refs/lux/sync") != c2 {
			t.Fatalf("refs/remotes/lux/main %q, want %s", m.ref("refs/remotes/lux/main"), c2)
		}
	})
	t.Run("dirty at the commit: kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Dirty || a != 0 || b != 0 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
	})
	t.Run("local commits: ahead", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		local := m.commitLocal("c.txt", "mine\n")
		m.commitLocal("d.txt", "mine too\n")
		mine := m.head()
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "ahead" || res.Dirty || res.Diverged || res.To != base || a != 2 || b != 0 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.head() != mine || gitT(t, m.dir, "rev-parse", mine+"~1") != local || m.ref("refs/remotes/lux/main") != base {
			t.Fatal("history changed")
		}
	})
	t.Run("diverged: kept", func(t *testing.T) {
		m := newModeRepo(t)
		local := m.commitLocal("c.txt", "mine\n")
		m.write("untracked.txt", "u\n")
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || !res.Diverged || res.Dirty || res.From != local || res.To != c2 || a != 1 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.ref("refs/remotes/lux/main") != c2 {
			t.Fatal("refs/remotes/lux/main not set")
		}
		// The workload can merge it itself.
		gitT(t, m.dir, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-edit", "lux/main")
		if m.read("a.txt") != "two\n" || m.read("c.txt") != "mine\n" {
			t.Fatal("merge of lux/main")
		}
	})
	t.Run("up to date", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		before := m.snap()
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "up-to-date" || res.To != base || a != 0 || b != 0 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/remotes/lux/main") != base {
			t.Fatal("refs/remotes/lux/main not set")
		}
	})
	t.Run("ignored file the commit adds: failed, its bytes kept", func(t *testing.T) {
		m := newModeRepo(t)
		m.write(".git/info/exclude", "ignored.db\n")
		m.write("ignored.db", "LOCAL WORK\n")
		before := m.snap()
		c2 := m.push("ignored.db", "UPSTREAM\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "failed" && res.Status != "kept" {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.read("ignored.db") != "LOCAL WORK\n" || m.ref("refs/heads/main") != before.head || m.ref("refs/remotes/lux/main") != c2 {
			t.Fatalf("ignored.db %q, main %s", m.read("ignored.db"), m.ref("refs/heads/main"))
		}
	})
	t.Run("untracked file the commit adds: failed, with its counts", func(t *testing.T) {
		m := newModeRepo(t)
		m.write("incoming.txt", "mine\n")
		before := m.snap()
		tip := m.push("incoming.txt", "theirs\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "failed" || res.Saved != "" || res.Dirty || a != 0 || b != 1 ||
			!strings.Contains(res.Error, "incoming.txt") {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.ref("refs/remotes/lux/main") != tip {
			t.Fatal("refs/remotes/lux/main not set")
		}
	})
	t.Run("HEAD on another branch: main's own commits kept", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		m.commitLocal("c.txt", "on main\n")
		mainTip := m.head()
		gitT(t, m.dir, "checkout", "-q", "-b", "side", base)
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "kept" || res.To != c2 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/heads/main") != mainTip {
			t.Fatal("main moved")
		}
	})
	// Only the branch HEAD is on moves: switching HEAD between refs has no
	// guard against the workload switching or committing meanwhile.
	t.Run("HEAD on another branch, the target's branch a clean ancestor: kept", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		gitT(t, m.dir, "checkout", "-q", "-b", "side")
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || res.To != c2 || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if m.ref("refs/heads/main") != base || m.ref("refs/heads/side") != base || m.ref("refs/remotes/lux/main") != c2 {
			t.Fatalf("main %s side %s, was %s", m.ref("refs/heads/main"), m.ref("refs/heads/side"), base)
		}
	})
	t.Run("HEAD on another branch, the target's branch absent: kept, not created", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		gitT(t, m.dir, "checkout", "-q", "-b", "side")
		gitT(t, m.dir, "branch", "-q", "-D", "main")
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "kept" || res.To != c2 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/heads/main") != "" || m.ref("refs/heads/side") != base {
			t.Fatalf("main %q side %s", m.ref("refs/heads/main"), m.ref("refs/heads/side"))
		}
	})
	t.Run("HEAD detached, a branch target: kept", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		gitT(t, m.dir, "checkout", "-q", "--detach")
		before := m.snap()
		c2 := m.push("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if res.Status != "kept" || res.To != c2 {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
		if m.ref("refs/heads/main") != base {
			t.Fatal("main moved")
		}
	})
}

func TestSyncFetch(t *testing.T) {
	for _, c := range []struct {
		name          string
		prep          func(m *modeRepo)
		ahead, behind int
	}{
		{"behind, clean", func(m *modeRepo) { m.push("a.txt", "two\n"); m.push("b.txt", "bee\n") }, 0, 2},
		{"behind, dirty", func(m *modeRepo) { m.write("a.txt", "edit\n"); m.push("b.txt", "bee\n") }, 0, 1},
		{"ahead", func(m *modeRepo) { m.commitLocal("c.txt", "c\n") }, 1, 0},
		{"diverged", func(m *modeRepo) {
			m.commitLocal("c.txt", "c\n")
			m.commitLocal("d.txt", "d\n")
			m.push("a.txt", "two\n")
		}, 2, 1},
		{"at the commit", func(*modeRepo) {}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newModeRepo(t)
			c.prep(m)
			m.write("untracked.txt", "u\n")
			before := m.snap()
			res := m.sync(proto.SyncFetch)
			want := gitT(t, m.upstream, "rev-parse", "main")
			if a, b := counts(res); res.Status != "fetched" || res.Mode != proto.SyncFetch || res.To != want || a != c.ahead || b != c.behind {
				t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
			}
			m.unchanged(before)
			if m.ref("refs/remotes/lux/main") != want || m.ref("refs/lux/sync") != want {
				t.Fatal("refs not set")
			}
		})
	}
}

// A detached target (a tag or sha: no Branch) sets no remote-tracking ref,
// keeps local commits as a branch target does, and moves only a detached
// HEAD: a HEAD on a branch is kept, the branch where it is.
func TestSyncFastForwardDetached(t *testing.T) {
	for _, c := range []struct {
		name          string
		onBranch      bool
		prep          func(m *modeRepo)
		status        string
		ahead, behind int
		moves         bool
	}{
		{"clean behind", false, func(m *modeRepo) { m.push("a.txt", "two\n") }, "fast-forward", 0, 1, true},
		{"clean behind, HEAD on a branch", true, func(m *modeRepo) { m.push("a.txt", "two\n") }, "kept", 0, 1, false},
		{"local ahead", false, func(m *modeRepo) { m.commitLocal("local.txt", "mine\n") }, "ahead", 1, 0, false},
		{"diverged", false, func(m *modeRepo) { m.commitLocal("local.txt", "mine\n"); m.push("a.txt", "two\n") }, "kept", 1, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newModeRepo(t)
			m.write("untracked.txt", "u\n")
			if !c.onBranch {
				gitT(t, m.dir, "checkout", "-q", "--detach")
			}
			c.prep(m)
			mainTip := m.ref("refs/heads/main")
			before := m.snap()
			res := m.syncAs(proto.SyncFastForward, "")
			tip := gitT(t, m.upstream, "rev-parse", "main")
			if a, b := counts(res); res.Status != c.status || res.To != tip || a != c.ahead || b != c.behind ||
				res.Diverged != (c.ahead > 0 && c.behind > 0) || res.Dirty {
				t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
			}
			if m.ref("refs/remotes/lux/main") != "" || m.ref("refs/lux/sync") != tip {
				t.Fatalf("refs: lux/main %q, sync %q", m.ref("refs/remotes/lux/main"), m.ref("refs/lux/sync"))
			}
			if m.ref("refs/heads/main") != mainTip {
				t.Fatalf("main %s, was %s", m.ref("refs/heads/main"), mainTip)
			}
			if !c.moves {
				m.unchanged(before)
				return
			}
			if m.head() != tip || gitT(t, m.dir, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" || m.read("untracked.txt") != "u\n" {
				t.Fatalf("after: head %s", m.head())
			}
		})
	}
}

// raceGit puts a git ahead of the real one on PATH that, the first time
// it is asked to move a branch or HEAD (checkout, merge, update-ref of a
// refs/heads/ ref), first runs workload in the checkout with the real git
// as "$REAL" and an identity as $id; workload writes the id of the commit
// it makes to "$OUT". Returns OUT.
func raceGit(t *testing.T, workload string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "raced")
	script := `#!/bin/sh
REAL='` + real + `'
OUT='` + out + `'
hit=; upd=
for a in "$@"; do
	case "$a" in
	checkout|merge) hit=1 ;;
	update-ref) upd=1 ;;
	refs/heads/*) [ -n "$upd" ] && hit=1 ;;
	esac
done
if [ -n "$hit" ] && mkdir "$OUT.once" 2>/dev/null; then
	id="-c user.name=t -c user.email=t@t"
	set -e
	` + workload + `
	set +e
fi
exec "$REAL" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}

const (
	commitOnHead = `echo raced > local-race; "$REAL" add local-race; "$REAL" $id commit -qm raced; "$REAL" rev-parse HEAD > "$OUT"`
	detachCommit = `"$REAL" checkout -q --detach; ` + commitOnHead
)

// racedCommit is the commit raceGit's workload made; fails when none.
func racedCommit(t *testing.T, out string, res proto.SyncResult) string {
	t.Helper()
	b, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("no commit raced: %v (%+v)", err, res)
	}
	return strings.TrimSpace(string(b))
}

// A commit the workload makes while a fast-forward runs, between its
// checks and its move, is never reset away, nor HEAD moved off it.
func TestSyncFastForwardRacedCommit(t *testing.T) {
	t.Run("HEAD on the branch", func(t *testing.T) {
		m := newModeRepo(t)
		c2 := m.push("a.txt", "two\n")
		raced := raceGit(t, commitOnHead)
		res := m.sync(proto.SyncFastForward)
		local := racedCommit(t, raced, res)
		if res.Status == "fast-forward" || m.ref("refs/heads/main") != local || m.head() != local || m.read("local-race") != "raced\n" {
			t.Fatalf("%+v: main %s, raced %s", res, m.ref("refs/heads/main"), local)
		}
		if m.ref("refs/remotes/lux/main") != c2 {
			t.Fatal("refs/remotes/lux/main not set")
		}
	})
	t.Run("HEAD detached and a commit made on it, a branch target", func(t *testing.T) {
		m := newModeRepo(t)
		base := m.head()
		c2 := m.push("a.txt", "two\n")
		raced := raceGit(t, detachCommit)
		res := m.sync(proto.SyncFastForward)
		local := racedCommit(t, raced, res)
		if res.Status == "fast-forward" || m.head() != local || m.read("local-race") != "raced\n" ||
			gitT(t, m.dir, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
			t.Fatalf("%+v: HEAD %s, raced %s", res, m.head(), local)
		}
		if m.ref("refs/heads/main") != base || m.ref("refs/remotes/lux/main") != c2 {
			t.Fatalf("main %s, lux/main %s", m.ref("refs/heads/main"), m.ref("refs/remotes/lux/main"))
		}
	})
	t.Run("HEAD detached and a commit made on it, a sha target", func(t *testing.T) {
		m := newModeRepo(t)
		gitT(t, m.dir, "checkout", "-q", "--detach")
		m.push("a.txt", "two\n")
		raced := raceGit(t, `"$REAL" checkout -q -b raced; `+commitOnHead)
		res := m.syncAs(proto.SyncFastForward, "")
		local := racedCommit(t, raced, res)
		if res.Status == "fast-forward" || m.head() != local || m.ref("refs/heads/raced") != local || m.read("local-race") != "raced\n" {
			t.Fatalf("%+v: HEAD %s, raced %s", res, m.head(), local)
		}
	})
	// The workload detaches HEAD and commits just as a fast-forward of a
	// sha target would have moved HEAD.
	t.Run("HEAD on a branch, a sha target, detached and committed meanwhile", func(t *testing.T) {
		m := newModeRepo(t)
		m.push("a.txt", "two\n")
		raced := raceGit(t, detachCommit)
		before := m.snap()
		res := m.syncAs(proto.SyncFastForward, "")
		if b, _ := os.ReadFile(raced); len(b) > 0 {
			local := strings.TrimSpace(string(b))
			refs := gitT(t, m.dir, "for-each-ref", "--contains", local, "--format=%(refname)")
			if refs == "" && m.head() != local {
				t.Fatalf("%+v: raced detached commit %s lost from HEAD and all refs", res, local)
			}
			return
		}
		if res.Status != "kept" {
			t.Fatalf("%+v", res)
		}
		m.unchanged(before)
	})
}

// adopt makes the checkout's HEAD upstream's main, then commits
// path=content on it upstream: the checkout is one behind, an ancestor.
func (m *modeRepo) adopt(path, content string) string {
	m.t.Helper()
	gitT(m.t, m.upstream, "fetch", "-q", m.dir, "HEAD")
	gitT(m.t, m.upstream, "reset", "-q", "--hard", "FETCH_HEAD")
	return m.push(path, content)
}

// An operation in progress (a merge, a paused rebase) is the workload's:
// fast-forward keeps the checkout, refs and counts still updated.
func TestSyncFastForwardOperationInProgress(t *testing.T) {
	t.Run("merge, its index resolved to HEAD", func(t *testing.T) {
		m := newModeRepo(t)
		gitT(t, m.dir, "checkout", "-q", "-b", "side")
		m.commitLocal("side.txt", "side\n")
		gitT(t, m.dir, "checkout", "-q", "main")
		m.commitLocal("main.txt", "main\n")
		gitT(t, m.dir, "merge", "-q", "--no-commit", "--no-ff", "side")
		gitT(t, m.dir, "restore", "--source=HEAD", "--staged", "--worktree", ".")
		mergeHead := filepath.Join(m.dir, ".git", "MERGE_HEAD")
		before := m.snap()
		c2 := m.adopt("a.txt", "two\n")
		res := m.sync(proto.SyncFastForward)
		if a, b := counts(res); res.Status != "kept" || res.Operation != "merge" || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		m.unchanged(before)
		if _, err := os.Stat(mergeHead); err != nil || m.ref("refs/remotes/lux/main") != c2 {
			t.Fatalf("MERGE_HEAD: %v, lux/main %q", err, m.ref("refs/remotes/lux/main"))
		}
	})
	t.Run("paused rebase", func(t *testing.T) {
		m := newModeRepo(t)
		gitT(t, m.dir, "checkout", "-q", "-b", "side")
		m.commitLocal("local.txt", "local\n")
		m.push("a.txt", "two\n")
		gitT(t, m.dir, "fetch", "-q", m.upstream, "main")
		cmd := exec.Command("git", "-C", m.dir, "-c", "sequence.editor=true", "-c", "user.name=t", "-c", "user.email=t@t",
			"rebase", "-i", "--exec", "false", "FETCH_HEAD")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("rebase did not pause: %s", out)
		}
		rebaseDir := filepath.Join(m.dir, ".git", "rebase-merge")
		if _, err := os.Stat(rebaseDir); err != nil {
			t.Fatal(err)
		}
		head := m.head()
		tree, index := m.tree(), m.index()
		c3 := m.adopt("a.txt", "three\n")
		res := m.syncAs(proto.SyncFastForward, "")
		if a, b := counts(res); res.Status != "kept" || res.Operation != "rebase" || res.To != c3 || a != 0 || b != 1 {
			t.Fatalf("%+v (ahead %d behind %d)", res, a, b)
		}
		if _, err := os.Stat(rebaseDir); err != nil || m.head() != head || m.index() != index || len(m.tree()) != len(tree) {
			t.Fatalf("rebase state: %v, head %s was %s", err, m.head(), head)
		}
		if m.ref("refs/lux/sync") != c3 {
			t.Fatal("refs/lux/sync not set")
		}
	})
}

// gitStops runs a git command that must stop for the workload (a
// conflict): it fails.
func gitStops(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_EDITOR=true",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %v did not stop: %s", args, out)
	}
}

// sideConflict commits a.txt=side on branch side and a.txt=main on main,
// HEAD on main: picking or merging side conflicts.
func (m *modeRepo) sideConflict() {
	gitT(m.t, m.dir, "checkout", "-q", "-b", "side")
	m.commitLocal("a.txt", "side\n")
	gitT(m.t, m.dir, "checkout", "-q", "main")
	m.commitLocal("a.txt", "main\n")
}

// Modes fast-forward and fetch name the operation in progress, whatever
// the status, and leave it as it is.
func TestSyncReportsTheOperation(t *testing.T) {
	for _, c := range []struct {
		name, op, state string
		start           func(m *modeRepo)
	}{
		{"merge", "merge", "MERGE_HEAD", func(m *modeRepo) {
			m.sideConflict()
			gitStops(m.t, m.dir, "merge", "side")
		}},
		{"rebase, merge backend", "rebase", "rebase-merge", func(m *modeRepo) {
			m.commitLocal("a.txt", "mine\n")
			m.push("a.txt", "theirs\n")
			gitT(m.t, m.dir, "fetch", "-q", m.upstream, "main")
			gitStops(m.t, m.dir, "rebase", "--merge", "FETCH_HEAD")
		}},
		{"rebase, apply backend", "rebase", "rebase-apply", func(m *modeRepo) {
			m.commitLocal("a.txt", "mine\n")
			m.push("a.txt", "theirs\n")
			gitT(m.t, m.dir, "fetch", "-q", m.upstream, "main")
			gitStops(m.t, m.dir, "rebase", "--apply", "FETCH_HEAD")
		}},
		// rebase-apply too, but an am session: git rebase refuses it.
		{"am", "am", "rebase-apply/applying", func(m *modeRepo) {
			m.sideConflict()
			patch := filepath.Join(m.t.TempDir(), "side.patch")
			os.WriteFile(patch, []byte(gitT(m.t, m.dir, "format-patch", "--stdout", "main..side")+"\n"), 0o644)
			gitStops(m.t, m.dir, "am", patch)
		}},
		{"cherry-pick", "cherry-pick", "CHERRY_PICK_HEAD", func(m *modeRepo) {
			m.sideConflict()
			gitStops(m.t, m.dir, "cherry-pick", "side")
		}},
		{"revert", "revert", "REVERT_HEAD", func(m *modeRepo) {
			m.commitLocal("a.txt", "x\n")
			m.commitLocal("a.txt", "y\n")
			gitStops(m.t, m.dir, "revert", "--no-edit", "HEAD~1")
		}},
		// A range's first pick, conflicting, concluded by hand and not
		// continued: CHERRY_PICK_HEAD is gone, sequencer/todo is left.
		{"sequencer", "sequencer", "sequencer/todo", func(m *modeRepo) {
			m.sideConflict()
			gitT(m.t, m.dir, "checkout", "-q", "side")
			m.commitLocal("b.txt", "b\n")
			gitT(m.t, m.dir, "checkout", "-q", "main")
			gitStops(m.t, m.dir, "cherry-pick", "main..side")
			m.write("a.txt", "resolved\n")
			gitT(m.t, m.dir, "add", "a.txt")
			gitT(m.t, m.dir, "commit", "-q", "--no-edit")
		}},
		// A range A B stopped on B's conflict: CHERRY_PICK_HEAD and
		// sequencer/todo both, and git calls it a cherry-pick.
		{"range stopped on its second pick", "cherry-pick", "sequencer/todo", func(m *modeRepo) {
			gitT(m.t, m.dir, "checkout", "-q", "-b", "side")
			m.commitLocal("b.txt", "b\n")
			m.commitLocal("a.txt", "side\n")
			gitT(m.t, m.dir, "checkout", "-q", "main")
			m.commitLocal("a.txt", "main\n")
			gitStops(m.t, m.dir, "cherry-pick", "side~1", "side")
			if _, err := os.Stat(filepath.Join(m.dir, ".git", "CHERRY_PICK_HEAD")); err != nil {
				m.t.Fatal(err)
			}
		}},
	} {
		for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
			t.Run(c.name+", "+mode, func(t *testing.T) {
				m := newModeRepo(t)
				c.start(m)
				state := filepath.Join(m.dir, gitT(t, m.dir, "rev-parse", "--git-path", c.state))
				if _, err := os.Lstat(state); err != nil {
					t.Fatalf("not started: %v", err)
				}
				m.write("untracked.txt", "u\n")
				before := m.snap()
				tip := m.push("up.txt", "up\n")
				res := m.sync(mode)
				if res.Operation != c.op || res.To != tip || res.Ahead == nil || res.Behind == nil || *res.Behind < 1 {
					t.Fatalf("%+v", res)
				}
				if want := map[string]string{proto.SyncFastForward: "kept", proto.SyncFetch: "fetched"}[mode]; res.Status != want {
					t.Fatalf("status %q, want %q: %+v", res.Status, want, res)
				}
				m.unchanged(before)
				if _, err := os.Lstat(state); err != nil || m.ref("refs/remotes/lux/main") != tip {
					t.Fatalf("%s: %v, lux/main %q", c.state, err, m.ref("refs/remotes/lux/main"))
				}
			})
		}
	}
	t.Run("none", func(t *testing.T) {
		for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
			m := newModeRepo(t)
			m.commitLocal("c.txt", "c\n")
			m.push("a.txt", "two\n")
			if res := m.sync(mode); res.Operation != "" || res.Status == "failed" {
				t.Fatalf("%s: %+v", mode, res)
			}
		}
	})
	// An empty sequencer directory, git's own sequencer finds nothing in
	// progress: no operation, a fast-forward moves, the directory stays.
	t.Run("stale sequencer", func(t *testing.T) {
		for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
			m := newModeRepo(t)
			seq := filepath.Join(m.dir, ".git", "sequencer")
			if err := os.Mkdir(seq, 0o755); err != nil {
				t.Fatal(err)
			}
			tip := m.push("a.txt", "two\n")
			res := m.sync(mode)
			want := map[string]string{proto.SyncFastForward: "fast-forward", proto.SyncFetch: "fetched"}[mode]
			if res.Operation != "" || res.Status != want {
				t.Fatalf("%s: %+v", mode, res)
			}
			if mode == proto.SyncFastForward && m.head() != tip {
				t.Fatalf("HEAD %s, want %s", m.head(), tip)
			}
			if _, err := os.Stat(seq); err != nil {
				t.Fatalf("sequencer removed: %v", err)
			}
		}
	})
}

// fetchGit puts a git ahead of the real one on PATH that runs workload (sh,
// in the checkout, the real git as "$REAL") once, at the sync's fetch:
// after the operation was first looked at.
func fetchGit(t *testing.T, workload string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nREAL='" + real + "'\nhit=\nfor a in \"$@\"; do [ \"$a\" = fetch ] && hit=1; done\n" +
		"if [ -n \"$hit\" ] && mkdir '" + filepath.Join(dir, "once") + "' 2>/dev/null; then\n" + workload + "\nfi\n" +
		"exec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// An operation that starts or ends while a sync fetches is what its
// result says, whatever the outcome.
func TestSyncOperationChangesDuringFetch(t *testing.T) {
	const pause = `GIT_EDITOR=true "$REAL" -c sequence.editor=true -c user.name=t -c user.email=t@t rebase -i --exec false HEAD~1 >/dev/null 2>&1`
	for _, c := range []struct {
		name, mode, status, broken string
		setup                      func(m *modeRepo)
	}{
		// A local commit and an upstream one: diverged.
		{"kept", proto.SyncFastForward, "kept", "", func(m *modeRepo) { m.commitLocal("local.txt", "local\n"); m.push("up.txt", "up\n") }},
		{"ahead", proto.SyncFastForward, "ahead", "", func(m *modeRepo) { m.commitLocal("local.txt", "local\n") }},
		{"up-to-date", proto.SyncFastForward, "up-to-date", "", func(m *modeRepo) {
			m.commitLocal("local.txt", "local\n")
			gitT(m.t, m.upstream, "fetch", "-q", m.dir, "HEAD")
			gitT(m.t, m.upstream, "reset", "-q", "--hard", "FETCH_HEAD")
		}},
		{"fetched", proto.SyncFetch, "fetched", "", func(m *modeRepo) { m.commitLocal("local.txt", "local\n"); m.push("up.txt", "up\n") }},
		{"failed, the bundle's commit", proto.SyncFetch, "failed", "commit", func(m *modeRepo) { m.commitLocal("local.txt", "local\n") }},
		{"failed, the fetch", proto.SyncFastForward, "failed", "fetch", func(m *modeRepo) { m.commitLocal("local.txt", "local\n") }},
	} {
		t.Run("starts, "+c.name, func(t *testing.T) {
			m := newModeRepo(t)
			c.setup(m)
			fetchGit(t, pause)
			res := m.syncChecked(c.mode, c.broken)
			if _, err := os.Stat(filepath.Join(m.dir, ".git", "rebase-merge")); err != nil {
				t.Fatalf("the rebase did not start: %v", err)
			}
			if res.Status != c.status || res.Operation != "rebase" {
				t.Fatalf("%+v", res)
			}
		})
		t.Run("ends, "+c.name, func(t *testing.T) {
			m := newModeRepo(t)
			c.setup(m)
			gitStops(t, m.dir, "-c", "sequence.editor=true", "rebase", "-i", "--exec", "false", "HEAD~1")
			if _, err := os.Stat(filepath.Join(m.dir, ".git", "rebase-merge")); err != nil {
				t.Fatalf("the rebase did not start: %v", err)
			}
			fetchGit(t, `"$REAL" rebase --abort >/dev/null 2>&1 || exit 1`)
			res := m.syncChecked(c.mode, c.broken)
			if _, err := os.Stat(filepath.Join(m.dir, ".git", "rebase-merge")); err == nil {
				t.Fatal("the rebase did not end")
			}
			if res.Status != c.status || res.Operation != "" {
				t.Fatalf("%+v", res)
			}
		})
	}
}

// syncChecked is sync; when broken, of a commit the bundle lacks, or
// ("fetch") from a bundle that is not there.
func (m *modeRepo) syncChecked(mode, broken string) proto.SyncResult {
	m.t.Helper()
	if broken == "" {
		return m.sync(mode)
	}
	bundle := filepath.Join(m.t.TempDir(), "r.bundle")
	gitT(m.t, m.upstream, "update-ref", "refs/lux/sync", "main")
	if broken != "fetch" {
		gitT(m.t, m.upstream, "bundle", "create", "-q", bundle, "refs/lux/sync")
	}
	return SyncRepos(context.Background(), proto.SyncArgs{Repos: []proto.SyncRepo{{Name: "r", Path: m.dir, Ref: "main", Mode: mode,
		Commit: strings.Repeat("0", 40), Branch: "main", Bundle: bundle}}})[0]
}

// An unknown mode fails, the checkout untouched.
func TestSyncUnknownMode(t *testing.T) {
	m := newModeRepo(t)
	m.push("a.txt", "two\n")
	before := m.snap()
	res := m.sync("rebase")
	if res.Status != "failed" || !strings.Contains(res.Error, `unknown sync mode "rebase"`) {
		t.Fatalf("%+v", res)
	}
	m.unchanged(before)
}

// Mode move, explicitly or by default, still resets a dirty checkout.
func TestSyncMoveStillResets(t *testing.T) {
	for _, mode := range []string{"", proto.SyncMove} {
		m := newModeRepo(t)
		m.write("a.txt", "local edit\n")
		c2 := m.push("b.txt", "bee\n")
		res := m.sync(mode)
		if res.Status != "reset" || res.Mode != proto.SyncMove || res.Ahead != nil || res.Saved != "refs/lux/pre-sync" || m.head() != c2 ||
			m.read("a.txt") != "one\n" || m.ref("refs/remotes/lux/main") != "" {
			t.Fatalf("mode %q: %+v", mode, res)
		}
	}
}
