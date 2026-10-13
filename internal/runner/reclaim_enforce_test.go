package runner

import (
	"bufio"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanForReclaim is a fake podman for a spot placement's reclamation.
// Every call is logged to $0.log. `kill -s SIG X` appends "SIG X" to
// $0.signals, and KILL makes the container dead ($0.dead): `wait` returns
// 137 then, and `container inspect` says exited (running before). The
// start path is podmanForAStart's: `start` waits while $0.holdstart exists
// (touching $0.starting), and `image exists` while $0.holdimage does
// (touching $0.imaging).
const podmanForReclaim = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"kill -s") echo "$3 $4" >> "$0.signals"; [ "$3" = KILL ] && touch "$0.dead" ;;
"container inspect")
  [ "$3" = "--size" ] && exit 1
  [ -e "$0.ctr" ] || { echo "no such container" >&2; exit 125; }
  if [ -e "$0.dead" ]; then echo '[{"State":{"Status":"exited","Running":false}}]'
  elif [ -e "$0.started" ]; then echo '[{"State":{"Status":"running","Running":true}}]'
  else echo '[{"State":{"Status":"created","Running":false}}]'; fi ;;
"network create") ;;
"network inspect") echo "lux0 10.88.0.1" ;;
"image exists")
  if [ -e "$0.holdimage" ]; then touch "$0.imaging"; while [ -e "$0.holdimage" ]; do sleep 0.02; done; fi ;;
"image inspect") echo '[{"Id":"img1","Config":{"User":""}}]' ;;
"image mount") echo "$0.root" ;;
"image unmount") ;;
"volume exists") exit 1 ;;
"volume create") ;;
"volume rm") ;;
"rm -f") ;;
*)
  case "$1" in
  create) echo ctr-1 > "$0.ctr"; echo ctr-1 ;;
  start)
    touch "$0.starting"
    while [ -e "$0.holdstart" ]; do sleep 0.02; done
    touch "$0.started" ;;
  wait) while [ ! -e "$0.dead" ]; do sleep 0.02; done; echo 137 ;;
  *) exit 1 ;;
  esac ;;
esac
`

// shimMsg is a message the fake shim got, with the run state on disk at
// that moment.
type shimMsg struct {
	m     proto.ShimMsg
	at    time.Time
	saved *runState
}

// fakeShim listens on a placement's shim socket and records what it is
// sent; it answers every message OK.
type fakeShim struct {
	mu   sync.Mutex
	msgs []shimMsg
}

func listenShim(t *testing.T, sock, runDir string) *fakeShim {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeShim{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				enc := json.NewEncoder(c)
				for sc.Scan() {
					var m proto.ShimMsg
					_ = json.Unmarshal(sc.Bytes(), &m)
					saved, _ := readRunState(runDir)
					s.mu.Lock()
					s.msgs = append(s.msgs, shimMsg{m: m, at: time.Now(), saved: saved})
					s.mu.Unlock()
					_ = enc.Encode(proto.ShimMsg{Type: m.Type, OK: true})
				}
			}()
		}
	}()
	return s
}

func (s *fakeShim) got() []shimMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

func (s *fakeShim) stops() []shimMsg {
	var out []shimMsg
	for _, m := range s.got() {
		if m.m.Type == proto.ShimStop {
			out = append(out, m)
		}
	}
	return out
}

// shortReclaim makes the reclamation's grace d, and its clock now plus
// whatever *offset holds, for the test.
func shortReclaim(t *testing.T, d time.Duration) *atomic.Int64 {
	t.Helper()
	var offset atomic.Int64
	oldGrace, oldClock := reclaimGrace, reclaimClock
	reclaimGrace = d
	reclaimClock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { reclaimGrace, reclaimClock = oldGrace, oldClock })
	return &offset
}

// reclaimRig is a runner with podmanForReclaim, a fakeLuxd (held: it acks
// nothing until released) and run1's runtime volume at rt.
type reclaimRig struct {
	r    *Runner
	bin  string
	rt   string
	luxd *fakeLuxd
	ctx  context.Context
}

func newReclaimRig(t *testing.T, held bool, flags ...string) *reclaimRig {
	t.Helper()
	// Not t.TempDir: it embeds the test name, and rt/shim.sock must fit a
	// unix socket path (108 bytes on Linux, 104 on macOS). Registered
	// first, so it is removed last: after ctx is cancelled and the fake
	// podman's processes are done.
	dir, err := os.MkdirTemp("", "lux-rc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanForReclaim), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"rt", "podman.root", "runs", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, fl := range flags {
		writeFile(t, bin+"."+fl, "")
	}
	r := &Runner{cfg: Config{DataDir: dir, Shim: "/bin/true"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded(),
		images: newImageUse(dir)}
	r.conn = newConn(r)
	r.conn.polling = true
	r.uploads = newUploader(r)
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))
	luxd := &fakeLuxd{r: r, held: held}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { luxd.poll(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return &reclaimRig{r: r, bin: bin, rt: filepath.Join(dir, "rt"), luxd: luxd, ctx: ctx}
}

func (g *reclaimRig) signals() []string {
	b, _ := os.ReadFile(g.bin + ".signals")
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ctr-1", ""))
}

// killed: the fake podman has finished a KILL (its last write is $0.dead),
// so nothing of it still writes in the test's directory.
func (g *reclaimRig) killed() bool {
	_, err := os.Stat(g.bin + ".dead")
	return err == nil && slices.Contains(g.signals(), "KILL")
}

// running is a running spot placement of run1 in container ctr-1 (the
// fake's: alive until killed), its run state saved.
func (g *reclaimRig) running(t *testing.T) *placement {
	t.Helper()
	writeFile(t, g.bin+".ctr", "ctr-1")
	writeFile(t, g.bin+".started", "")
	sp := spec.RunSpec{Spot: true, ResumePolicy: spec.ResumeNever}
	sp.Workload.Grace.Duration = time.Minute
	p := newPlacement(g.r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1, Spec: sp}, nil)
	p.phase = "running"
	p.state = &runState{RunID: "run1", TenantID: "t1", Epoch: 1, Phase: "started", Container: "ctr-1", Spec: &sp}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.saveState(); err != nil {
		t.Fatal(err)
	}
	g.r.placements["run1"] = p
	return p
}

// eventually polls cond for up to 5s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// The first notice persists the kill instant (receipt + grace) before the
// shim is told; the shim gets it as KillAtUnixMs. A second notice 5s later
// sends the same instant, leaves the stored one alone and arms no second
// kill: the container is killed once, at the first deadline.
func TestReclaimNoticeIsPersistedFirstAndNeverMoves(t *testing.T) {
	offset := shortReclaim(t, 300*time.Millisecond)
	g := newReclaimRig(t, false)
	p := g.running(t)
	shim := listenShim(t, filepath.Join(g.rt, "shim.sock"), p.dir)
	if err := p.dialShim(g.ctx); err != nil {
		t.Fatal(err)
	}
	received := time.Now()
	p.requestStop(g.ctx, stopSpotReclaim)
	eventually(t, "no stop reached the shim", func() bool { return len(shim.stops()) == 1 })
	first := shim.stops()[0]
	want := first.m.KillAtUnixMs
	if want < received.Add(300*time.Millisecond).UnixMilli() || want > time.Now().Add(300*time.Millisecond).UnixMilli() {
		t.Fatalf("kill at %d, want receipt + 300ms (%d)", want, received.Add(300*time.Millisecond).UnixMilli())
	}
	if first.m.Reason != stopSpotReclaim || first.saved == nil || first.saved.ReclaimKillAt != want || first.saved.StopReason != stopSpotReclaim {
		t.Fatalf("shim stop %+v with run state %+v: want the deadline saved before the shim was told", first.m, first.saved)
	}

	offset.Store(int64(5 * time.Second))
	p.requestStop(g.ctx, stopSpotReclaim)
	eventually(t, "the second notice did not reach the shim", func() bool { return len(shim.stops()) == 2 })
	if got := shim.stops()[1].m.KillAtUnixMs; got != want {
		t.Fatalf("second notice moved the deadline %d -> %d", want, got)
	}
	if st, err := readRunState(p.dir); err != nil || st.ReclaimKillAt != want {
		t.Fatalf("stored deadline %+v (%v), want %d", st, err, want)
	}
	for _, m := range shim.got() {
		if m.m.Type == proto.ShimStop && m.m.KillAtUnixMs == 0 {
			t.Fatalf("an ordinary stop reached the shim: %+v", m.m)
		}
	}
	eventually(t, "no kill at the deadline", g.killed)
	if killed := time.Now(); killed.Before(time.UnixMilli(want)) {
		t.Fatalf("killed at %v, before the deadline %v", killed, time.UnixMilli(want))
	}
	time.Sleep(600 * time.Millisecond)
	if n := strings.Count(strings.Join(g.signals(), " "), "KILL"); n != 1 {
		t.Fatalf("container killed %d times (signals %v): want one kill, at the first deadline", n, g.signals())
	}
}

// A runner restarted after the deadline passed kills the container at
// once on re-adopting it, with no luxd to tell it anything.
func TestReclaimReadoptedPastDeadlineKillsAtOnce(t *testing.T) {
	shortReclaim(t, 300*time.Millisecond)
	g := newReclaimRig(t, true)
	p := g.running(t)
	p.state.StopReason = stopSpotReclaim
	p.state.ReclaimKillAt = time.Now().Add(-time.Second).UnixMilli()
	p.state.Egress = &egressState{Unrestricted: true, Interface: "lux0", Gateway: "10.88.0.1"}
	if err := p.saveState(); err != nil {
		t.Fatal(err)
	}
	g.r.placements = map[string]*placement{}
	start := time.Now()
	g.r.readopt(g.ctx)
	eventually(t, "the re-adopted container was not killed", g.killed)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("killed %v after re-adopting, want at once", took)
	}
	if n := g.luxd.acked(); n != 0 {
		t.Fatalf("luxd acked %d reports; it was not there", n)
	}
	g.luxd.release(g.ctx)
	mustEnd(t, g.r.placements["run1"], "the re-adopted placement never ended")
}

// A re-adopted placement whose deadline is still ahead keeps it: a notice
// after the restart sends the stored instant, and the kill comes then.
func TestReclaimReadoptedKeepsStoredDeadline(t *testing.T) {
	shortReclaim(t, 5*time.Second)
	g := newReclaimRig(t, false)
	p := g.running(t)
	stored := time.Now().Add(400 * time.Millisecond).UnixMilli()
	p.state.StopReason = stopSpotReclaim
	p.state.ReclaimKillAt = stored
	p.state.Egress = &egressState{Unrestricted: true, Interface: "lux0", Gateway: "10.88.0.1"}
	if err := p.saveState(); err != nil {
		t.Fatal(err)
	}
	shim := listenShim(t, filepath.Join(g.rt, "shim.sock"), p.dir)
	g.r.placements = map[string]*placement{}
	g.r.readopt(g.ctx)
	q := g.r.placements["run1"]
	eventually(t, "the re-adopted placement never reached its shim", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.shimEnc != nil
	})
	q.requestStop(g.ctx, stopSpotReclaim)
	eventually(t, "no stop reached the shim", func() bool { return len(shim.stops()) == 1 })
	if got := shim.stops()[0].m.KillAtUnixMs; got != stored {
		t.Fatalf("notice after the restart: kill at %d, want the stored %d", got, stored)
	}
	eventually(t, "no kill at the stored deadline", g.killed)
	if time.Now().UnixMilli() < stored || time.Now().UnixMilli() > stored+3000 {
		t.Fatalf("killed at %d, want at %d", time.Now().UnixMilli(), stored)
	}
	mustEnd(t, q, "the placement never ended")
}

// With the shim unreachable, the container gets SIGTERM from Podman at
// once and SIGKILL at the deadline, not before.
func TestReclaimShimUnreachableTermNowKillAtDeadline(t *testing.T) {
	shortReclaim(t, 400*time.Millisecond)
	g := newReclaimRig(t, false)
	p := g.running(t)
	received := time.Now()
	p.requestStop(g.ctx, stopSpotReclaim)
	eventually(t, "no SIGTERM", func() bool { return slices.Contains(g.signals(), "TERM") })
	if took := time.Since(received); took > 300*time.Millisecond {
		t.Fatalf("SIGTERM %v after the notice, want at once", took)
	}
	if slices.Contains(g.signals(), "KILL") {
		t.Fatalf("killed before the deadline: %v", g.signals())
	}
	eventually(t, "no SIGKILL", g.killed)
	if now, deadline := time.Now(), time.UnixMilli(p.reclaimAt); now.Before(deadline) || deadline.Before(received.Add(399*time.Millisecond)) {
		t.Fatalf("SIGKILL %v after the notice, deadline %v after it: want the kill at the 400ms deadline", now.Sub(received), deadline.Sub(received))
	}
	if got := g.signals(); !slices.Equal(got, []string{"TERM", "KILL"}) {
		t.Fatalf("signals %v, want TERM then KILL", got)
	}
}

// A placement already stopping for an ordinary stop ("stop") is reclaimed
// all the same: the reclaim's reason replaces it, the shim gets the
// deadline, and the kill comes at it; luxd, gone silent after the notice,
// changes nothing.
func TestReclaimOverridesStoppingAndIgnoresLuxd(t *testing.T) {
	shortReclaim(t, 300*time.Millisecond)
	g := newReclaimRig(t, false)
	p := g.running(t)
	shim := listenShim(t, filepath.Join(g.rt, "shim.sock"), p.dir)
	if err := p.dialShim(g.ctx); err != nil {
		t.Fatal(err)
	}
	p.requestStop(g.ctx, "stop")
	eventually(t, "the ordinary stop did not reach the shim", func() bool { return len(shim.stops()) == 1 })
	if s := shim.stops()[0].m; s.KillAtUnixMs != 0 || s.Reason != "stop" {
		t.Fatalf("ordinary stop %+v", s)
	}
	g.luxd.mu.Lock()
	g.luxd.held = true
	g.luxd.mu.Unlock()
	received := time.Now()
	p.requestStop(g.ctx, stopSpotReclaim)
	eventually(t, "the reclaim did not reach the shim", func() bool { return len(shim.stops()) == 2 })
	s := shim.stops()[1].m
	if s.Reason != stopSpotReclaim || s.KillAtUnixMs < received.Add(300*time.Millisecond).UnixMilli() {
		t.Fatalf("reclaim stop %+v", s)
	}
	if p.pendingStop() != stopSpotReclaim {
		t.Fatalf("stop reason %q, want %s", p.pendingStop(), stopSpotReclaim)
	}
	eventually(t, "no kill at the deadline", g.killed)
	if time.Now().UnixMilli() < s.KillAtUnixMs {
		t.Fatal("killed before the deadline")
	}
	g.luxd.release(g.ctx)
}

// start starts a spot placement of run1 through run.
func (g *reclaimRig) start(t *testing.T) *placement {
	t.Helper()
	sp := spec.RunSpec{Spot: true, ResumePolicy: spec.ResumeNever, Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true}}
	sp.Workload.Grace.Duration = time.Minute
	p := g.place(g.ctx, sp)
	t.Cleanup(func() {
		if !p.waitDone(20 * time.Second) {
			t.Error("the placement did not end")
		}
	})
	return p
}

// A reclamation that arrives while the container is being started (after
// the last pre-create stop check, before the shim is reached): the shim is
// never told to start the workload, and its stop carries the first
// notice's deadline, never the ordinary stop's hook and grace; the
// container dies at that deadline.
func TestReclaimDuringContainerStart(t *testing.T) {
	shortReclaim(t, 600*time.Millisecond)
	g := newReclaimRig(t, false, "holdstart")
	shim := listenShim(t, filepath.Join(g.rt, "shim.sock"), filepath.Join(g.r.cfg.DataDir, "runs", "run1"))
	p := g.start(t)
	g.waitFile(t, "starting")
	p.requestStop(g.ctx, stopSpotReclaim)
	killAt := p.reclaimAt
	if st, err := readRunState(p.dir); err != nil || st.ReclaimKillAt != killAt || killAt == 0 {
		t.Fatalf("run state %+v (%v): want the deadline %d saved", st, err, killAt)
	}
	if err := os.Remove(g.bin + ".holdstart"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "no stop reached the shim", func() bool { return len(shim.stops()) > 0 })
	for _, m := range shim.got() {
		if m.m.Type == proto.ShimStart {
			t.Fatalf("the shim was told to start the workload: %+v", m.m)
		}
		if m.m.Type == proto.ShimStop && (m.m.KillAtUnixMs != killAt || m.m.Reason != stopSpotReclaim) {
			t.Fatalf("stop %+v, want spot-reclaim with kill at %d", m.m, killAt)
		}
	}
	eventually(t, "no kill at the deadline", g.killed)
	if time.Now().UnixMilli() < killAt {
		t.Fatal("killed before the deadline")
	}
}

// A container that comes up after the reclamation's deadline has passed
// is killed before anything starts its workload: the shim is never
// contacted.
func TestReclaimDeadlinePassedBeforeContainerStarted(t *testing.T) {
	shortReclaim(t, 200*time.Millisecond)
	g := newReclaimRig(t, false, "holdstart")
	shim := listenShim(t, filepath.Join(g.rt, "shim.sock"), filepath.Join(g.r.cfg.DataDir, "runs", "run1"))
	p := g.start(t)
	g.waitFile(t, "starting")
	p.requestStop(g.ctx, stopSpotReclaim)
	time.Sleep(400 * time.Millisecond)
	if err := os.Remove(g.bin + ".holdstart"); err != nil {
		t.Fatal(err)
	}
	mustEnd(t, p, "the placement never ended")
	if got := shim.got(); len(got) != 0 {
		t.Fatalf("the shim was contacted: %+v", got)
	}
	calls := podmanCalls(g.bin)
	started := slices.Index(calls, "start ctr-1")
	if started < 0 || !slices.Contains(calls[started+1:], "kill -s KILL ctr-1") {
		t.Fatalf("podman calls %v: want the container killed after it started", calls)
	}
}

// A reclamation while the image is being prepared cancels the start: no
// container is ever made, and the placement ends stopped before start.
func TestReclaimWhileBuilding(t *testing.T) {
	shortReclaim(t, 300*time.Millisecond)
	g := newReclaimRig(t, false, "holdimage")
	p := g.start(t)
	g.waitFile(t, "imaging")
	p.requestStop(g.ctx, stopSpotReclaim)
	if st, err := readRunState(p.dir); err != nil || st.ReclaimKillAt == 0 {
		t.Fatalf("run state %+v (%v): want the deadline saved", st, err)
	}
	if err := os.Remove(g.bin + ".holdimage"); err != nil {
		t.Fatal(err)
	}
	mustEnd(t, p, "the placement never ended")
	if slices.ContainsFunc(podmanCalls(g.bin), func(l string) bool { return strings.HasPrefix(l, "create") }) {
		t.Fatalf("a container was made after the reclamation: %v", podmanCalls(g.bin))
	}
	sts := g.luxd.statuses()
	if end := sts[len(sts)-1]; end.State != "exited" || end.Reason != "stopped" {
		t.Fatalf("end report %+v, want exited: stopped", end)
	}
}

func (g *reclaimRig) waitFile(t *testing.T, flag string) {
	t.Helper()
	eventually(t, flag+" never appeared", func() bool { _, err := os.Stat(g.bin + "." + flag); return err == nil })
}
