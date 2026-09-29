package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/gitdiff"
	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// fakePodman is a podman stand-in: every call is logged; `volume inspect`
// answers a directory, `volume export` a few bytes; `run` and `exec` (a
// diff) sleep for $FAKE_DIFF_SLEEP seconds, then print the file out; `rm`
// kills the sleeping `run`, and the end of its stdin the sleeping `exec`
// (as the shim stops when its stdin ends). A `run --name N` container
// exists (`container exists`, `ps`) as the file ctr/N until `rm` removes
// it. With the file rm-fail, `rm` fails and removes nothing; with
// rm-noop, it succeeds and removes nothing; with rm-hang, it hangs; with $FAKE_RM_STUCK, it removes the container but
// the `run` goes on; with exec-linger, an `exec` lingers that many
// seconds after its stdin ends. With the file running, the Run's
// container is running (`container inspect`).
const fakePodman = `#!/bin/sh
D=$(dirname "$0")
echo "$(date +%s.%N) $*" >> "$D/log"
mkdir -p "$D/ctr"
case "$1" in
volume)
	case "$2" in
	inspect) mkdir -p "$D/vol/$5"; echo "$D/vol/$5" ;;
	export) printf 'tar' ;;
	esac ;;
container)
	[ "$2" = exists ] && { [ -f "$D/ctr/$3" ]; exit $?; }
	[ "$2" = inspect ] && [ -f "$D/running" ] && echo '[{"State":{"Running":true}}]' ;;
ps)
	ls "$D/ctr" ;;
run|exec)
	echo $$ > "$D/$1.pid"
	if [ "$1" = run ]; then
		shift; while [ $# -gt 0 ] && [ "$1" != --name ]; do shift; done
		touch "$D/ctr/$2"
		set -- run
	fi
	sleep "${FAKE_DIFF_SLEEP:-0}" &
	S=$!
	echo $S > "$D/sleep.pid"
	echo "$(date +%s.%N) $1 started" >> "$D/log"
	# A background job's stdin is /dev/null unless given another fd.
	exec 3<&0
	[ "$1" = exec ] && (cat <&3; kill $S) >/dev/null 2>&1 &
	wait $S
	[ "$1" = exec ] && [ -f "$D/exec-linger" ] && sleep "$(cat "$D/exec-linger")"
	[ -f "$D/out" ] && cat "$D/out"
	echo "$(date +%s.%N) $1 ended" >> "$D/log" ;;
rm)
	[ -f "$D/rm-hang" ] && exec sleep 3600
	if [ -f "$D/rm-fail" ]; then echo "$(date +%s.%N) rm failed" >> "$D/log"; echo "rm: device busy" >&2; exit 125; fi
	[ -f "$D/rm-noop" ] && exit 0
	[ -z "$FAKE_RM_STUCK" ] && [ -f "$D/sleep.pid" ] && kill "$(cat "$D/sleep.pid")" 2>/dev/null
	N=""; for a in "$@"; do N=$a; done
	rm -f "$D/ctr/$N"
	echo "$(date +%s.%N) rm done" >> "$D/log" ;;
esac
exit 0
`

type fakeLuxd struct {
	mu      sync.Mutex
	reports []timedFrame
	// hold delays the ack of a report of a type until its channel closes.
	hold map[string]chan struct{}
}

func (l *fakeLuxd) holdAcks(typ string) (release func()) {
	ch := make(chan struct{})
	l.mu.Lock()
	if l.hold == nil {
		l.hold = map[string]chan struct{}{}
	}
	l.hold[typ] = ch
	l.mu.Unlock()
	return sync.OnceFunc(func() { close(ch) })
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
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: &egress.Firewall{},
		git: gitws.New(data), subs: map[string]context.CancelFunc{}}
	r.conn = newConn(r)
	r.conn.polling = true
	r.snapshotDiffs.Store(true)
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
				hold := l.hold[f.Type]
				l.mu.Unlock()
				ack := proto.Frame{Type: proto.MsgAck, ID: f.ID}
				if hold != nil {
					go func() {
						select {
						case <-hold:
						case <-ctx.Done():
						}
						r.conn.dispatch(ctx, ack)
					}()
					continue
				}
				r.conn.dispatch(ctx, ack)
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
	if !strings.Contains(d.Error, "did not finish in 2s") || len(d.Diffs) != 2 || d.Diffs[0].Error == "" {
		t.Errorf("diffs: %+v", d)
	}
	if took := diffs.at.Sub(start); took > 2*time.Second+diffCleanupTimeout {
		t.Errorf("the diff ran %s past a 2s budget", took)
	}
	log := logOf(t, dir)
	if !strings.Contains(log, " run --rm --name lux-diff-r1") || !strings.Contains(log, "rm -f -t 0 lux-diff-r1") {
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
// diff is cancelled, and the new placement goes on only once its
// container is confirmed gone.
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
	r.assign(context.Background(), nestedAssign(2))
	f, _ := waitStatus(t, l, 2, 5*time.Second)
	if took := f.at.Sub(start); took > 5*time.Second {
		t.Errorf("the new placement went on after %s", took)
	}
	// Its podman run has ended, its container removed and confirmed gone,
	// before the new placement went on.
	pid, _ := os.ReadFile(filepath.Join(dir, "run.pid"))
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		t.Errorf("podman run (pid %s) still runs after the assign", pid)
	}
	if _, err := os.Stat(filepath.Join(dir, "ctr", "lux-diff-r1")); err == nil {
		t.Error("the diff container still exists")
	}
	if checked := lastLine(t, dir, "container exists lux-diff-r1"); !f.at.After(checked) {
		t.Errorf("the new placement went on at %s, before the helper was confirmed gone at %s", f.at, checked)
	}
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second))
	if !strings.Contains(d.Skipped, "superseded") || len(d.Diffs) != 2 || d.CleanupFailed != "" {
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
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		live := p.live
		p.mu.Unlock()
		if live == nil {
			return
		}
	}
	t.Error("a cancelled diff is still the placement's live one after its exec ended")
}

func setVar[T any](t *testing.T, v *T, to T) {
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusOf is the first status report for epoch, if any.
func statusOf(l *fakeLuxd, epoch int) (timedFrame, proto.Status, bool) {
	for _, f := range l.frames(proto.MsgStatus) {
		var st proto.Status
		json.Unmarshal(f.Data, &st)
		if f.Epoch == epoch && (st.State == "failed" || st.State == "exited") {
			return f, st, true
		}
	}
	return timedFrame{}, proto.Status{}, false
}

func waitStatus(t *testing.T, l *fakeLuxd, epoch int, within time.Duration) (timedFrame, proto.Status) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if f, st, ok := statusOf(l, epoch); ok {
			return f, st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no final status for epoch %d in %s", epoch, within)
	return timedFrame{}, proto.Status{}
}

// nestedAssign is epoch's assignment of r1, whose placement fails as soon
// as it is past the volumes' fence (no nested containers here).
func nestedAssign(epoch int) proto.Assign {
	return proto.Assign{RunID: "r1", TenantID: "t1", Epoch: epoch, Spec: spec.RunSpec{Sandbox: spec.Sandbox{NestedContainers: true}}}
}

// The Run resumes here while its old placement's snapshot.done still
// awaits luxd's ack: the old snapshot's diff never starts (no helper
// container is ever run), and is reported skipped.
func TestAssignBeforeTheDiffStartsSupersedesIt(t *testing.T) {
	r, l, dir := diffRunner(t)
	release := l.holdAcks(proto.MsgSnapshotDone)
	p := exitedPlacement(t, r, 1)
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	l.wait(t, proto.MsgSnapshotDone, 5*time.Second)
	r.assign(context.Background(), nestedAssign(2))
	waitStatus(t, l, 2, 5*time.Second)
	release()
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second))
	if !strings.Contains(d.Skipped, "superseded") || len(d.Diffs) != 2 || d.Diffs[0].Error == "" {
		t.Errorf("%+v", d)
	}
	if log := logOf(t, dir); strings.Contains(log, " run ") {
		t.Errorf("a diff container was started:\n%s", log)
	}
}

// The helper cannot be removed at first (podman rm fails, or says it
// succeeded while the container is still there): the new placement does
// not touch the volumes (it waits), the diff reports cleanupFailed, and
// once the sweep removes the helper the new placement goes on.
func TestResumeWaitsForTheHelperToBeGone(t *testing.T) {
	for _, mode := range []string{"rm-fail", "rm-noop"} {
		t.Run(mode, func(t *testing.T) {
			r, l, dir := diffRunner(t)
			t.Setenv("FAKE_DIFF_SLEEP", "30")
			setVar(t, &diffCleanupTimeout, time.Second)
			setVar(t, &helperSweepEvery, 200*time.Millisecond)
			p := exitedPlacement(t, r, 1)
			go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
			for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logOf(t, dir), " run started") && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			touch(t, dir, mode)
			if mode == "rm-noop" {
				// The run itself ends; only the container stays.
				pid, _ := os.ReadFile(filepath.Join(dir, "sleep.pid"))
				exec.Command("kill", strings.TrimSpace(string(pid))).Run()
			}
			r.assign(context.Background(), nestedAssign(2))
			d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 10*time.Second))
			if d.CleanupFailed == "" {
				t.Errorf("%+v", d)
			}
			time.Sleep(500 * time.Millisecond)
			if _, _, ok := statusOf(l, 2); ok {
				t.Fatal("the new placement went on while the helper still existed")
			}
			if _, err := os.Stat(filepath.Join(dir, "ctr", "lux-diff-r1")); err != nil {
				t.Fatal("the helper is gone, yet rm did nothing")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go r.helperLoop(ctx)
			os.Remove(filepath.Join(dir, mode))
			f, _ := waitStatus(t, l, 2, 5*time.Second)
			if removed := lastLine(t, dir, "rm done"); !f.at.After(removed) {
				t.Errorf("the new placement went on at %s, the helper was removed at %s", f.at, removed)
			}
		})
	}
}

// A helper that never goes (rm hangs): the new placement fails, after its
// bound, with the reason, and the volumes are never touched.
func TestResumeFailsWhenTheHelperNeverGoes(t *testing.T) {
	r, l, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "30")
	setVar(t, &diffCleanupTimeout, 500*time.Millisecond)
	setVar(t, &volumesWait, 2*time.Second)
	p := exitedPlacement(t, r, 1)
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logOf(t, dir), " run started") && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	touch(t, dir, "rm-hang")
	start := time.Now()
	r.assign(context.Background(), nestedAssign(2))
	_, st := waitStatus(t, l, 2, 15*time.Second)
	if st.State != "failed" || !strings.Contains(st.Message, "lux-diff-r1") || !strings.Contains(st.Message, "could not be removed") {
		t.Errorf("%+v", st)
	}
	if took := time.Since(start); took < 2*time.Second {
		t.Errorf("failed after %s, before its bound", took)
	}
	if d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second)); d.CleanupFailed == "" {
		t.Errorf("%+v", d)
	}
	// Released for the test's cleanup.
	os.Remove(filepath.Join(dir, "rm-hang"))
	r.sweepHelpers(context.Background())
}

// A discard while the helper cannot be removed leaves the local copy.
func TestDiscardWaitsForTheHelper(t *testing.T) {
	r, l, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "30")
	setVar(t, &diffCleanupTimeout, 500*time.Millisecond)
	setVar(t, &volumesWait, time.Second)
	p := exitedPlacement(t, r, 1)
	go p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logOf(t, dir), " run started") && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	touch(t, dir, "rm-fail")
	r.discard(context.Background(), "r1", 2)
	l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second)
	if strings.Contains(logOf(t, dir), "volume ls") {
		t.Error("the volumes were removed while the helper had them mounted")
	}
	if r.placement("r1", 0) != p {
		t.Error("the placement was forgotten")
	}
	os.Remove(filepath.Join(dir, "rm-fail"))
	r.sweepHelpers(context.Background())
	r.discard(context.Background(), "r1", 2)
	if !strings.Contains(logOf(t, dir), "volume ls") {
		t.Error("not discarded once the helper was gone")
	}
}

// The sweep removes a helper a crashed runner left behind, but never one
// a diff is running in.
func TestSweepRemovesLeftoverHelpers(t *testing.T) {
	r, _, dir := diffRunner(t)
	os.MkdirAll(filepath.Join(dir, "ctr"), 0o755)
	touch(t, dir, "ctr/lux-diff-old")
	touch(t, dir, "ctr/lux-diff-busy")
	touch(t, dir, "ctr/lux-r1")
	r.helpers.setActive("lux-diff-busy", true)
	r.sweepHelpers(context.Background())
	for name, want := range map[string]bool{"lux-diff-old": false, "lux-diff-busy": true, "lux-r1": true} {
		if _, err := os.Stat(filepath.Join(dir, "ctr", name)); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", name, err == nil, want)
		}
	}
}

// linesAt is every time the log said what.
func linesAt(t *testing.T, dir, what string) []time.Time {
	t.Helper()
	var at []time.Time
	for _, l := range strings.Split(logOf(t, dir), "\n") {
		if ts, rest, ok := strings.Cut(l, " "); ok && rest == what {
			var sec, nsec int64
			fmt.Sscanf(ts, "%d.%d", &sec, &nsec)
			at = append(at, time.Unix(sec, nsec))
		}
	}
	return at
}

// lastLine is when the log last said what.
func lastLine(t *testing.T, dir, what string) time.Time {
	t.Helper()
	at := linesAt(t, dir, what)
	if len(at) == 0 {
		t.Fatalf("the log never says %q", what)
	}
	return at[len(at)-1]
}

// A luxd that does not advertise snapshot diffs (an older release) gets
// none: no helper runs, no snapshot.diffs is sent, nothing is owed. One
// that does gets them; a runner's hello says it has diffs.
func TestSnapshotDiffsOnlyForALuxdThatTakesThem(t *testing.T) {
	r, l, dir := diffRunner(t)
	if !slices.Contains(r.hello(context.Background()).Capabilities, proto.CapDiff) {
		t.Error("the hello does not advertise diffs")
	}
	r.onWelcome(context.Background(), proto.Welcome{})
	p := exitedPlacement(t, r, 1)
	p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	l.wait(t, proto.MsgSnapshotDone, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	if n := len(l.frames(proto.MsgSnapshotDiffs)); n != 0 || strings.Contains(logOf(t, dir), " run ") {
		t.Errorf("an old luxd got %d snapshot.diffs:\n%s", n, logOf(t, dir))
	}
	if st, _ := readRunState(p.dir); st.DiffsFor != "" {
		t.Errorf("diffs owed to an old luxd: %s", st.DiffsFor)
	}

	r2, l2, dir2 := diffRunner(t)
	r2.onWelcome(context.Background(), proto.Welcome{Capabilities: []string{proto.CapSnapshotDiffs}})
	p2 := exitedPlacement(t, r2, 1)
	p2.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped"})
	l2.wait(t, proto.MsgSnapshotDiffs, 5*time.Second)
	if !strings.Contains(logOf(t, dir2), " run ") {
		t.Error("no diff container for a luxd that takes them")
	}
}

// A restarted runner owes a snapshot's diffs: it computes them once a
// welcome says luxd takes them, and once only across reconnects.
func TestOwedDiffsWaitForTheWelcome(t *testing.T) {
	r, l, dir := diffRunner(t)
	r.snapshotDiffs.Store(false)
	sp := spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "app", Path: "/workspace/repos/app"}}}}
	st := &runState{RunID: "r1", TenantID: "t1", Epoch: 1, Phase: "reported", Spec: &sp, Image: "img", User: "1000:1000",
		DiffsFor: "snap_owed", GitBases: map[string]string{"app": "abc"},
		Volumes: []volumeRef{{Name: "workspace", Volume: "lux-r1-workspace", Path: "/workspace", Kind: "state"}}}
	if err := writeRunState(r.runDir("r1"), st); err != nil {
		t.Fatal(err)
	}
	r.readopt(context.Background())
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(logOf(t, dir), " run ") {
		t.Fatal("a diff started before luxd's welcome")
	}
	w := proto.Welcome{Capabilities: []string{proto.CapSnapshotDiffs}}
	r.onWelcome(context.Background(), w)
	r.onWelcome(context.Background(), w)
	d := diffsOf(t, l.wait(t, proto.MsgSnapshotDiffs, 5*time.Second))
	if d.SnapshotID != "snap_owed" {
		t.Errorf("%+v", d)
	}
	p := r.placement("r1", 0)
	p.mu.Lock()
	done := p.diffDone
	p.mu.Unlock()
	<-done
	if n := strings.Count(logOf(t, dir), " run started"); n != 1 {
		t.Errorf("%d diff containers for one owed diff", n)
	}
}

// A cancelled live diff keeps the placement's one slot until its exec has
// ended: meanwhile a different request is busy, an identical one waits
// and then starts afresh; once it has ended, a different one runs.
func TestLiveDiffHoldsItsSlotUntilTheExecEnds(t *testing.T) {
	r, _, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	os.WriteFile(filepath.Join(dir, "exec-linger"), []byte("1"), 0o644)
	var out bytes.Buffer
	gitdiff.WriteRecord(&out, gitdiff.Diff{Stat: proto.DiffStat{Repo: "app", Kind: "clone", Files: 1}, Patch: []byte("p\n")})
	os.WriteFile(filepath.Join(dir, "out"), out.Bytes(), 0o644)
	p := exitedPlacement(t, r, 1)
	req := proto.DiffRequest{SubID: "a", Kind: "clone", Repos: []proto.DiffRepo{{Name: "app", Path: "/workspace/repos/app"}}}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.serveLiveDiff(ctx, req, func(proto.DiffResult) error { return nil })
		errc <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logOf(t, dir), " exec started") && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-errc
	cancelled := time.Now()
	other := req
	other.SubID, other.Kind = "b", "head"
	if busy, _ := p.serveLiveDiff(context.Background(), other, func(proto.DiffResult) error { return nil }); !busy {
		t.Error("a different diff ran while the cancelled one's exec still lingered")
	}
	// Identical: waits for the old exec, then a fresh one with results.
	t.Setenv("FAKE_DIFF_SLEEP", "0")
	same := req
	same.SubID = "c"
	var got []proto.DiffResult
	busy, err := p.serveLiveDiff(context.Background(), same, func(r proto.DiffResult) error { got = append(got, r); return nil })
	if busy || err != nil || len(got) != 1 || got[0].SubID != "c" {
		t.Errorf("identical after cancel: busy %v, %v, %+v", busy, err, got)
	}
	started, ended := linesAt(t, dir, "exec started"), linesAt(t, dir, "exec ended")
	if len(started) != 2 || len(ended) != 2 || !started[1].After(ended[0]) || ended[0].Before(cancelled.Add(900*time.Millisecond)) {
		t.Errorf("execs started %v, ended %v; cancelled at %v", started, ended, cancelled)
	}
	if n := strings.Count(logOf(t, dir), " exec started"); n != 2 {
		t.Errorf("%d execs", n)
	}
	if busy, _ := p.serveLiveDiff(context.Background(), other, func(proto.DiffResult) error { return nil }); busy {
		t.Error("a different diff is still busy once the slot is free")
	}
}

// A diff request's cancel takes effect whatever the order its frames are
// handled in: right after the request (it stops at once), or even before
// it (it is never served).
func TestLiveDiffCancelOrdering(t *testing.T) {
	r, _, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	touch(t, dir, "running")
	p := exitedPlacement(t, r, 1)
	req := func(sub string) proto.Frame {
		return proto.Frame{Type: proto.MsgDiffRequest, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.DiffRequest{SubID: sub, Kind: "clone",
			Repos: []proto.DiffRepo{{Name: "app", Path: "/workspace/repos/app"}}})}
	}
	cancelOf := func(sub string) proto.Frame {
		return proto.Frame{Type: proto.MsgDiffCancel, RunID: "r1", Epoch: 1, Data: proto.Marshal(proto.DiffEnd{SubID: sub})}
	}
	idle := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		return p.live == nil && len(r.subs) == 0
	}
	ctx := context.Background()
	// Cancel first: nothing is served.
	r.conn.dispatch(ctx, cancelOf("early"))
	r.conn.dispatch(ctx, req("early"))
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(logOf(t, dir), "container inspect") || !idle() {
		t.Fatalf("a request cancelled before it arrived was served:\n%s", logOf(t, dir))
	}
	// Request then cancel, back to back, many times: each ends at once.
	for i := range 20 {
		sub := fmt.Sprint("s", i)
		r.conn.dispatch(ctx, req(sub))
		r.conn.dispatch(ctx, cancelOf(sub))
		deadline := time.Now().Add(5 * time.Second)
		for !idle() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !idle() {
			t.Fatalf("request %d still served after its cancel", i)
		}
	}
}
