package runner

import (
	"bytes"
	"context"
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
	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// fakePodman is a podman stand-in: every call is logged. `exec` (a diff)
// records its arguments in exec.args, sleeps $FAKE_DIFF_SLEEP seconds,
// then prints the file out; the end of its stdin kills the sleep (as the
// shim stops when its stdin ends). With the file exec-linger, it lingers
// that many seconds after its stdin ends; with exec-ignore-eof, it ignores
// its stdin and runs until signalled. With the file running, the Run's
// container is running (`container inspect`).
const fakePodman = `#!/bin/sh
D=$(dirname "$0")
echo "$(date +%s.%N) $*" >> "$D/log"
case "$1" in
container)
	[ "$2" = inspect ] && [ -f "$D/running" ] && echo '[{"State":{"Running":true}}]' ;;
exec)
	echo $$ > "$D/exec.pid"
	if [ -f "$D/exec-ignore-eof" ]; then
		# A shim that ignores the end of its stdin: only a signal ends it.
		sleep 60 >/dev/null 2>&1 &
		echo $! > "$D/sleep.pid"
		echo "$(date +%s.%N) exec started" >> "$D/log"
		wait $!
		echo "$(date +%s.%N) exec ended" >> "$D/log"
		exit 0
	fi
	printf '%s\n' "$@" > "$D/exec.args"
	sleep "${FAKE_DIFF_SLEEP:-0}" &
	S=$!
	echo "$(date +%s.%N) exec started" >> "$D/log"
	# A background job's stdin is /dev/null unless given another fd.
	exec 3<&0
	(cat <&3; kill $S) >/dev/null 2>&1 &
	wait $S
	[ -f "$D/exec-linger" ] && sleep "$(cat "$D/exec-linger")"
	[ -f "$D/out" ] && cat "$D/out"
	echo "$(date +%s.%N) exec ended" >> "$D/log" ;;
esac
exit 0
`

// diffRunner is a runner whose podman is fakePodman, over the polling
// transport (frames it sends are queued, never delivered).
func diffRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(fakePodman), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	os.MkdirAll(filepath.Join(data, "runs"), 0o700)
	r := &Runner{cfg: Config{DataDir: data, Shim: "/shim"}, log: slog.New(slog.DiscardHandler),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: &egress.Firewall{},
		git: gitws.New(data), subs: map[string]context.CancelFunc{}}
	r.conn = newConn(r)
	r.conn.polling = true
	return r, dir
}

// runningPlacement is Run r1's placement on r, with one repository cloned
// at abc, whose workload runs as 1000:1000.
func runningPlacement(t *testing.T, r *Runner) *placement {
	sp := spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "app", Path: "/workspace/repos/app"}}}}
	p := &placement{r: r, runID: "r1", tenantID: "t1", epoch: 1, dir: r.runDir("r1"), phase: "running", done: make(chan struct{}),
		assign: &proto.Assign{RunID: "r1", TenantID: "t1", Epoch: 1, Spec: sp},
		state: &runState{RunID: "r1", TenantID: "t1", Epoch: 1, Spec: &sp, User: "1000:1000",
			GitBases: map[string]string{"app": "abc"}}}
	os.MkdirAll(p.dir, 0o700)
	r.mu.Lock()
	r.placements["r1"] = p
	r.mu.Unlock()
	return p
}

func logOf(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return string(b)
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeOut(t *testing.T, dir string, diffs ...gitdiff.Diff) {
	t.Helper()
	var out bytes.Buffer
	for _, d := range diffs {
		if err := gitdiff.WriteRecord(&out, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linesAt are the times of the log lines ending in what.
func linesAt(t *testing.T, dir, what string) []time.Time {
	var out []time.Time
	for l := range strings.SplitSeq(logOf(t, dir), "\n") {
		ts, rest, ok := strings.Cut(l, " ")
		if !ok || rest != what {
			continue
		}
		var sec, nsec int64
		fmt.Sscanf(ts, "%d.%d", &sec, &nsec)
		out = append(out, time.Unix(sec, nsec))
	}
	return out
}

// Live diffs: one at a time per placement. Two identical requests share
// one exec, each getting every result; a different one meanwhile is busy;
// the exec stops (its stdin closed) once the last request is cancelled.
func TestLiveDiffsCoalesceAndCancel(t *testing.T) {
	r, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "1")
	writeOut(t, dir, gitdiff.Diff{Stat: proto.DiffStat{Repo: "app", Kind: "clone", Files: 1}, Patch: []byte("p\n")})
	p := runningPlacement(t, r)
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
	// The shim ran in the Run's container, as the workload user.
	args, _ := os.ReadFile(filepath.Join(dir, "exec.args"))
	if !strings.HasPrefix(string(args), "exec\n-i\n--user\n1000:1000\n--workdir\n/\nlux-r1\n"+proto.ShimBinary+"\ndiff\n") {
		t.Errorf("exec args:\n%s", args)
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

// A cancelled live diff keeps the placement's one slot until its exec has
// ended: meanwhile a different request is busy, an identical one waits
// and then starts afresh; once it has ended, a different one runs.
func TestLiveDiffHoldsItsSlotUntilTheExecEnds(t *testing.T) {
	r, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	os.WriteFile(filepath.Join(dir, "exec-linger"), []byte("1"), 0o644)
	writeOut(t, dir, gitdiff.Diff{Stat: proto.DiffStat{Repo: "app", Kind: "clone", Files: 1}, Patch: []byte("p\n")})
	p := runningPlacement(t, r)
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
	if busy, _ := p.serveLiveDiff(context.Background(), other, func(proto.DiffResult) error { return nil }); busy {
		t.Error("a different diff is still busy once the slot is free")
	}
}

// A diff request's cancel takes effect whatever the order its frames are
// handled in: right after the request (it stops at once), or even before
// it (it is never served).
func TestLiveDiffCancelOrdering(t *testing.T) {
	r, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "60")
	touch(t, dir, "running")
	p := runningPlacement(t, r)
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

// A diff request for a placement whose container is not running is
// answered NotRunning without an exec; one for a running container gets
// its results, with the base the placement cloned at when luxd sent none.
func TestDiffRequestNotRunningAndBases(t *testing.T) {
	r, dir := diffRunner(t)
	writeOut(t, dir, gitdiff.Diff{Stat: proto.DiffStat{Repo: "app", Kind: "clone", Base: "abc", Files: 1}, Patch: []byte("p\n")})
	runningPlacement(t, r)
	var got []proto.DiffResult
	answer := func(sub string) proto.DiffEnd {
		req := proto.DiffRequest{SubID: sub, Kind: "clone", Repos: []proto.DiffRepo{{Name: "app", Path: "/workspace/repos/app"}}}
		return r.answerDiff(context.Background(), "r1", 1, req, func(res proto.DiffResult) error { got = append(got, res); return nil })
	}
	if e := answer("stopped"); !e.NotRunning || len(got) != 0 || strings.Contains(logOf(t, dir), " exec") {
		t.Fatalf("not running: %+v\n%s", e, logOf(t, dir))
	}
	touch(t, dir, "running")
	if e := answer("running"); e.NotRunning || e.Error != "" || e.Busy || len(got) != 1 || got[0].SubID != "running" {
		t.Fatalf("running: %+v %+v", e, got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "exec.args"))
	if !strings.Contains(string(args), `"base":"abc"`) {
		t.Errorf("the placement's clone base was not passed: %s", args)
	}
	// Another epoch's request: not this placement's container.
	if e := r.answerDiff(context.Background(), "r1", 2, proto.DiffRequest{SubID: "x", Kind: "clone"}, nil); !e.NotRunning {
		t.Errorf("another epoch: %+v", e)
	}
}

// What a live diff keeps for its sharers is bounded by the diff's budget,
// whatever the shim prints: three identical requests share one exec, and
// the records past the budget reach them as totals only.
func TestLiveDiffRetainsAtMostTheBudget(t *testing.T) {
	r, dir := diffRunner(t)
	t.Setenv("FAKE_DIFF_SLEEP", "1")
	var diffs []gitdiff.Diff
	var repos []proto.DiffRepo
	for i := range 4 {
		name := fmt.Sprint("r", i)
		diffs = append(diffs, gitdiff.Diff{Stat: proto.DiffStat{Repo: name, Kind: "clone", Files: 1, Insertions: 3}, Patch: bytes.Repeat([]byte("p"), 10<<20)})
		repos = append(repos, proto.DiffRepo{Name: name, Path: "/workspace/repos/" + name})
	}
	writeOut(t, dir, diffs...)
	p := runningPlacement(t, r)
	req := proto.DiffRequest{Kind: "clone", Repos: repos}
	var mu sync.Mutex
	var peak int64
	got := make([][]proto.DiffResult, 3)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			rq := req
			rq.SubID = fmt.Sprint("sub", i)
			if _, err := p.serveLiveDiff(context.Background(), rq, func(res proto.DiffResult) error {
				p.mu.Lock()
				l := p.live
				p.mu.Unlock()
				if l != nil {
					l.mu.Lock()
					mu.Lock()
					peak = max(peak, l.retained)
					mu.Unlock()
					l.mu.Unlock()
				}
				got[i] = append(got[i], res)
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := strings.Count(logOf(t, dir), " exec started"); n != 1 {
		t.Errorf("%d execs for identical requests", n)
	}
	if peak == 0 || peak > proto.DiffBudget {
		t.Errorf("retained %d bytes (budget %d)", peak, proto.DiffBudget)
	}
	for i, g := range got {
		if len(g) != 4 || len(g[2].Patch) != 10<<20 || g[3].Patch != nil || g[3].Stat.Error != proto.DiffBudgetExceeded ||
			!g[3].Stat.Truncated || g[3].Stat.Files != 1 || g[3].Stat.Insertions != 3 {
			t.Errorf("request %d: %d results", i, len(g))
		}
	}
}

// A request that will be busy is answered without spawning anything:
// podman inspect included.
func TestBusyDiffSpawnsNothing(t *testing.T) {
	r, dir := diffRunner(t)
	touch(t, dir, "running")
	p := runningPlacement(t, r)
	p.mu.Lock()
	p.live = &liveRun{key: "another diff", changed: make(chan struct{}), exited: make(chan struct{})}
	p.mu.Unlock()
	req := proto.DiffRequest{SubID: "b", Kind: "clone", Repos: []proto.DiffRepo{{Name: "app", Path: "/workspace/repos/app"}}}
	if e := r.answerDiff(context.Background(), "r1", 1, req, nil); !e.Busy {
		t.Fatalf("not busy: %+v", e)
	}
	if l := logOf(t, dir); l != "" {
		t.Errorf("a busy request ran podman:\n%s", l)
	}
}

// When the last request cancels, the exec's stdin closes at once and
// podman is signalled diffExecGrace later if the shim has not ended (this
// one ignores its stdin). The slot is held until the exec has exited.
func TestCancelledDiffIsSignalledAfterAGrace(t *testing.T) {
	r, dir := diffRunner(t)
	touch(t, dir, "exec-ignore-eof")
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(dir, "sleep.pid")); err == nil {
			var pid int
			fmt.Sscan(string(b), &pid)
			if proc, err := os.FindProcess(pid); err == nil && pid > 0 {
				proc.Kill()
			}
		}
	})
	p := runningPlacement(t, r)
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
	pid, _ := os.ReadFile(filepath.Join(dir, "exec.pid"))
	cancel()
	cancelled := time.Now()
	<-errc
	time.Sleep(diffExecGrace / 2)
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err != nil {
		t.Error("the exec was signalled before its grace ran out")
	}
	other := req
	other.Kind = "head"
	if busy, _ := p.serveLiveDiff(context.Background(), other, func(proto.DiffResult) error { return nil }); !busy {
		t.Error("the slot was released while the exec still ran")
	}
	var gone time.Time
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		live := p.live
		p.mu.Unlock()
		if live == nil {
			gone = time.Now()
			break
		}
	}
	if gone.IsZero() || gone.Sub(cancelled) > diffExecGrace+3*time.Second {
		t.Fatalf("the slot was not released after the grace (cancelled %v, released %v)", cancelled, gone)
	}
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		t.Error("the slot was released while the exec runs")
	}
}
