package runner

import (
	"context"
	"fmt"
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

// resizePodman is a fake podman keeping its container and volumes as files
// beside it ($0.ctr, $0.vol.<name>), and logging its commands to $0.log.
// The container, while it exists, is inspected as stopped, made from image
// img1 with the lux.spec label in $0.hash. A create writes a new id to
// $0.ctr and its arguments to $0.created; `volume import` copies its input
// into the volume's file; `volume rm` removes the container too, as
// podman's -f does.
const resizePodman = `#!/bin/sh
echo "$*" >> "$0.log"
case "$1 $2" in
"container inspect")
  [ -e "$0.ctr" ] || { echo "no such container $3" >&2; exit 125; }
  printf '[{"Image":"img1","State":{"Status":"exited","Running":false},"Config":{"Labels":{"lux.spec":"%s"}}}]' "$(cat "$0.hash")" ;;
"volume exists") [ -e "$0.vol.$3" ] ;;
"volume create") eval "v=\${$#}"; : > "$0.vol.$v" ;;
"volume rm") rm -f "$0.vol.$4" "$0.ctr" ;;
"volume import") cat > "$0.vol.$3" ;;
"rm -f") rm -f "$0.ctr" ;;
"start "*)
  [ -e "$0.ctr" ] || { echo "no container $2" >&2; exit 125; }
  cat "$0.ctr" >> "$0.started" ;;
*)
  if [ "$1" = create ]; then
    id=$(cat "$0.next-id")
    echo "ctr-$id" > "$0.ctr"
    echo "$((id+1))" > "$0.next-id"
    printf '%s\n' "$@" > "$0.created"
  fi ;;
esac
`

// resizeFixture is run1's placement at epoch 2 on the host that ran epoch
// 1: its state volume "data" holds "state" (snapshot snap1, also kept
// locally as blob1), and its stopped container ctr-1 was made from old.
// The new placement's assignment carries now.
type resizeFixture struct {
	r   *Runner
	p   *placement
	a   *proto.Assign
	bin string
}

func newResizeFixture(t *testing.T, old, now spec.Resources, local string) *resizeFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(resizePodman), 0o755); err != nil {
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

	vols := []spec.Volume{{Name: "data", Path: "/data", Kind: "state"}}
	oldSpec := spec.RunSpec{Volumes: vols, Resources: old}
	a := &proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2, Spec: spec.RunSpec{Volumes: vols, Resources: now},
		Resume: &proto.ResumeInfo{Snapshot: manifest}}
	p := newPlacement(r, *a)
	p.state = &runState{RunID: "run1", TenantID: "t1", Epoch: 2, VolumesSnapshot: local,
		Volumes: []volumeRef{{Name: "data", Volume: volumeName("run1", "data"), Path: "/data", Kind: "state"}}}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for suffix, value := range map[string]string{
		".vol." + volumeName("run1", "data"): "state",
		".vol." + runtimeVolume("run1"):      "",
		".ctr":                               "ctr-1\n",
		".next-id":                           "2\n",
		".hash":                              argsHash(p.createArgs(oldSpec, "img", podman.Network{})),
	} {
		if err := os.WriteFile(bin+suffix, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// luxd: every report acknowledged.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			reports := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, fr := range reports {
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
			}
			time.Sleep(time.Millisecond)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &resizeFixture{r: r, p: p, a: a, bin: bin}
}

// start does what placement.run does from stopping the previous container
// to starting this placement's.
func (f *resizeFixture) start(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	err := f.p.stopPrevious(ctx)
	if err == nil {
		err = f.p.prepareVolumes(ctx, f.a.Spec, nil, f.a.Resume)
	}
	if err == nil {
		err = f.p.createContainer(ctx, f.a.Spec, "img", "img1", podman.Network{}, f.a)
	}
	if err == nil {
		err = f.r.pm.Start(ctx, containerName("run1"))
	}
	if err != nil {
		t.Fatalf("start: %v; podman:\n%s", err, f.read(".log"))
	}
}

func (f *resizeFixture) read(suffix string) string {
	b, _ := os.ReadFile(f.bin + suffix)
	return string(b)
}

// flag is the value following name in the last create's arguments.
func (f *resizeFixture) flag(name string) string {
	args := strings.Split(f.read(".created"), "\n")
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// A resume with other cpus or memory does not reuse the stopped container:
// a new one is made with the new limits, and the Run's state volume keeps
// its data, whether it was the snapshot's already (nothing moves) or is
// restored from it.
func TestResizedResumeGetsANewContainer(t *testing.T) {
	old := spec.Resources{CPUs: 2, Memory: 4 << 30, Pids: 1024}
	for _, c := range []struct {
		name  string
		now   spec.Resources
		local string
	}{
		{"more cpus, less memory, volumes local", spec.Resources{CPUs: 4, Memory: 1 << 30, Pids: 1024}, "snap1"},
		{"fewer cpus, more memory, volumes local", spec.Resources{CPUs: 0.5, Memory: 8 << 30, Pids: 1024}, "snap1"},
		{"more cpus, volumes restored", spec.Resources{CPUs: 4, Memory: 4 << 30, Pids: 1024}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newResizeFixture(t, old, c.now, c.local)
			f.start(t)
			if got := f.read(".started"); got != "ctr-2\n" {
				t.Fatalf("started %q, want a new container ctr-2; podman:\n%s", got, f.read(".log"))
			}
			wantCPUs, wantMem := fmt.Sprintf("%g", c.now.CPUs), fmt.Sprintf("%d", c.now.Memory)
			if cpus, mem := f.flag("--cpus"), f.flag("--memory"); cpus != wantCPUs || mem != wantMem {
				t.Errorf("created with --cpus %s --memory %s, want %s %s", cpus, mem, wantCPUs, wantMem)
			}
			if !strings.Contains(f.read(".created"), volumeName("run1", "data")+":/data:idmap") {
				t.Errorf("new container does not mount the state volume:\n%s", f.read(".created"))
			}
			if got := f.read(".vol." + volumeName("run1", "data")); got != "state" {
				t.Errorf("state volume holds %q, want its data", got)
			}
			restored := strings.Contains(f.read(".log"), "volume import "+volumeName("run1", "data"))
			if restored != (c.local == "") {
				t.Errorf("volume imported: %v; podman:\n%s", restored, f.read(".log"))
			}
		})
	}
	// The same size, volumes local: the stopped container is reused.
	t.Run("same size", func(t *testing.T) {
		f := newResizeFixture(t, old, old, "snap1")
		f.start(t)
		if got := f.read(".started"); got != "ctr-1\n" {
			t.Fatalf("started %q, want the stopped ctr-1 reused; podman:\n%s", got, f.read(".log"))
		}
	})
}
