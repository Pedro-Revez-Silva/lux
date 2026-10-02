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

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanWithVolumes is a fake podman keeping its container and volumes as
// files beside it ($0.ctr, $0.vol.<name>), and logging its commands to
// $0.log. As podman does, `volume rm -f` also removes the container that
// mounts the volume. The container, while it exists, is inspected as
// stopped, made from image img1 with the lux.spec label in $0.hash.
// Each create assigns a new ID; start records the ID it actually starts.
const podmanWithVolumes = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"container inspect")
  [ -e "$0.ctr" ] || { echo "no such container $3" >&2; exit 125; }
  printf '[{"Image":"img1","State":{"Status":"exited","Running":false},"Config":{"Labels":{"lux.spec":"%s"}}}]' "$(cat "$0.hash")" ;;
"volume exists") [ -e "$0.vol.$3" ] ;;
"volume create") eval "v=\${$#}"; touch "$0.vol.$v" ;;
"volume rm") rm -f "$0.vol.$4" "$0.ctr" ;;
"volume import") cat > /dev/null ;;
"rm -f") rm -f "$0.ctr" ;;
"start "*)
  [ -e "$0.ctr" ] || { echo "no container $2" >&2; exit 125; }
  cat "$0.ctr" >> "$0.started" ;;
*)
  if [ "$1" = create ]; then
    id=$(cat "$0.next-id")
    echo "ctr-$id" > "$0.ctr"
    echo "$((id+1))" > "$0.next-id"
  fi ;;
esac
`

// resumeFixture is a resume of run1 at epoch onto a host that has the
// Run's state volume data, its runtime volume and its stopped container,
// made as this placement would make it. local is the snapshot the host's
// volumes hold ("" when they diverged from any). The snapshot to restore,
// snap1, is held locally. luxd acks every report; events are the run
// events reported so far.
type resumeFixture struct {
	r   *Runner
	p   *placement
	a   *proto.Assign
	sp  spec.RunSpec
	bin string

	mu     sync.Mutex
	events []string
}

func newResumeFixture(t *testing.T, epoch int, local string) *resumeFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanWithVolumes), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"snapshots", "runs", "rt"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}}
	r.conn = newConn(r)
	r.conn.polling = true
	r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))

	blob, err := os.Create(r.blobPath("blob1"))
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := zstd.NewWriter(blob)
	zw.Write([]byte("state"))
	zw.Close()
	blob.Close()
	manifest := &proto.Manifest{SnapshotID: "snap1", RunID: "run1", Epoch: 1,
		Volumes: []proto.VolumeSnapshot{{Name: "data", Path: "/data", BlobID: "blob1"}}}

	sp := spec.RunSpec{Volumes: []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}}}
	a := &proto.Assign{RunID: "run1", TenantID: "t1", Epoch: epoch, Spec: sp, Resume: &proto.ResumeInfo{Snapshot: manifest}}
	p := newPlacement(r, *a)
	p.state = &runState{RunID: "run1", TenantID: "t1", Epoch: epoch, VolumesSnapshot: local,
		Volumes: []volumeRef{{Name: "data", Volume: volumeName("run1", "data"), Path: "/data", Kind: "state"}}}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".vol." + volumeName("run1", "data"), ".vol." + runtimeVolume("run1")} {
		if err := os.WriteFile(bin+f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for suffix, value := range map[string]string{".ctr": "ctr-1\n", ".next-id": "2\n"} {
		if err := os.WriteFile(bin+suffix, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin+".hash", []byte(argsHash(p.createArgs(sp, "img", podman.Network{}))), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &resumeFixture{r: r, p: p, a: a, sp: sp, bin: bin}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			reports := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, fr := range reports {
				var ev proto.RunEvent
				if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil {
					f.mu.Lock()
					f.events = append(f.events, ev.Type)
					f.mu.Unlock()
				}
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
			}
			time.Sleep(time.Millisecond)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	return f
}

// start does what placement.run does from stopping the previous container
// to starting this placement's.
func (f *resumeFixture) start(ctx context.Context) error {
	if err := f.p.stopPrevious(ctx); err != nil {
		return err
	}
	if err := f.p.prepareVolumes(ctx, f.sp, nil, f.a.Resume); err != nil {
		return err
	}
	if err := f.p.createContainer(ctx, f.sp, "img", "img1", podman.Network{}, f.a); err != nil {
		return err
	}
	return f.r.pm.Start(ctx, containerName("run1"))
}

func (f *resumeFixture) waitEvent(t *testing.T, typ string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := slices.Clone(f.events)
		f.mu.Unlock()
		if slices.Contains(got, typ) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %q: no %s", got, typ)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *resumeFixture) assertStartedContainer(t *testing.T, id string) {
	t.Helper()
	started, err := os.ReadFile(f.bin + ".started")
	if err != nil {
		t.Fatal(err)
	}
	if string(started) != id+"\n" {
		t.Fatalf("started containers %q, want only %s; podman:\n%s", started, id, f.podmanLog())
	}
}

func (f *resumeFixture) podmanLog() string {
	b, _ := os.ReadFile(f.bin + ".log")
	return string(b)
}

// A resume on the host that still has the Run's stopped container, whose
// volumes are not the snapshot's (its last placement was lost before it
// reported one): restoring the state volume removes the container with
// it, so the placement creates a new one rather than reuse it, and starts.
func TestResumeAfterRestoreRemovedTheContainerCreatesOne(t *testing.T) {
	f := newResumeFixture(t, 3, "")
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.assertStartedContainer(t, "ctr-2")
	f.waitEvent(t, "volumes.restored")
}

// A same-host resume whose volumes are the snapshot's keeps its stopped
// container: nothing removed it.
func TestSameHostResumeReusesTheStoppedContainer(t *testing.T) {
	f := newResumeFixture(t, 2, "snap1")
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.assertStartedContainer(t, "ctr-1")
	f.waitEvent(t, "container.reused")
}
