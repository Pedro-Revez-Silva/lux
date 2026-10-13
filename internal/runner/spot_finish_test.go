package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// spotFinishFixture is a finishFixture whose placement is a spot Run's
// (resumePolicy never), with an ephemeral volume beside its state volume.
func spotFinishFixture(t *testing.T) *finishFixture {
	t.Helper()
	f := newFinishFixture(t)
	f.serveUploads(t)
	f.p.assign.Spec = spec.RunSpec{Spot: true, ResumePolicy: spec.ResumeNever}
	f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
	f.p.state.Spec = &f.p.assign.Spec
	scratch := volumeName("run1", "scratch")
	f.p.state.Volumes = append(f.p.state.Volumes, volumeRef{Name: "scratch", Volume: scratch, Path: "/scratch", Kind: "ephemeral"})
	writeFile(t, f.bin+".vol."+scratch, "")
	scratchDir := filepath.Join(f.r.cfg.DataDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.r.mounts.Store(scratch, scratchDir)
	writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
	f.release()
	old := releaseRetry
	releaseRetry = 50 * time.Millisecond
	t.Cleanup(func() { releaseRetry = old })
	return f
}

func (f *finishFixture) volumeExists(name string) bool {
	_, err := os.Stat(f.bin + ".vol." + volumeName("run1", name))
	return err == nil
}

func (f *finishFixture) runEvents() []proto.RunEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.RunEvent
	for _, fr := range f.reports {
		if fr.Type == proto.MsgRunEvent {
			var ev proto.RunEvent
			_ = json.Unmarshal(fr.Data, &ev)
			out = append(out, ev)
		}
	}
	return out
}

// A reclaimed spot Run's end frees its disk at luxd, so the runner removes
// its container and its state and ephemeral volumes first: luxd gets the
// snapshot (output and artifacts staged as blobs, kept and uploaded), then
// the removals run, then the end report.
func TestSpotFinishRemovesDiskBeforeEndReport(t *testing.T) {
	spotFinishRemovesDisk(t, spotFinishFixture(t))
}

// With disk scaling on (a shaped pool's runner), the Run's limit is its
// share of the filesystem, but the volumes released and the order of the
// end are the same.
func TestSpotFinishRemovesDiskWithDiskScale(t *testing.T) {
	f := spotFinishFixture(t)
	f.r.disk = diskScale{share: newShare(150<<30, 160<<30), on: true, headroom: 10 << 30, fsCapacity: 160 << 30}
	f.p.assign.Spec.Resources.Disk = spec.Bytes(40 << 30)
	if l := f.p.diskLimit(); !l.Scaled || l.Effective != 40_265_318_400 {
		t.Fatalf("disk limit %+v, want 40 GiB scaled by 150/160", l)
	}
	spotFinishRemovesDisk(t, f)
}

func spotFinishRemovesDisk(t *testing.T, f *finishFixture) {
	t.Helper()
	f.exitedAsSupervised(context.Background())
	mustEnd(t, f.p, "the placement never ended")
	if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
		t.Fatalf("reports %v, want snapshot.done then status", got)
	}
	f.mu.Lock()
	ctrAt, volAt := slices.Clone(f.ctrAtStatus), slices.Clone(f.volAtStatus)
	f.mu.Unlock()
	if !slices.Equal(ctrAt, []bool{false}) || !slices.Equal(volAt, []bool{false}) {
		t.Fatalf("at the end report: container present %v, state volume present %v; want both gone first\npodman:\n%s", ctrAt, volAt, f.podmanLog())
	}
	if f.volumeExists("scratch") {
		t.Fatalf("the ephemeral volume is still there; podman:\n%s", f.podmanLog())
	}
	sd := f.snapshotDone(t)
	if sd.Output == nil || len(sd.Artifacts) != 1 {
		t.Fatalf("snapshot.done %+v: want its output and artifact", sd)
	}
	f.r.uploads.pass(context.Background())
	f.mu.Lock()
	uploaded := slices.Clone(f.uploaded)
	f.mu.Unlock()
	if !slices.Contains(uploaded, sd.Output.BlobID) || !slices.Contains(uploaded, sd.Artifacts[0].BlobID) {
		t.Fatalf("uploaded %v, want the output and artifact blobs", uploaded)
	}
	// The order podman saw: the container and both volumes removed, and
	// only then (above) the end report.
	log := f.podmanLog()
	for _, want := range []string{"rm -f -t 0 ctr-1", "volume rm -f " + volumeName("run1", "data"), "volume rm -f " + volumeName("run1", "scratch")} {
		if !strings.Contains(log, want) {
			t.Fatalf("podman never ran %q:\n%s", want, log)
		}
	}
	if st, err := readRunState(f.p.dir); err != nil || st.Phase != "reported" {
		t.Fatalf("run state %+v (%v), want reported", st, err)
	}
}

// A spot Run's volume that cannot be removed keeps its end unreported:
// luxd keeps the placement live (it is in every heartbeat's leases) and so
// its disk reserved. The pending release survives a runner restart; once
// the volume goes, the end is reported.
func TestSpotFinishKeepsDiskReservedUntilCleanup(t *testing.T) {
	f := spotFinishFixture(t)
	writeFile(t, f.bin+".failvolrm", "")
	ctx, cancel := context.WithCancel(context.Background())
	f.exitedAsSupervised(ctx)
	waitFor(t, "the snapshot was not reported", func() bool { return slices.Contains(f.types(), proto.MsgSnapshotDone) })
	waitFor(t, "no volume removal", func() bool { return strings.Contains(f.podmanLog(), "volume rm") })
	time.Sleep(300 * time.Millisecond)
	if got := f.types(); slices.Contains(got, proto.MsgStatus) {
		t.Fatalf("reports %v: the end was reported with the volume still there", got)
	}
	if n := strings.Count(f.podmanLog(), "volume rm"); n < 3 {
		t.Fatalf("%d volume removals in 300ms: want retries", n)
	}
	if evs := f.runEvents(); len(evs) != 1 || evs[0].Type != proto.EvDiskReleaseFailed || evs[0].Data["error"] == "" || evs[0].Data["error"] == nil {
		t.Fatalf("run events %+v: want one %s with its error across the failed removals", evs, proto.EvDiskReleaseFailed)
	}
	leases, _ := f.heartbeatAfter(t, f.count())
	if len(leases) != 1 || leases[0].RunID != "run1" || leases[0].State != "stopping" {
		t.Fatalf("leases %+v: want run1 still held (stopping)", leases)
	}
	st, err := readRunState(f.p.dir)
	if err != nil || !st.ReleasePending || st.Phase != "exited" {
		t.Fatalf("run state %+v (%v): want exited with its release pending", st, err)
	}

	// The runner restarts: the re-adopted placement does not snapshot
	// again; it goes on removing, and reports the end once it has.
	cancel()
	mustEnd(t, f.p, "the first runner's finish did not stop")
	f.r.mu.Lock()
	f.r.placements = map[string]*placement{}
	f.r.mu.Unlock()
	f.r.readopt(context.Background())
	f.r.mu.Lock()
	p := f.r.placements["run1"]
	f.r.mu.Unlock()
	if p == nil {
		t.Fatal("the placement was not re-adopted")
	}
	time.Sleep(200 * time.Millisecond)
	if got := f.types(); slices.Contains(got, proto.MsgStatus) {
		t.Fatalf("reports %v after the restart: the end was reported with the volume still there", got)
	}
	if err := os.Remove(f.bin + ".failvolrm"); err != nil {
		t.Fatal(err)
	}
	mustEnd(t, p, "the re-adopted placement never ended")
	// The restarted runner says once more that the release fails.
	if got := f.types(); !slices.Equal(got, []string{proto.MsgSnapshotDone, proto.MsgRunEvent, proto.MsgRunEvent, proto.MsgStatus}) {
		t.Fatalf("reports %v, want one snapshot.done, one release failure per runner, then the end", got)
	}
	f.mu.Lock()
	volAt := slices.Clone(f.volAtStatus)
	f.mu.Unlock()
	if !slices.Equal(volAt, []bool{false}) {
		t.Fatalf("state volume present at the end report: %v", volAt)
	}
}
