package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanForAStart is a fake podman that takes a placement through its
// whole start: a network, an image the host has (mounted at $0.root), a
// container. `start` waits while $0.holdstart exists and fails if
// $0.failstart does; a started container inspects as running, but no shim
// ever listens, and `wait` blocks until the test ends.
const podmanForAStart = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"network create") ;;
"network inspect") echo "lux0 10.88.0.1" ;;
"image exists") ;;
"image inspect") echo '[{"Id":"img1","Config":{"User":""}}]' ;;
"image mount") echo "$0.root" ;;
"image unmount") ;;
"volume exists") exit 1 ;;
"volume create") ;;
"container inspect")
  [ -e "$0.ctr" ] || { echo "no such container" >&2; exit 125; }
  if [ -e "$0.started" ]; then echo '[{"State":{"Status":"running","Running":true}}]'
  else echo '[{"State":{"Status":"created","Running":false}}]'; fi ;;
"rm -f") ;;
"kill -s") ;;
*)
  case "$1" in
  create) echo ctr-1 > "$0.ctr"; echo ctr-1 ;;
  start)
    while [ -e "$0.holdstart" ]; do sleep 0.02; done
    [ -e "$0.failstart" ] && { echo "start refused" >&2; exit 1; }
    touch "$0.started" ;;
  wait) while :; do sleep 0.05; done ;;
  *) exit 1 ;;
  esac ;;
esac
`

// fakeLuxd takes a placement's reports off the poll transport, in the
// order sent, and acks them; while held it acks none.
type fakeLuxd struct {
	r       *Runner
	mu      sync.Mutex
	held    bool
	frames  []proto.Frame // every report, in order
	unacked []proto.Frame
	// maxStarting: the most starting reports ever unacked at once.
	maxStarting int
}

func (f *fakeLuxd) poll(ctx context.Context) {
	for ctx.Err() == nil {
		f.r.conn.mu.Lock()
		in := f.r.conn.pollReports
		f.r.conn.pollReports = nil
		f.r.conn.mu.Unlock()
		f.mu.Lock()
		for _, fr := range in {
			f.frames = append(f.frames, fr)
			f.unacked = append(f.unacked, fr)
			n := 0
			for _, u := range f.unacked {
				if statusOf(u).State == "starting" {
					n++
				}
			}
			f.maxStarting = max(f.maxStarting, n)
		}
		var ack []proto.Frame
		if !f.held {
			ack, f.unacked = f.unacked, nil
		}
		f.mu.Unlock()
		for _, fr := range ack {
			f.r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
		}
		time.Sleep(time.Millisecond)
	}
}

// release acks every report held so far, one at a time in the order sent,
// letting the runner act on each before the next; later ones are acked at
// once.
func (f *fakeLuxd) release(ctx context.Context) {
	for {
		f.mu.Lock()
		if len(f.unacked) == 0 {
			f.held = false
			f.mu.Unlock()
			return
		}
		fr := f.unacked[0]
		f.unacked = f.unacked[1:]
		f.mu.Unlock()
		f.r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
		time.Sleep(100 * time.Millisecond)
	}
}

func (f *fakeLuxd) statuses() []proto.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.Status
	for _, fr := range f.frames {
		if fr.Type == proto.MsgStatus {
			out = append(out, statusOf(fr))
		}
	}
	return out
}

func (f *fakeLuxd) acked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames) - len(f.unacked)
}

func statusOf(fr proto.Frame) proto.Status {
	var st proto.Status
	if fr.Type == proto.MsgStatus {
		_ = json.Unmarshal(fr.Data, &st)
	}
	return st
}

// startFixture runs a placement of run1 against podmanForAStart and a
// fakeLuxd, held from the start when hold; flags are files made next to
// the fake podman (holdstart, failstart) before it runs.
func startFixture(t *testing.T, hold bool, flags ...string) (*fakeLuxd, string, context.Context) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanForAStart), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"rt", "podman.root", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, fl := range flags {
		if err := os.WriteFile(bin+"."+fl, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{cfg: Config{DataDir: dir, Shim: "/bin/true"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded(),
		images: newImageUse(dir)}
	r.conn = newConn(r)
	r.conn.polling = true
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))

	luxd := &fakeLuxd{r: r, held: hold}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { luxd.poll(ctx) })

	sp := spec.RunSpec{Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true}}
	p := newPlacement(r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1, Spec: sp}, nil)
	r.placements["run1"] = p
	go p.run(ctx)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		select {
		case <-p.done:
		case <-time.After(45 * time.Second):
			t.Error("the placement did not end")
		}
	})
	return luxd, bin, ctx
}

// awaitStart polls cond for up to 10 s.
func awaitStart(t *testing.T, what string, cond func() bool, detail func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out; %s", what, detail())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func podmanCalls(bin string) []string {
	b, _ := os.ReadFile(bin + ".log")
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func calledFirst(bin, verb string) bool {
	return slices.ContainsFunc(podmanCalls(bin), func(l string) bool { return strings.HasPrefix(l, verb+" ") || l == verb })
}

// Each start phase reaches luxd as a starting status while the start is
// still under way, with every mark so far, and the start never waits for
// luxd: with no report acked, it still gets as far as creating and
// starting the container. One starting report is in flight at a time, and
// each carries the marks of the one before it.
func TestStartPhasesAreReportedAsTheyHappen(t *testing.T) {
	luxd, bin, ctx := startFixture(t, true, "holdstart")
	detail := func() string { return strings.Join(podmanCalls(bin), "; ") }

	// luxd answers nothing: the start goes on regardless.
	awaitStart(t, "podman create and start with no report acked", func() bool {
		return calledFirst(bin, "create") && calledFirst(bin, "start")
	}, detail)
	if n := luxd.acked(); n != 0 {
		t.Fatalf("%d reports acked while luxd was held", n)
	}
	// Every phase so far has asked for a report; one is in flight.
	luxd.mu.Lock()
	inFlight := luxd.maxStarting
	luxd.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("%d starting reports unacked at once; want one in flight at a time", inFlight)
	}

	luxd.release(ctx)
	waitMark := func(mark string) {
		t.Helper()
		awaitStart(t, "a starting report with "+mark, func() bool {
			return slices.ContainsFunc(luxd.statuses(), func(st proto.Status) bool { return st.State == "starting" && st.Times[mark] != 0 })
		}, func() string { return detail() })
	}
	// The container's start is held: everything before it is reported.
	waitMark("reposReady")
	for _, st := range luxd.statuses() {
		if st.State != "starting" || st.Times["containerStarted"] != 0 {
			t.Fatalf("while the container's start is held: %+v", st)
		}
	}
	all := luxd.statuses()
	got := all[len(all)-1].Times
	if got["imageReady"] == 0 || got["volumesRestored"] == 0 ||
		got["imageReady"] > got["volumesRestored"] || got["volumesRestored"] > got["reposReady"] {
		t.Fatalf("marks %v: want imageReady <= volumesRestored <= reposReady", got)
	}

	if err := os.Remove(bin + ".holdstart"); err != nil {
		t.Fatal(err)
	}
	// Started: reported while the container runs (no shim answers).
	waitMark("containerStarted")
	all = luxd.statuses()
	for i, st := range all {
		if st.State != "starting" {
			t.Fatalf("report %d: %+v; want only starting reports before the shim answers", i, st)
		}
		if i == 0 {
			continue
		}
		for k, v := range all[i-1].Times {
			if st.Times[k] != v {
				t.Fatalf("report %d %v drops or changes %s of report %d %v", i, st.Times, k, i-1, all[i-1].Times)
			}
		}
	}
	if len(all) < 2 {
		t.Fatalf("reports %+v: want the phases reported apart", all)
	}
}

// A start whose shim answers reports running with every mark, and sends
// no starting report after it.
func TestNoStartingReportAfterRunning(t *testing.T) {
	luxd, bin, ctx := startFixture(t, true)
	sock, err := net.Listen("unix", filepath.Join(filepath.Dir(bin), "rt", "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	go func() {
		for {
			c, err := sock.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	awaitStart(t, "the running report", func() bool {
		return slices.ContainsFunc(luxd.statuses(), func(st proto.Status) bool { return st.State == "running" })
	}, func() string { return strings.Join(podmanCalls(bin), "; ") })
	luxd.release(ctx)
	time.Sleep(500 * time.Millisecond)
	all := luxd.statuses()
	run := slices.IndexFunc(all, func(st proto.Status) bool { return st.State == "running" })
	for i, st := range all[run+1:] {
		if st.State == "starting" {
			t.Fatalf("report %d after the running one is starting: %+v", run+1+i, all)
		}
	}
	if all[run].Times["containerStarted"] == 0 {
		t.Fatalf("running report %+v: want it to carry containerStarted", all[run])
	}
}

// A start that fails sends no starting report after its end report: one
// in flight is abandoned, and a mark still due is not sent.
func TestNoStartingReportAfterAFailedStart(t *testing.T) {
	luxd, bin, ctx := startFixture(t, true, "failstart")
	awaitStart(t, "the failed report", func() bool {
		return slices.ContainsFunc(luxd.statuses(), func(st proto.Status) bool { return st.State == "failed" })
	}, func() string { return strings.Join(podmanCalls(bin), "; ") })
	// luxd now answers everything, in order: a starting report the runner
	// still wanted to send would follow.
	luxd.release(ctx)
	time.Sleep(500 * time.Millisecond)
	all := luxd.statuses()
	end := slices.IndexFunc(all, func(st proto.Status) bool { return st.State == "failed" })
	for i, st := range all[end+1:] {
		if st.State == "starting" {
			t.Fatalf("report %d after the failed one is starting: %+v", end+1+i, all)
		}
	}
	if all[end].Times["reposReady"] == 0 {
		t.Fatalf("failed report %+v: want it to carry every mark", all[end])
	}
}
