package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// mounts the volume; `volume import` writes its input into the volume. The
// container, while it exists, is inspected as stopped. Each create assigns
// a new ID, prints it and records its arguments in $0.created; as
// podman's, it fails while a container exists under the name. Start
// records the ID it actually starts.
const podmanWithVolumes = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"container inspect")
  [ -e "$0.ctr" ] || { echo "no such container $3" >&2; exit 125; }
  printf '[{"Image":"img1","State":{"Status":"exited","Running":false}}]' ;;
"volume exists") [ -e "$0.vol.$3" ] ;;
"volume create") eval "v=\${$#}"; touch "$0.vol.$v" ;;
"volume rm") rm -f "$0.vol.$4" "$0.ctr" ;;
"volume import") cat > "$0.vol.$3" ;;
"rm -f") rm -f "$0.ctr" ;;
"start "*)
  [ -e "$0.ctr" ] || { echo "no container $2" >&2; exit 125; }
  cat "$0.ctr" >> "$0.started" ;;
*)
  if [ "$1" = create ]; then
    [ -e "$0.ctr" ] && { echo "Error: creating container storage: the container name \"lux-run1\" is already in use by $(cat "$0.ctr")" >&2; exit 125; }
    id=$(cat "$0.next-id")
    echo "ctr-$id" > "$0.ctr"
    echo "$((id+1))" > "$0.next-id"
    printf '%s\n' "$@" > "$0.created"
    echo "ctr-$id"
  fi ;;
esac
`

// resumeFixture is a resume of run1 at epoch onto a host that has the
// Run's state volume data, its runtime volume and its stopped container
// ctr-1, and each of extra (spec volumes beyond data) holding "old". local
// is the snapshot the host's
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

func newResumeFixture(t *testing.T, epoch int, local string, extra ...spec.Volume) *resumeFixture {
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

	sp := spec.RunSpec{Volumes: append([]spec.Volume{{Name: "data", Path: "/data", Kind: "state"}}, extra...)}
	a := &proto.Assign{RunID: "run1", TenantID: "t1", Epoch: epoch, Spec: sp, Resume: &proto.ResumeInfo{Snapshot: manifest}}
	p := newPlacement(r, *a, nil)
	p.state = &runState{RunID: "run1", TenantID: "t1", Epoch: epoch, VolumesSnapshot: local,
		Volumes: []volumeRef{{Name: "data", Volume: volumeName("run1", "data"), Path: "/data", Kind: "state"}}}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".ctr": "ctr-1\n", ".next-id": "2\n",
		".vol." + volumeName("run1", "data"): "local-state", ".vol." + runtimeVolume("run1"): ""}
	for _, v := range extra {
		files[".vol."+volumeName("run1", v.Name)] = "old"
	}
	for suffix, value := range files {
		if err := os.WriteFile(bin+suffix, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if err := f.p.prepareVolumes(ctx, f.sp, f.a.Resume); err != nil {
		return err
	}
	if err := f.p.createContainer(ctx, f.sp, "img", podman.Network{}, nil); err != nil {
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
// reported one): restoring the state volume removes the container with it,
// and the placement creates a new one and starts it.
func TestResumeAfterRestoreRemovedTheContainerCreatesOne(t *testing.T) {
	f := newResumeFixture(t, 3, "")
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.assertStartedContainer(t, "ctr-2")
	f.waitEvent(t, "volumes.restored")
}

// A same-host resume whose volumes are the snapshot's keeps them as they
// are (nothing imported or downloaded) and starts a new container, as on a
// new host: the stopped one, with its writable layer, is removed (a create
// under its name would fail).
func TestSameHostResumeGetsANewContainerOnItsLocalVolumes(t *testing.T) {
	f := newResumeFixture(t, 2, "snap1")
	if got, err := os.ReadFile(f.bin + ".ctr"); err != nil || string(got) != "ctr-1\n" {
		t.Fatalf("the host holds container %q (%v), want the stopped ctr-1", got, err)
	}
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.assertStartedContainer(t, "ctr-2")
	if !strings.Contains(f.podmanLog(), "rm -f -t 0 "+containerName("run1")+"\n") {
		t.Errorf("the stopped container was not removed by name; podman:\n%s", f.podmanLog())
	}
	f.waitEvent(t, "volumes.local")
	data := volumeName("run1", "data")
	if got, err := os.ReadFile(f.bin + ".vol." + data); err != nil || string(got) != "local-state" {
		t.Errorf("state volume holds %q (%v), want the host's local-state untouched; podman:\n%s", got, err, f.podmanLog())
	}
	created, _ := os.ReadFile(f.bin + ".created")
	if !slices.Contains(strings.Split(string(created), "\n"), data+":/data:idmap") {
		t.Errorf("the new container does not mount the local state volume:\n%s", created)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.events, "volumes.restored") {
		t.Errorf("events %q: the volumes were restored", f.events)
	}
}

// createContainer records the new container's id, persisted: the stop
// removes that container by it, after a runner restart too.
func TestCreateContainerRecordsItsID(t *testing.T) {
	f := newResumeFixture(t, 2, "snap1")
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.p.mu.Lock()
	id := f.p.state.Container
	f.p.mu.Unlock()
	if id != "ctr-2" {
		t.Errorf("placement records container %q, want ctr-2", id)
	}
	st, err := readRunState(f.p.dir)
	if err != nil || st.Container != "ctr-2" {
		t.Errorf("persisted run state %+v (%v), want container ctr-2", st, err)
	}
}

// An ephemeral volume starts empty on a same-host resume, as on a new host:
// the one left here is removed, then made again, while the state volume
// stays as it is.
func TestSameHostResumeEmptiesEphemeralVolumes(t *testing.T) {
	f := newResumeFixture(t, 2, "snap1", spec.Volume{Name: "scratch", Path: "/scratch", Kind: "ephemeral"})
	if err := f.start(context.Background()); err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.podmanLog())
	}
	f.waitEvent(t, "volumes.local")
	scratch := volumeName("run1", "scratch")
	log := strings.Split(f.podmanLog(), "\n")
	rm := slices.Index(log, "volume rm -f "+scratch)
	create := slices.IndexFunc(log, func(l string) bool {
		return strings.HasPrefix(l, "volume create ") && strings.HasSuffix(l, " "+scratch)
	})
	if rm < 0 || create < rm {
		t.Errorf("ephemeral volume removed at line %d, created at %d: want removed, then created; podman:\n%s", rm, create, f.podmanLog())
	}
	if got, err := os.ReadFile(f.bin + ".vol." + scratch); err != nil || string(got) != "" {
		t.Errorf("ephemeral volume holds %q (%v), want it empty", got, err)
	}
	if got, err := os.ReadFile(f.bin + ".vol." + volumeName("run1", "data")); err != nil || string(got) != "local-state" {
		t.Errorf("state volume holds %q (%v), want local-state untouched", got, err)
	}
}
