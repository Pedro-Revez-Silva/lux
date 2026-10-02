package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// export blocks until release is called.
type finishFixture struct {
	r       *Runner
	p       *placement
	bin     string
	mu      sync.Mutex
	reports []proto.Frame
}

func newFinishFixture(t *testing.T) *finishFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	script := `#!/bin/sh
case "$1 $2" in
"volume export")
  touch "$0.exporting"
  while [ ! -e "$0.release" ]; do sleep 0.05; done
  echo volume-data ;;
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
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, git: gitws.New(filepath.Join(dir, "git"))}
	r.conn = newConn(r)
	r.conn.polling = true
	r.uploads = newUploader(r)
	r.lease.Store(int64(3 * time.Second))
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))
	r.mounts.Store(volumeName("run1", "data"), filepath.Join(dir, "data"))
	p := &placement{r: r, runID: "run1", tenantID: "t1", epoch: 1, dir: r.runDir("run1"), phase: "stopping",
		assign: &proto.Assign{}, done: make(chan struct{}),
		state: &runState{RunID: "run1", TenantID: "t1", Epoch: 1, Phase: "started",
			Volumes: []volumeRef{{Name: "data", Volume: volumeName("run1", "data"), Path: "/data", Kind: "state"}}}}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.placements["run1"] = p
	f := &finishFixture{r: r, p: p, bin: bin}

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
