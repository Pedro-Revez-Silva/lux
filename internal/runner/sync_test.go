package runner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Two syncs of one Run under way at once keep their own bundles: preparing
// the second leaves the first's for its shim to fetch, and each is removed
// when its own sync is done.
func TestConcurrentSyncsKeepTheirBundles(t *testing.T) {
	f := newSyncFixture(t, `true`)
	p, sp := f.p, f.sp
	rt := filepath.Join(p.r.cfg.DataDir, "rt")
	refs := []proto.SyncRef{{Repo: "app", Ref: "main"}}
	ctx := context.Background()

	first, _ := p.prepareSync(ctx, sp, refs, "s1", false)
	second, _ := p.prepareSync(ctx, sp, refs, "s2", false)
	if first == nil || second == nil || first.Repos[0].Bundle == second.Repos[0].Bundle {
		t.Fatalf("bundles: %+v %+v", first, second)
	}
	host := func(b string) string { return filepath.Join(rt, strings.TrimPrefix(b, proto.ShimRunDir+"/")) }
	for _, a := range []*proto.SyncArgs{first, second} {
		if _, err := os.Stat(host(a.Repos[0].Bundle)); err != nil {
			t.Fatalf("a sync's bundle is gone before its shim ran: %v", err)
		}
	}
	p.removeSyncBundles(ctx, "s1")
	if _, err := os.Stat(host(first.Repos[0].Bundle)); !os.IsNotExist(err) {
		t.Fatalf("s1's bundle after s1: %v", err)
	}
	if _, err := os.Stat(host(second.Repos[0].Bundle)); err != nil {
		t.Fatalf("s2's bundle after s1: %v", err)
	}
	// A request id is never a path.
	if d := syncSubdir("../../etc"); strings.ContainsAny(d, "./") {
		t.Fatalf("subdir of a hostile id: %q", d)
	}
}

// A sync's mode reaches lux-shim sync, in the first try and in the
// whole-history retry; a sync whose checkouts were kept, ahead or only
// fetched moved nothing: sync.done says changed false, and the next
// bundle's base stays where it was.
func TestSyncModeReachesTheShim(t *testing.T) {
	for _, c := range []struct {
		status  string
		changed bool
	}{{"kept", false}, {"ahead", false}, {"fetched", false}, {"fast-forward", true}} {
		t.Run(c.status, func(t *testing.T) {
			// Each lux-shim sync's argument (podman's last) to $0.args.<n>;
			// the first answers missingBase, the retry c.status.
			f := newSyncFixture(t, `for a; do last=$a; done
n=$(cat "$0.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0.n"
printf '%s' "$last" > "$0.args.$n"
if [ $n = 1 ]; then
  echo '[{"repo":"app","ref":"main","status":"failed","error":"no base","missingBase":true}]'
else
  echo '[{"repo":"app","ref":"main","mode":"fast-forward","status":"`+c.status+`","to":"0123456789012345678901234567890123456789"}]'
fi`)
			f.p.state.GitBases = map[string]string{"app": "base"}
			f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq1", Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFastForward}}})
			bin := f.p.r.pm.Bin
			for _, n := range []string{"1", "2"} {
				b, err := os.ReadFile(bin + ".args." + n)
				if err != nil {
					t.Fatalf("lux-shim sync %s: %v", n, err)
				}
				var a proto.SyncArgs
				if err := json.Unmarshal(b, &a); err != nil || len(a.Repos) != 1 || a.Repos[0].Mode != proto.SyncFastForward {
					t.Fatalf("lux-shim sync %s's args: %s", n, b)
				}
			}
			waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.done) == 1 })
			if evs := f.events(); len(evs) != 1 || evs[0].Status != c.status || !evs[0].FullBundle {
				t.Fatalf("git.sync: %+v", evs)
			}
			if got := f.done[0]["changed"]; got != c.changed {
				t.Fatalf("sync.done changed %v, want %v", got, c.changed)
			}
			f.p.mu.Lock()
			base := f.p.state.GitBases["app"]
			f.p.mu.Unlock()
			if moved := base != "base"; moved != c.changed {
				t.Fatalf("base %q after %s", base, c.status)
			}
		})
	}
}

// A placement whose container started with a lux-shim older than sync
// modes never runs a fast-forward or fetch there (that shim would move):
// each fails, naming why, without lux-shim sync. Mode move still runs.
func TestSyncModesOnAnOldShim(t *testing.T) {
	f := newSyncFixture(t, `echo called >> "$0.calls"
echo '[{"repo":"app","ref":"main","status":"up-to-date"}]'`)
	f.p.state.ShimSyncModes = false
	for _, mode := range []string{proto.SyncFastForward, proto.SyncFetch} {
		f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq-" + mode, Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: mode}}})
	}
	if _, err := os.Stat(f.p.r.pm.Bin + ".calls"); !os.IsNotExist(err) {
		t.Fatalf("lux-shim sync ran on an old shim: %v", err)
	}
	waitUntil(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.done) == 2 })
	evs := f.events()
	if len(evs) != 2 {
		t.Fatalf("git.sync: %+v", evs)
	}
	for _, ev := range evs {
		if ev.Status != "failed" || ev.Error != "this Run's lux-shim predates sync modes; resume it to update" {
			t.Fatalf("git.sync: %+v", ev)
		}
	}
	for _, mode := range []string{"", proto.SyncMove} {
		f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq-move" + mode, Repos: []proto.SyncRef{{Repo: "app", Ref: "main", Mode: mode}}})
	}
	b, _ := os.ReadFile(f.p.r.pm.Bin + ".calls")
	if strings.Count(string(b), "called") != 2 {
		t.Fatalf("mode move on an old shim: %d lux-shim syncs, want 2", strings.Count(string(b), "called"))
	}
}

// The runner asks its --shim whether it knows sync modes, and says so in
// Hello: the shim built from this tree does; one that ignores the mode
// (an older shim, which reports none) does not.
func TestShimKnowsSyncModes(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "lux-shim")
	build := exec.Command("go", "build", "-o", shim, "github.com/marcioapm/lux/cmd/lux-shim")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if !shimKnowsSyncModes(shim) {
		t.Fatal("this tree's lux-shim: no sync modes")
	}
	old := filepath.Join(dir, "old-shim")
	os.WriteFile(old, []byte("#!/bin/sh\necho '[{\"repo\":\"probe\",\"ref\":\"\",\"status\":\"failed\",\"error\":\"/nonexistent has no checkout\"}]'\n"), 0o755)
	if shimKnowsSyncModes(old) || shimKnowsSyncModes(filepath.Join(dir, "missing")) {
		t.Fatal("an old or missing lux-shim: sync modes")
	}
	r := &Runner{shimSyncModes: false}
	if slices.Contains(r.capabilities(), proto.CapSyncModes) {
		t.Fatal("Hello offers sync modes with an old shim")
	}
	r.shimSyncModes = true
	if !slices.Contains(r.capabilities(), proto.CapSyncModes) {
		t.Fatal("Hello without sync modes")
	}
}

// A lux-shim that never answers, its child holding the output open past
// its own death, is no sync modes, within the probe's deadline: runner
// startup waits for it. A child in the shim's process group goes with it;
// one that escaped the group (setsid) keeps the output open, and the probe
// stops waiting for it WaitDelay after the deadline.
func TestShimProbeHangs(t *testing.T) {
	probe := func(t *testing.T, launch string) (pid string) {
		dir := t.TempDir()
		shim := filepath.Join(dir, "lux-shim")
		pidFile := filepath.Join(dir, "child")
		if err := os.WriteFile(shim, []byte("#!/bin/sh\n"+launch+" sleep 25 &\necho $! > '"+pidFile+"'\nwait\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ok := shimKnowsSyncModes(shim)
		took := time.Since(start)
		b, _ := os.ReadFile(pidFile)
		pid = strings.TrimSpace(string(b))
		if pid != "" {
			t.Cleanup(func() { _ = exec.Command("kill", "-9", pid).Run() })
		}
		if ok || took > 13*time.Second {
			t.Fatalf("a hanging lux-shim: sync modes %v, after %s", ok, took)
		}
		return pid
	}
	t.Run("its child in its process group", func(t *testing.T) {
		pid := probe(t, "")
		if pid == "" {
			t.Fatal("the hanging lux-shim started no child")
		}
		// Its process group was killed with it: the child too. SIGKILL is
		// delivered when the child is next scheduled, so on a loaded host
		// it can still be runnable just after the probe returns: wait for
		// it to die (gone, or a zombie until init reaps it), well short of
		// its own 25s sleep.
		var stat []byte
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var err error
			if stat, err = os.ReadFile("/proc/" + pid + "/stat"); err != nil ||
				strings.Contains(string(stat), ") Z ") || strings.Contains(string(stat), ") X ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the hanging lux-shim's child %q outlived the probe: %s", pid, stat)
			}
		}
	})
	t.Run("its child escaped the process group", func(t *testing.T) {
		if _, err := exec.LookPath("setsid"); err != nil {
			t.Fatal("setsid is needed to start a child outside the shim's process group")
		}
		probe(t, "setsid")
	})
}

// A sync that fetched a commit into the checkout, moved or not, is the
// next bundle's prerequisite: two fetch syncs of one target in a row
// bundle its history once. A failed one is not, and the live diff's base
// stays the clone's throughout.
func TestSyncBundlesFromTheLastFetch(t *testing.T) {
	f := newSyncFixture(t, `true`)
	ctx := context.Background()
	base, tip := bigRange(t, f.bare)
	f.p.state.GitBases = map[string]string{"app": base}
	refs := []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFetch}}
	size := func(a *proto.SyncArgs) int64 { return bundleSize(t, f, a) }
	sync := func(id, status string) *proto.SyncArgs {
		t.Helper()
		a, failed := f.p.prepareSync(ctx, f.sp, refs, id, false)
		if a == nil || len(failed) != 0 || a.Repos[0].Commit != tip {
			t.Fatalf("%s: %+v %+v", id, a, failed)
		}
		f.p.reportSync(ctx, proto.SyncResult{Repo: "app", Ref: "main", Mode: proto.SyncFetch, Status: status, To: tip}, id)
		return a
	}
	first := sync("s1", "failed")
	if size(first) < 4<<20 || first.Repos[0].Base != base {
		t.Fatalf("first bundle: %d bytes, base %q", size(first), first.Repos[0].Base)
	}
	// The first failed: the second bundles the same history again.
	second := sync("s2", "fetched")
	if size(second) < 4<<20 || second.Repos[0].Base != base {
		t.Fatalf("after a failed sync: %d bytes, base %q", size(second), second.Repos[0].Base)
	}
	third := sync("s3", "fetched")
	t.Logf("bundles: %d, %d, %d bytes", size(first), size(second), size(third))
	if size(third) > 64<<10 || third.Repos[0].Base != tip {
		t.Fatalf("after a fetch of the same commit: %d bytes, base %q", size(third), third.Repos[0].Base)
	}
	f.p.mu.Lock()
	diffBase := f.p.state.GitBases["app"]
	f.p.mu.Unlock()
	if diffBase != base {
		t.Fatalf("live diff base %q, want the clone's %s", diffBase, base)
	}
	// A restarted runner reads it back.
	st, err := readRunState(f.p.dir)
	if err != nil || st.SyncBases["app"] != tip || st.GitBases["app"] != base {
		t.Fatalf("run state: %+v %v", st, err)
	}
}

// bigRange makes main in bare base (its tip now), then a commit adding 4
// MiB that does not compress, then a small one on top (git writes no empty
// bundle, so a bundle of the tip alone holds only the small one).
func bigRange(t *testing.T, bare string) (base, tip string) {
	t.Helper()
	base = gitOut(t, bare, "rev-parse", "main")
	big := filepath.Join(t.TempDir(), "big")
	b := make([]byte, 4<<20)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, b, 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitOut(t, bare, "hash-object", "-w", big)
	commit := func(tree, parent, msg string) string {
		return gitOut(t, bare, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", tree, "-p", parent, "-m", msg)
	}
	tip = commit(gitMktree(t, bare, "100644 blob "+blob+"\tbig\n100644 blob "+gitOut(t, bare, "rev-parse", base+":a.txt")+"\tsmall\n"),
		commit(gitMktree(t, bare, "100644 blob "+blob+"\tbig\n"), base, "big"), "small")
	gitOut(t, bare, "update-ref", "refs/heads/main", tip)
	return base, tip
}

func bundleSize(t *testing.T, f *syncFixture, a *proto.SyncArgs) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(f.p.r.cfg.DataDir, "rt", strings.TrimPrefix(a.Repos[0].Bundle, proto.ShimRunDir+"/")))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// A placement restored from a snapshot whose checkout an earlier placement
// only fetched into (a fetch resume) starts with luxd's SyncBases from its
// assignment, as decoded off the wire: its next sync bundles none of what
// was fetched, while its live diff's base stays the clone's.
func TestRestoredPlacementBundlesFromItsSyncBases(t *testing.T) {
	f := newSyncFixture(t, `true`)
	ctx := context.Background()
	base, tip := bigRange(t, f.bare)
	f.p.r.shimSyncModes = true
	var a proto.Assign
	if err := json.Unmarshal(proto.Marshal(proto.Assign{RunID: "run_x", Epoch: 2, Spec: f.sp,
		GitBases: map[string]string{"app": base}, SyncBases: map[string]string{"app": tip}}), &a); err != nil {
		t.Fatal(err)
	}
	f.p.epoch, f.p.assign, f.p.state = 2, &a, nil
	f.p.restoreState()
	f.p.mu.Lock()
	f.p.state.User = "1000:1000"
	f.p.mu.Unlock()
	args, failed := f.p.prepareSync(ctx, f.sp, []proto.SyncRef{{Repo: "app", Ref: "main", Mode: proto.SyncFetch}}, "s1", false)
	if args == nil || len(failed) != 0 || args.Repos[0].Commit != tip {
		t.Fatalf("%+v %+v", args, failed)
	}
	if n := bundleSize(t, f, args); n > 64<<10 || args.Repos[0].Base != tip {
		t.Fatalf("restored placement's bundle: %d bytes, base %q", n, args.Repos[0].Base)
	}
	f.p.mu.Lock()
	diffBase := f.p.state.GitBases["app"]
	f.p.mu.Unlock()
	if diffBase != base {
		t.Fatalf("live diff base %q, want the clone's %s", diffBase, base)
	}
}

func gitMktree(t *testing.T, dir, entries string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "mktree")
	cmd.Stdin = strings.NewReader(entries)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
