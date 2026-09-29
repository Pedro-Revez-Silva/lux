package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/gitdiff"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// fakePodman is a podman stand-in: every call is logged; `volume inspect`
// answers a directory, `volume export` a few bytes; `run` and `exec` (a
// diff) sleep for $FAKE_DIFF_SLEEP seconds, then print the file out; `rm`
// kills the sleeping `run`, and the end of its stdin the sleeping `exec`
// (as the shim stops when its stdin ends).
const fakePodman = `#!/bin/sh
D=$(dirname "$0")
echo "$(date +%s.%N) $*" >> "$D/log"
case "$1" in
volume)
	case "$2" in
	inspect) mkdir -p "$D/vol/$5"; echo "$D/vol/$5" ;;
	export) printf 'tar' ;;
	esac ;;
run|exec)
	echo $$ > "$D/$1.pid"
	sleep "${FAKE_DIFF_SLEEP:-0}" &
	S=$!
	echo $S > "$D/sleep.pid"
	echo "$(date +%s.%N) $1 started" >> "$D/log"
	# A background job's stdin is /dev/null unless given another fd.
	exec 3<&0
	[ "$1" = exec ] && (cat <&3; kill $S) >/dev/null 2>&1 &
	wait $S
	[ -f "$D/out" ] && cat "$D/out"
	echo "$(date +%s.%N) $1 ended" >> "$D/log" ;;
rm)
	[ -z "$FAKE_RM_STUCK" ] && [ -f "$D/sleep.pid" ] && kill "$(cat "$D/sleep.pid")" 2>/dev/null
	echo "$(date +%s.%N) rm done" >> "$D/log" ;;
esac
exit 0
`

type fakeLuxd struct {
	mu      sync.Mutex
	reports []timedFrame
}

type timedFrame struct {
	at time.Time
	proto.Frame
}

func (l *fakeLuxd) frames(typ string) []timedFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []timedFrame
	for _, f := range l.reports {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func (l *fakeLuxd) wait(t *testing.T, typ string, within time.Duration) timedFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if f := l.frames(typ); len(f) > 0 {
			return f[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s report in %s", typ, within)
	return timedFrame{}
}

// diffRunner is a runner whose podman is fakePodman and whose luxd acks
// every report (over the polling transport, with no network).
func diffRunner(t *testing.T) (*Runner, *fakeLuxd, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(fakePodman), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	for _, d := range []string{"runs", "snapshots"} {
		os.MkdirAll(filepath.Join(data, d), 0o700)
	}
	r := &Runner{cfg: Config{DataDir: data, Shim: "/shim"}, log: slog.New(slog.DiscardHandler),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: &egress.Firewall{}}
	r.conn = newConn(r)
	r.conn.polling = true
	r.uploads = newUploader(r)
	l := &fakeLuxd{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			fs := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, f := range fs {
				l.mu.Lock()
				l.reports = append(l.reports, timedFrame{time.Now(), f})
				l.mu.Unlock()
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: f.ID})
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return r, l, dir
}

// exitedPlacement is a Run's placement on r whose container has exited,
// with one repository.
func exitedPlacement(t *testing.T, r *Runner, epoch int) *placement {
	sp := spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "app", Path: "/workspace/repos/app"}}}}
	p := &placement{r: r, runID: "r1", tenantID: "t1", epoch: epoch, dir: r.runDir("r1"), phase: "exited", done: make(chan struct{}),
		assign: &proto.Assign{RunID: "r1", TenantID: "t1", Epoch: epoch, Spec: sp},
		state: &runState{RunID: "r1", TenantID: "t1", Epoch: epoch, Spec: &sp, Image: "img", User: "1000:1000",
			GitBases: map[string]string{"app": "abc"},
			Volumes:  []volumeRef{{Name: "workspace", Volume: "lux-r1-workspace", Path: "/workspace", Kind: "state"}}}}
	os.MkdirAll(p.dir, 0o700)
	r.mu.Lock()
	r.placements["r1"] = p
	r.mu.Unlock()
	// The diff's goroutine writes the state file last: let it finish.
	t.Cleanup(func() {
		p.mu.Lock()
		done := p.diffDone
		p.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the snapshot's diff never ended")
			}
		}
	})
	return p
}

func logOf(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return string(b)
}

func diffsOf(t *testing.T, f timedFrame) proto.SnapshotDiffs {
	t.Helper()
	var d proto.SnapshotDiffs
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func setDiffTimeout(t *testing.T, d time.Duration) {
	old := diffTimeout
	diffTimeout = d
	t.Cleanup(func() { diffTimeout = old })
}

// A slow diff holds up nothing: the snapshot and the exit are reported
// while it runs; its container is removed at its deadline, and the diff is
// reported then, failed.
func TestSlowDiffDoesNotDelaySnapshot(t *testing.T) {
	r, l, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "30")
	setDiffTimeout(t, 2*time.Second)
	p := exitedPlacement(t, r, 1)
	start := time.Now()
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})

	done := l.wait(t, proto.MsgSnapshotDone, 5*time.Second)
	for {
		if st := l.frames(proto.MsgStatus); len(st) > 0 {
			if took := st[0].at.Sub(start); took > time.Second {
				t.Errorf("exited reported %s after the start", took)
			}
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("no status report")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if took := done.at.Sub(start); took > time.Second {
		t.Errorf("snapshot.done reported %s after the start", took)
	}
	diffs := l.wait(t, proto.MsgSnapshotDiffs, 10*time.Second)
	if !diffs.at.After(done.at) {
		t.Error("the diffs came before the snapshot")
	}
	d := diffsOf(t, diffs)
	if d.Error == "" || len(d.Diffs) != 2 || d.Diffs[0].Error == "" {
		t.Errorf("diffs: %+v", d)
	}
	if took := diffs.at.Sub(start); took > 2*time.Second+diffCleanupTimeout {
		t.Errorf("the diff ran %s past a 2s budget", took)
	}
	log := logOf(t, dir)
	if !strings.Contains(log, " run --rm --name lux-r1-diff") || !strings.Contains(log, "rm -f -t 0 lux-r1-diff") {
		t.Errorf("podman calls:\n%s", log)
	}
	// Read-only: the state volume, the root; a tmpfs for git's files.
	if !strings.Contains(log, "lux-r1-workspace:/workspace:ro,idmap,nocopy") || !strings.Contains(log, "--read-only --tmpfs /tmp:") {
		t.Errorf("not read-only:\n%s", log)
	}
}

// An evicted host with too little time left skips the diffs: its uploads
// come first, and luxd hears why.
func TestDrainWithShortBudgetSkipsDiffs(t *testing.T) {
	r, l, dir := diffRunner(t)
	r.evictBy.Store(&evicting{at: time.Now().Add(25 * time.Second), reason: "spot"})
	p := exitedPlacement(t, r, 1)
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	l.wait(t, proto.MsgSnapshotDone, 5*time.Second)
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second))
	if !strings.Contains(d.Skipped, "going away") || len(d.Diffs) != 2 || d.Diffs[0].Error == "" {
		t.Errorf("%+v", d)
	}
	if strings.Contains(logOf(t, dir), " run ") {
		t.Error("a diff container was started")
	}
	// With more time, it runs, bounded by what is left less the margin.
	if b, skip := r.diffBudget(); skip == "" || b != 0 {
		t.Errorf("25s left: %v %q", b, skip)
	}
	r.evictBy.Store(&evicting{at: time.Now().Add(50 * time.Second)})
	if b, skip := r.diffBudget(); skip != "" || b > 20*time.Second || b < 19*time.Second {
		t.Errorf("50s left: %v %q", b, skip)
	}
	r.evictBy.Store(nil)
	if b, skip := r.diffBudget(); skip != "" || b != diffTimeout {
		t.Errorf("not evicting: %v %q", b, skip)
	}
}

// The Run resumes on this host while its last snapshot's diff runs: the
// diff is cancelled, its container gone, before the new placement starts.
func TestResumeOnTheSameHostCancelsTheDiff(t *testing.T) {
	r, l, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "30")
	p := exitedPlacement(t, r, 1)
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	l.wait(t, proto.MsgSnapshotDone, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logOf(t, dir), " run started") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	// A new epoch whose placement fails at once (no nested containers
	// here), so nothing else runs.
	a := proto.Assign{RunID: "r1", TenantID: "t1", Epoch: 2, Spec: spec.RunSpec{Sandbox: spec.Sandbox{NestedContainers: true}}}
	r.assign(context.Background(), a)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("assign took %s", took)
	}
	// Its podman run has ended, its container removed.
	pid, _ := os.ReadFile(filepath.Join(dir, "run.pid"))
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		t.Errorf("podman run (pid %s) still runs after the assign", pid)
	}
	if log := logOf(t, dir); strings.Count(log, "rm -f -t 0 lux-r1-diff") < 2 {
		t.Fatalf("the diff container was not removed:\n%s", log)
	}
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second))
	if !strings.Contains(d.Skipped, "superseded") || len(d.Diffs) != 2 {
		t.Errorf("%+v", d)
	}
}

// A podman that does not end when its container is removed still does not
// hold the diff (or a resume waiting for it) past its budget and cleanup.
func TestStuckPodmanIsBounded(t *testing.T) {
	r, l, _ := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	t.Setenv("FAKE_RM_STUCK", "1")
	setDiffTimeout(t, time.Second)
	old := diffCleanupTimeout
	diffCleanupTimeout = time.Second
	t.Cleanup(func() { diffCleanupTimeout = old })
	p := exitedPlacement(t, r, 1)
	start := time.Now()
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 20*time.Second))
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the diff ended after %s (budget 1s, cleanup 1s)", took)
	}
	if d.Error == "" {
		t.Errorf("%+v", d)
	}
}

// Live diffs: one at a time per placement. Two identical requests share
// one exec, each getting every result; a different one meanwhile is busy;
// the exec stops (its stdin closed) once the last request is cancelled.
func TestLiveDiffsCoalesceAndCancel(t *testing.T) {
	r, _, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "1")
	var out bytes.Buffer
	gitdiff.WriteRecord(&out, gitdiff.Diff{Stat: proto.DiffStat{Repo: "app", Kind: "clone", Files: 1}, Patch: []byte("p\n")})
	os.WriteFile(filepath.Join(dir, "out"), out.Bytes(), 0o644)
	p := exitedPlacement(t, r, 1)
	req := proto.DiffRequest{Kind: "clone", Repos: []proto.DiffRepo{{Name: "app", Path: "/workspace/repos/app"}}}

	var wg sync.WaitGroup
	got := make([][]proto.DiffResult, 2)
	for i := range 2 {
		wg.Go(func() {
			rq := req
			rq.SubID = fmt.Sprint("sub", i)
			busy, err := p.serveLiveDiff(context.Background(), rq, func(res proto.DiffResult) error {
				got[i] = append(got[i], res)
				return nil
			})
			if busy || err != nil {
				t.Errorf("request %d: busy %v, %v", i, busy, err)
			}
		})
	}
	// A different request while they run.
	time.Sleep(200 * time.Millisecond)
	other := req
	other.Kind = "head"
	if busy, _ := p.serveLiveDiff(context.Background(), other, func(proto.DiffResult) error { return nil }); !busy {
		t.Error("a different live diff ran alongside")
	}
	wg.Wait()
	if n := strings.Count(logOf(t, dir), " exec started"); n != 1 {
		t.Errorf("%d execs for two identical requests", n)
	}
	for i, g := range got {
		if len(g) != 1 || g[0].SubID != fmt.Sprint("sub", i) || string(g[0].Patch) != "p\n" {
			t.Errorf("request %d got %+v", i, g)
		}
	}

	// Cancelled: its exec ends, long before its sleep would.
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.serveLiveDiff(ctx, req, func(proto.DiffResult) error { return nil })
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logOf(t, dir), " exec started") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	pid, _ := os.ReadFile(filepath.Join(dir, "exec.pid"))
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled request: %v", err)
	}
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		t.Error("the exec still runs after its request was cancelled")
	}
	// The next request starts afresh.
	p.mu.Lock()
	live := p.live
	p.mu.Unlock()
	if live != nil {
		t.Error("a cancelled diff is still the placement's live one")
	}
}
