package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// containerExec is a podman whose `exec --user U --workdir W NAME cmd...`
// runs cmd on the host in $ROOT/co/W, with /.lux/run in any argument read
// as the runtime volume ($ROOT/rt); git is a wrapper that appends each
// `git bundle` it runs to $ROOT/bundles. Anything else exits 125.
const containerExec = `ROOT=$(dirname "$BARE")
[ "$1" = exec ] && [ "$2" = --user ] && [ "$4" = --workdir ] || exit 125
cd "$ROOT/co$5" || exit 125
shift 6
n=$#
while [ $n -gt 0 ]; do
	a=$1; shift
	case "$a" in /.lux/run/*) a="$ROOT/rt${a#/.lux/run}" ;; esac
	set -- "$@" "$a"
	n=$((n-1))
done
PATH="$ROOT/bin:$PATH" exec "$@"
`

// pushFixture is a syncFixture whose spec pushes app and lib to lux/x,
// each a checkout at $ROOT/co/w/<name> cloned from its bare repository,
// run through containerExec.
type pushFixture struct {
	*syncFixture
	root  string
	bares map[string]string
}

func newPushFixture(t *testing.T, script string) *pushFixture {
	t.Helper()
	f := newSyncFixture(t, script)
	root := filepath.Dir(f.bare)
	pf := &pushFixture{syncFixture: f, root: root, bares: map[string]string{"app": f.bare, "lib": filepath.Join(root, "lib.git")}}
	run(t, root, "git", "clone", "-q", "--bare", f.bare, pf.bares["lib"])
	for name, bare := range pf.bares {
		run(t, root, "git", "clone", "-q", bare, pf.checkout(name))
	}
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	pf.raceApp(t, ":")
	f.sp.Git = &spec.Git{Repositories: []spec.Repository{
		{Name: "app", URL: pf.bares["app"], Path: "/w/app"},
		{Name: "lib", URL: pf.bares["lib"], Path: "/w/lib"},
	}, Push: &spec.Push{Branch: "lux/x"}}
	f.p.assign.Spec = f.sp
	f.p.user = passwd.User{UID: os.Getuid(), GID: os.Getgid()}
	// An old container's lux-shim: the refusal must not depend on it.
	f.p.state.ShimSyncModes = false
	return pf
}

func (pf *pushFixture) checkout(name string) string { return filepath.Join(pf.root, "co", "w", name) }

// bundled is the checkouts a git bundle ran in.
func (pf *pushFixture) bundled() string {
	b, _ := os.ReadFile(filepath.Join(pf.root, "bundles"))
	return string(b)
}

// branch is lux/x in name's bare repository, "" when absent.
func (pf *pushFixture) branch(name string) string {
	out, _ := exec.Command("git", "-C", pf.bares[name], "rev-parse", "--verify", "-q", "refs/heads/lux/x").Output()
	return strings.TrimSpace(string(out))
}

// pushed runs a push and returns its git.push results by repository.
func (pf *pushFixture) pushed(t *testing.T) map[string]map[string]any {
	t.Helper()
	pf.p.push(context.Background(), proto.Push{RequestID: "rq"})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		pf.mu.Lock()
		evs := pf.pushes
		pf.mu.Unlock()
		if len(evs) == 0 {
			continue
		}
		out := map[string]map[string]any{}
		for _, x := range evs[0]["results"].([]any) {
			r := x.(map[string]any)
			out[r["repo"].(string)] = r
		}
		return out
	}
	t.Fatal("no git.push event")
	return nil
}

// gitEnv is a git with an identity and no global config, as on a CI host
// whose hostname git cannot make an email of (git am and rebase then fail
// before they start, rather than stopping on a conflict).
func gitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
}

// gitAs is git in dir with gitEnv, for a command expected to stop.
func gitAs(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	return cmd
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkout with a rebase in progress is refused, never bundled, and
// its push branch is not created; another repository pushes as normal.
func TestPushRefusesAnOperationInProgress(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app, lib := pf.checkout("app"), pf.checkout("lib")
	// app: a local commit rebased onto a conflicting one, stopped.
	run(t, app, "git", "checkout", "-q", "-b", "mine")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("mine\n"), 0o644)
	run(t, app, "git", "commit", "-qam", "mine")
	run(t, app, "git", "checkout", "-q", "main")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("theirs\n"), 0o644)
	run(t, app, "git", "commit", "-qam", "theirs")
	run(t, app, "git", "checkout", "-q", "mine")
	if out, err := gitAs(app, "rebase", "main").CombinedOutput(); err == nil {
		t.Fatalf("the rebase did not stop: %s", out)
	}
	if _, err := os.Stat(filepath.Join(app, ".git", "rebase-merge")); err != nil {
		t.Fatalf("the rebase stopped without a rebase in progress: %v", err)
	}
	os.WriteFile(filepath.Join(lib, "l.txt"), []byte("lib\n"), 0o644)
	run(t, lib, "git", "add", "l.txt")
	run(t, lib, "git", "commit", "-qm", "lib")
	libHead := run(t, lib, "git", "rev-parse", "HEAD")

	res := pf.pushed(t)
	a := res["app"]
	if a["status"] != "refused" || a["operation"] != "rebase" || a["branch"] != "lux/x" ||
		a["error"] != "a rebase is in progress in the checkout: finish or abort it, then push" || a["commit"] != nil {
		t.Fatalf("app: %v", a)
	}
	if b := pf.bundled(); strings.Contains(b, "/co/w/app") {
		t.Fatalf("app was bundled: %q", b)
	}
	pf.unpushed(t)
	if l := res["lib"]; l["status"] != "pushed" || l["commit"] != libHead || l["operation"] != nil || pf.branch("lib") != libHead {
		t.Fatalf("lib: %v, lux/x %q", l, pf.branch("lib"))
	}
}

// Each operation is named in the refusal.
func TestPushRefusalNamesTheOperation(t *testing.T) {
	for _, c := range []struct{ op, state string }{
		{"merge", "MERGE_HEAD"}, {"rebase", "rebase-apply"}, {"cherry-pick", "CHERRY_PICK_HEAD"},
		{"revert", "REVERT_HEAD"}, {"sequencer", "sequencer/todo"},
	} {
		t.Run(c.op, func(t *testing.T) {
			pf := newPushFixture(t, containerExec)
			// The state git leaves, as git resolves its path.
			p := filepath.Join(pf.checkout("app"), run(t, pf.checkout("app"), "git", "rev-parse", "--git-path", c.state))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			a := pf.pushed(t)["app"]
			want := "a " + c.op + " is in progress in the checkout: finish or abort it, then push"
			if a["status"] != "refused" || a["operation"] != c.op || a["error"] != want {
				t.Fatalf("%v", a)
			}
			if strings.Contains(pf.bundled(), "/co/w/app") || pf.branch("app") != "" {
				t.Fatal("bundled or pushed")
			}
		})
	}
}

// raceBundle makes the container's git run workload (sh, in the checkout,
// the real git as "$REAL") once, just before app's `git bundle create`:
// after every check made before the bundle.
func (pf *pushFixture) raceBundle(t *testing.T, workload string) {
	t.Helper()
	pf.raceApp(t, `if [ "$1 $2" = 'bundle create' ] && mkdir "${ONCE}bundle" 2>/dev/null; then
`+workload+`
fi`)
}

// raceApp installs the container's git, which appends the checkout of each
// `git bundle` to $ROOT/bundles: body runs in app's checkout before every
// git command there, with $REAL the real git, $ONCE a path prefix for
// run-once markers and "$@" the command, then the command runs (unless
// body exits).
func (pf *pushFixture) raceApp(t *testing.T, body string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nREAL='" + real + "'\nONCE='" + pf.root + "/once-'\n" +
		"if [ \"$PWD\" = '" + pf.checkout("app") + "' ]; then\n" + body + "\nfi\n" +
		"[ \"$1\" = bundle ] && echo \"$PWD\" >> '" + filepath.Join(pf.root, "bundles") + "'\n" +
		"exec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(pf.root, "bin", "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
}

// refusedUnpushed asserts app was refused for a rebase, nothing pushed
// and no bundle left.
func (pf *pushFixture) refusedUnpushed(t *testing.T, a map[string]any) {
	t.Helper()
	if a["status"] != "refused" || a["operation"] != "rebase" || a["commit"] != nil {
		t.Fatalf("app: %v", a)
	}
	pf.unpushed(t)
}

// unpushed asserts app's lux/x was not pushed and no bundle is left.
func (pf *pushFixture) unpushed(t *testing.T) {
	t.Helper()
	if got := pf.branch("app"); got != "" {
		t.Fatalf("app's lux/x pushed at %s", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(pf.root, "rt", "push")); len(entries) != 0 {
		t.Fatalf("bundles left: %v", entries)
	}
}

// commitTwo commits two files in app; returns HEAD.
func (pf *pushFixture) commitTwo(t *testing.T) string {
	app := pf.checkout("app")
	for _, name := range []string{"first", "second"} {
		os.WriteFile(filepath.Join(app, name), []byte(name), 0o644)
		run(t, app, "git", "add", name)
		run(t, app, "git", "commit", "-qm", name)
	}
	return run(t, app, "git", "rev-parse", "HEAD")
}

// An operation that starts after the checks before the bundle (a rebase
// stopped after replaying a commit, HEAD half-done when bundled) is seen
// by the check after it: refused, nothing pushed, the bundle deleted.
func TestPushRefusesAnOperationStartedWhileBundling(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app := pf.checkout("app")
	pf.commitTwo(t)
	pf.raceBundle(t, `GIT_EDITOR=true "$REAL" -c sequence.editor=true -c user.name=t -c user.email=t@t rebase -i --exec false HEAD~2 >/dev/null 2>&1`)
	a := pf.pushed(t)["app"]
	if _, err := os.Stat(filepath.Join(app, ".git", "rebase-merge")); err != nil {
		t.Fatalf("the rebase did not start: %v", err)
	}
	pf.refusedUnpushed(t, a)
}

// The first check: a rebase already stopped when the push starts is
// refused before HEAD is read. (Here reading HEAD would abort it, so
// without that check the later ones see none and it would push.)
func TestPushRefusesAnOperationBeforeReadingHead(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app := pf.checkout("app")
	pf.commitTwo(t)
	if out, err := gitAs(app, "-c", "sequence.editor=true", "rebase", "-i", "--exec", "false", "HEAD~2").CombinedOutput(); err == nil {
		t.Fatalf("the rebase did not stop: %s", out)
	}
	if _, err := os.Stat(filepath.Join(app, ".git", "rebase-merge")); err != nil {
		t.Fatalf("the rebase did not start: %v", err)
	}
	pf.raceApp(t, `if [ "$1 $2 $3" = 'rev-parse --verify HEAD' ] && mkdir "${ONCE}head" 2>/dev/null; then
	"$REAL" rebase --abort >/dev/null 2>&1 || exit 1
fi`)
	pf.refusedUnpushed(t, pf.pushed(t)["app"])
	if _, err := os.Stat(filepath.Join(app, ".git", "rebase-merge")); err != nil {
		t.Fatal("HEAD was read: the first check let the rebase through")
	}
}

// The second check: a rebase that starts while HEAD is read is refused,
// even if it ends again before the bundle (HEAD back where it was read,
// so the third check and the head compare cannot see it).
func TestPushRefusesAnOperationStartedWhileReadingHead(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	pf.commitTwo(t)
	pf.raceApp(t, `if [ "$1 $2 $3" = 'rev-parse --verify HEAD' ] && mkdir "${ONCE}head" 2>/dev/null; then
	S=$("$REAL" "$@") || exit 1
	GIT_EDITOR=true "$REAL" -c sequence.editor=true -c user.name=t -c user.email=t@t rebase -i --exec false HEAD~2 >/dev/null 2>&1
	[ -d .git/rebase-merge ] || exit 1
	printf '%s\n' "$S"
	exit 0
fi
if [ "$1 $2" = 'bundle create' ] && mkdir "${ONCE}bundle" 2>/dev/null; then
	"$REAL" rebase --abort >/dev/null 2>&1 || exit 1
fi`)
	pf.refusedUnpushed(t, pf.pushed(t)["app"])
	if _, err := os.Stat(filepath.Join(pf.root, "once-head")); err != nil {
		t.Fatal("the rebase was never started at the HEAD read")
	}
}

// HEAD moving while the bundle is made (no operation): failed, nothing
// pushed, the bundle deleted.
func TestPushFailsWhenHeadMovesWhileBundling(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	head := pf.commitTwo(t)
	pf.raceBundle(t, `"$REAL" reset -q --hard HEAD~1`)
	a := pf.pushed(t)["app"]
	if moved := run(t, pf.checkout("app"), "git", "rev-parse", "HEAD"); moved == head {
		t.Fatal("HEAD did not move")
	}
	if a["status"] != "failed" || a["error"] != "the checkout changed while it was being pushed; push again" ||
		a["operation"] != nil || a["commit"] != nil {
		t.Fatalf("app: %v", a)
	}
	pf.unpushed(t)
}

// An am session stopped on a conflict is refused as am, not rebase.
func TestPushRefusesAnAmSession(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app := pf.checkout("app")
	run(t, app, "git", "checkout", "-q", "-b", "side")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("side\n"), 0o644)
	run(t, app, "git", "add", "a.txt")
	run(t, app, "git", "commit", "-qm", "side")
	patch := filepath.Join(pf.root, "side.patch")
	os.WriteFile(patch, []byte(run(t, app, "git", "format-patch", "--stdout", "main..side")+"\n"), 0o644)
	run(t, app, "git", "checkout", "-q", "main")
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("main\n"), 0o644)
	run(t, app, "git", "add", "a.txt")
	run(t, app, "git", "commit", "-qm", "main")
	if out, err := gitAs(app, "am", patch).CombinedOutput(); err == nil {
		t.Fatalf("git am did not stop: %s", out)
	}
	if _, err := os.Stat(filepath.Join(app, ".git", "rebase-apply", "applying")); err != nil {
		t.Fatalf("git am stopped without an am session: %v", err)
	}
	a := pf.pushed(t)["app"]
	if a["status"] != "refused" || a["operation"] != "am" ||
		a["error"] != "an am is in progress in the checkout: finish or abort it, then push" {
		t.Fatalf("%v", a)
	}
	if strings.Contains(pf.bundled(), "/co/w/app") || pf.branch("app") != "" {
		t.Fatal("bundled or pushed")
	}
}

// An empty sequencer directory (no todo: git has no sequence in progress)
// is not an operation: the checkout is pushed, the directory left alone.
func TestPushIgnoresAStaleSequencer(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	app := pf.checkout("app")
	head := pf.commitTwo(t)
	seq := filepath.Join(app, ".git", "sequencer")
	if err := os.Mkdir(seq, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", app, "cherry-pick", "--continue").CombinedOutput(); err == nil {
		t.Fatalf("git sees a sequence: %s", out)
	}
	a := pf.pushed(t)["app"]
	if a["status"] != "pushed" || a["commit"] != head || a["operation"] != nil || pf.branch("app") != head {
		t.Fatalf("%v", a)
	}
	if _, err := os.Stat(seq); err != nil {
		t.Fatalf("sequencer removed: %v", err)
	}
}

// When the check cannot run, the push fails: nothing is bundled.
func TestPushFailsClosed(t *testing.T) {
	for _, c := range []struct{ name, script string }{
		{"container gone", `echo "no such container" >&2; exit 125`},
		{"no shell in the image", `[ "$7" = sh ] && { echo 'executable file "sh" not found' >&2; exit 127; }
` + containerExec},
		{"not a checkout", `rm -rf "$(dirname "$BARE")/co/w/app/.git"
` + containerExec},
	} {
		t.Run(c.name, func(t *testing.T) {
			pf := newPushFixture(t, c.script)
			a := pf.pushed(t)["app"]
			if a["status"] != "failed" || a["error"] == "" || a["operation"] != nil {
				t.Fatalf("%v", a)
			}
			if strings.Contains(pf.bundled(), "/co/w/app") || pf.branch("app") != "" {
				t.Fatal("bundled or pushed")
			}
		})
	}
}
