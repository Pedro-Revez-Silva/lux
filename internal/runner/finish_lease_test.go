package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
)

// finishFixture is a runner heartbeating every second to a fake luxd that
// acks every report and keeps them, with a placement of run1 (epoch 1,
// one state volume) whose container has exited. Its podman's volume
// export blocks until release is called. $0.ctr holds the id of the Run's
// container while one exists (the placement's, ctr-1); `rm -f` removes it
// by that id or the Run's container name, and `volume rm` its volume's file.
// `rmi` records the image it removes in $0.rmi. The placement's image is
// img, which lux pulled for t1.
type finishFixture struct {
	r       *Runner
	p       *placement
	bin     string
	data    string
	mu      sync.Mutex
	reports []proto.Frame
	// ctrAtStatus: whether the container existed when luxd got the status.
	ctrAtStatus []bool
}

func newFinishFixture(t *testing.T) *finishFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	script := `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"volume export")
  touch "$0.exporting"
  while [ ! -e "$0.release" ]; do sleep 0.05; done
  echo volume-data ;;
"rm -f") case "$5" in lux-run1|"$(cat "$0.ctr")") rm -f "$0.ctr" ;; esac ;;
"volume rm") rm -f "$0.vol.$4" ;;
"rmi "*) echo "$2" >> "$0.rmi" ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"snapshots", "runs", "rt", "data"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(dir, "data")
	for path, content := range map[string]string{bin + ".ctr": "ctr-1", bin + ".vol." + volumeName("run1", "data"): "",
		filepath.Join(data, "state"): "kept"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, git: gitws.New(filepath.Join(dir, "git")),
		images: newImageUse(dir)}
	r.images.pulledBy(finishImage, "t1")
	r.conn = newConn(r)
	r.conn.polling = true
	r.uploads = newUploader(r)
	r.lease.Store(int64(3 * time.Second))
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))
	r.mounts.Store(volumeName("run1", "data"), data)
	p := &placement{r: r, runID: "run1", tenantID: "t1", epoch: 1, dir: r.runDir("run1"), phase: "stopping",
		assign: &proto.Assign{}, done: make(chan struct{}),
		state: &runState{RunID: "run1", TenantID: "t1", Epoch: 1, Phase: "started", Container: "ctr-1", Image: finishImage,
			Volumes: []volumeRef{{Name: "data", Volume: volumeName("run1", "data"), Path: "/data", Kind: "state"}}}}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.placements["run1"] = p
	f := &finishFixture{r: r, p: p, bin: bin, data: data}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			reports := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, fr := range reports {
				f.mu.Lock()
				f.reports = append(f.reports, fr)
				if fr.Type == proto.MsgStatus {
					_, err := os.Stat(bin + ".ctr")
					f.ctrAtStatus = append(f.ctrAtStatus, err == nil)
				}
				f.mu.Unlock()
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
			}
			time.Sleep(time.Millisecond)
		}
	})
	wg.Go(func() { r.heartbeatLoop(ctx) })
	t.Cleanup(func() {
		f.release()
		cancel()
		wg.Wait()
	})
	return f
}

func (f *finishFixture) release() { _ = os.WriteFile(f.bin+".release", nil, 0o600) }

const finishImage = "ghcr.io/a/img:1"

// exitedAsSupervised ends the placement's container as supervise does, and
// runs finish in the background.
func (f *finishFixture) exitedAsSupervised(ctx context.Context) {
	f.p.setPhase("exited")
	go func() {
		defer close(f.p.done)
		f.p.finish(ctx, &exitRecord{Code: 0, Reason: "stopped"})
	}()
}

func (f *finishFixture) waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// heartbeatAfter waits for a heartbeat sent after n reports, and returns
// its leases and how many reports there were up to it.
func (f *finishFixture) heartbeatAfter(t *testing.T, n int) ([]proto.LivePlacement, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for i := n; i < len(f.reports); i++ {
			if f.reports[i].Type == proto.MsgHeartbeat {
				var hb proto.Heartbeat
				if err := json.Unmarshal(f.reports[i].Data, &hb); err != nil {
					f.mu.Unlock()
					t.Fatal(err)
				}
				f.mu.Unlock()
				return hb.Leases, i + 1
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no heartbeat")
	return nil, 0
}

func (f *finishFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reports)
}

// types are the report types sent so far, but heartbeats.
func (f *finishFixture) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.reports {
		if fr.Type != proto.MsgHeartbeat {
			out = append(out, fr.Type)
		}
	}
	return out
}

// A placement whose container has exited keeps its lease in heartbeats
// while its snapshot is exported, however long that takes, and drops it
// once it has reported its snapshot and its end.
func TestFinishingPlacementKeepsItsLease(t *testing.T) {
	f := newFinishFixture(t)
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".exporting")

	// Ignore heartbeats within the lease window, then require a fresh one.
	leaseEnds := time.Now().Add(time.Duration(f.r.lease.Load()))
	n := f.count()
	for time.Now().Before(leaseEnds) {
		_, n = f.heartbeatAfter(t, n)
	}
	leases, _ := f.heartbeatAfter(t, f.count())
	if len(leases) != 1 || leases[0].RunID != "run1" || leases[0].Epoch != 1 || leases[0].State != "stopping" {
		t.Fatalf("leases while snapshotting: %+v, want run1 epoch 1 stopping", leases)
	}

	f.release()
	f.p.waitDone(10 * time.Second)
	if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
		t.Fatalf("reports %v, want snapshot.done then status", got)
	}
	if leases, _ := f.heartbeatAfter(t, f.count()); len(leases) != 0 {
		t.Fatalf("leases once reported: %+v, want none", leases)
	}
}

// A stop, once its snapshot and end are reported, leaves the Run's volumes
// on the host for a resume here, and no container: none is started again.
// The container is still there while luxd is told the Run ended.
func TestReportedStopLeavesVolumesAndNoContainer(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.exitedAsSupervised(context.Background())
	f.p.waitDone(10 * time.Second)
	if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
		t.Fatalf("reports %v, want snapshot.done then status", got)
	}
	log, _ := os.ReadFile(f.bin + ".log")
	if _, err := os.Stat(f.bin + ".ctr"); err == nil {
		t.Errorf("the stopped container is still there; podman:\n%s", log)
	}
	f.mu.Lock()
	atStatus := slices.Clone(f.ctrAtStatus)
	f.mu.Unlock()
	if !slices.Equal(atStatus, []bool{true}) {
		t.Errorf("container present at each status report: %v, want removed only after it", atStatus)
	}
	if _, err := os.Stat(f.bin + ".vol." + volumeName("run1", "data")); err != nil {
		t.Errorf("the state volume was removed; podman:\n%s", log)
	}
	if b, err := os.ReadFile(filepath.Join(f.data, "state")); err != nil || string(b) != "kept" {
		t.Errorf("the state volume holds %q (%v), want its data", b, err)
	}
	st, err := readRunState(f.p.dir)
	if err != nil || st.Phase != "reported" || st.VolumesSnapshot == "" {
		t.Errorf("run state %+v (%v): want reported, its volumes the snapshot's", st, err)
	}
}

// A stop removes the container that kept its image from the GC: the image
// counts as used at the stop, so a Run that ran longer than the TTL keeps
// it for a resume here as long as its volumes.
func TestStoppedRunsImageIsUsedAtItsStop(t *testing.T) {
	f := newFinishFixture(t)
	started := time.Now().Add(-2 * time.Hour).UnixMilli()
	f.r.images.mu.Lock()
	f.r.images.last[finishImage] = started
	f.r.images.mu.Unlock()
	before := time.Now().UnixMilli()
	f.release()
	f.exitedAsSupervised(context.Background())
	f.p.waitDone(10 * time.Second)
	if at := f.r.images.snapshot()[finishImage]; at < before {
		t.Fatalf("image last used at %d, want at the stop (>= %d)", at, before)
	}
	f.r.gcImages(context.Background(), time.Hour)
	if b, err := os.ReadFile(f.bin + ".rmi"); err == nil {
		t.Errorf("the GC removed %q, a stopped Run's image used within its TTL", b)
	}
	if _, ok := f.r.images.snapshot()[finishImage]; !ok {
		t.Error("the image is no longer recorded as lux's")
	}
}

func TestStoppedRunsOperatorImageIsNotClaimed(t *testing.T) {
	f := newFinishFixture(t)
	f.p.state.Image = "alpine:3"
	if _, ok := f.r.images.snapshot()[f.p.state.Image]; ok {
		t.Fatal("the operator's image is already recorded as lux's")
	}
	f.release()
	f.exitedAsSupervised(context.Background())
	f.p.waitDone(10 * time.Second)
	if _, ok := f.r.images.snapshot()[f.p.state.Image]; ok {
		t.Error("finish claimed the operator's image as lux's")
	}
}

// A finishing placement removes only the container it made: a resume here,
// assigned before it was done, may have made the next under the Run's name.
func TestFinishLeavesTheNextPlacementsContainer(t *testing.T) {
	f := newFinishFixture(t)
	if err := os.WriteFile(f.bin+".ctr", []byte("ctr-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.release()
	f.exitedAsSupervised(context.Background())
	f.p.waitDone(10 * time.Second)
	if b, err := os.ReadFile(f.bin + ".ctr"); err != nil || string(b) != "ctr-2" {
		log, _ := os.ReadFile(f.bin + ".log")
		t.Errorf("the next placement's container is gone (%q, %v); podman:\n%s", b, err, log)
	}
}

// A runner restarted while a placement was finishing (its container exited,
// its end not yet reported) re-adopts it: it reports the stop, then removes
// the container and keeps the volumes.
func TestReadoptedFinishingPlacementRemovesItsContainer(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.state.Phase = "exited"
	f.p.state.Exit = &exitRecord{Code: 0, Reason: "stopped"}
	if err := writeRunState(f.p.dir, f.p.state); err != nil {
		t.Fatal(err)
	}
	f.r.mu.Lock()
	f.r.placements = map[string]*placement{}
	f.r.mu.Unlock()
	f.r.readopt(context.Background())
	f.r.mu.Lock()
	p := f.r.placements["run1"]
	f.r.mu.Unlock()
	if p == nil {
		t.Fatal("the finishing placement was not re-adopted")
	}
	p.waitDone(10 * time.Second)
	if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
		t.Fatalf("reports %v, want snapshot.done then status", got)
	}
	log, _ := os.ReadFile(f.bin + ".log")
	if _, err := os.Stat(f.bin + ".ctr"); err == nil {
		t.Errorf("the stopped container is still there; podman:\n%s", log)
	}
	if b, err := os.ReadFile(filepath.Join(f.data, "state")); err != nil || string(b) != "kept" {
		t.Errorf("the state volume holds %q (%v), want its data", b, err)
	}
}

// A finishing placement luxd fenced off (it gave up on it) renews no
// lease: it cannot hold a Run luxd moved on from.
func TestFencedFinishingPlacementHasNoLease(t *testing.T) {
	f := newFinishFixture(t)
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".exporting")
	f.p.markStale()
	if leases, _ := f.heartbeatAfter(t, f.count()); len(leases) != 0 {
		t.Fatalf("leases of a fenced-off placement: %+v, want none", leases)
	}
}

// luxd's welcome after a reconnect no longer lists a finishing placement
// it has already recorded: the runner lets it finish rather than fence it
// off, which would skip uploading the snapshot luxd recorded.
func TestWelcomeDoesNotFenceAFinishingPlacement(t *testing.T) {
	f := newFinishFixture(t)
	f.p.setPhase("exited")
	f.r.onWelcome(context.Background(), proto.Welcome{})
	if f.p.isStale() {
		t.Fatal("a finishing placement was fenced off by a welcome")
	}
	f.p.setPhase("running")
	f.r.onWelcome(context.Background(), proto.Welcome{})
	if !f.p.isStale() {
		t.Fatal("a running placement luxd does not list was not fenced off")
	}
}
