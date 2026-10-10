package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// A placement's exit snapshot by resumePolicy: a never Run, which nothing
// resumes, exports no state volume (none in its manifest, none uploaded,
// the stale local copy it held no longer claimed) but still reports its
// snapshot with its output, artifacts and output seq, then its end; every
// other policy exports its state volume as before.
func TestExitSnapshotExportsStateVolumesUnlessNever(t *testing.T) {
	for _, c := range []struct {
		policy  string
		volumes bool
	}{
		{"", true}, {"auto", true}, {"restart", true}, {"manual", true}, {"never", false},
	} {
		t.Run(cmp.Or(c.policy, "unset"), func(t *testing.T) {
			f := newFinishFixture(t)
			f.serveUploads(t)
			f.p.assign.Spec.ResumePolicy = c.policy
			f.p.assign.Spec.Artifacts.Paths = []string{"/data/state"}
			f.p.state.VolumesSnapshot, f.p.state.VolumesEpoch = "snap_prev", 0
			writeFile(t, filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1)), `{"seq":1}`+"\n")
			f.release()
			f.p.setPhase("exited")
			go func() {
				defer close(f.p.done)
				f.p.finish(context.Background(), &exitRecord{Code: 0, Reason: "stopped", OutputSeq: 7})
			}()
			mustEnd(t, f.p, "the placement never ended")

			if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
				t.Fatalf("reports %v, want snapshot.done then status", got)
			}
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" || sd.Output == nil {
				t.Fatalf("snapshot.done %+v: want a snapshot id and the output, no error", sd)
			}
			if sd.OutputSeq != 7 {
				t.Errorf("snapshot.done output seq %d, want 7", sd.OutputSeq)
			}
			if len(sd.Artifacts) != 1 || sd.Artifacts[0].Path != "/data/state" {
				t.Fatalf("snapshot.done artifacts %+v, want /data/state", sd.Artifacts)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes {
				t.Errorf("volume export run: %v, want %v; podman:\n%s", exported, c.volumes, f.podmanLog())
			}
			want := []string{sd.Output.BlobID, sd.Artifacts[0].BlobID}
			if c.volumes {
				if len(sd.Manifest.Volumes) != 1 || sd.Manifest.Volumes[0].Name != "data" {
					t.Fatalf("manifest volumes %+v, want data", sd.Manifest.Volumes)
				}
				want = append(want, sd.Manifest.Volumes[0].BlobID)
			} else if len(sd.Manifest.Volumes) != 0 {
				t.Fatalf("manifest volumes %+v, want none", sd.Manifest.Volumes)
			}

			f.r.uploads.pass(context.Background())
			f.mu.Lock()
			uploaded := slices.Clone(f.uploaded)
			f.mu.Unlock()
			slices.Sort(uploaded)
			slices.Sort(want)
			if !slices.Equal(uploaded, want) {
				t.Errorf("uploaded %v, want %v", uploaded, want)
			}

			st, err := readRunState(f.p.dir)
			if err != nil || st.Phase != "reported" {
				t.Fatalf("run state %+v (%v), want reported", st, err)
			}
			wantLocal := ""
			if c.volumes {
				wantLocal = sd.Manifest.SnapshotID
			}
			if st.VolumesSnapshot != wantLocal {
				t.Errorf("local volumes hold snapshot %q, want %q", st.VolumesSnapshot, wantLocal)
			}
			held := slices.ContainsFunc(f.r.localSnapshots(), func(l proto.LocalSnapshot) bool { return l.RunID == "run1" })
			if held != c.volumes {
				t.Errorf("heartbeat lists a local copy: %v, want %v", held, c.volumes)
			}
		})
	}
}

// A placement re-adopted after a restart, its container exited, takes its
// resumePolicy from the spec its run state stored: a never Run exports no
// state volume. A run state with no spec (no assignment to read a policy
// from) exports them, as for a Run that may be resumed.
func TestReadoptedExitSnapshotExportsStateVolumesUnlessNever(t *testing.T) {
	for _, c := range []struct {
		name    string
		spec    *spec.RunSpec
		volumes bool
	}{
		{"never", &spec.RunSpec{ResumePolicy: "never"}, false},
		{"no spec", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFinishFixture(t)
			f.release()
			f.p.state.Phase = "exited"
			f.p.state.Exit = &exitRecord{Code: 0, Reason: "stopped"}
			f.p.state.Spec = c.spec
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
			if (p.assign == nil) != (c.spec == nil) {
				t.Fatalf("re-adopted assignment %+v, want one only from a stored spec", p.assign)
			}
			mustEnd(t, p, "the re-adopted placement never ended")
			if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
				t.Fatalf("reports %v, want snapshot.done then status", got)
			}
			sd := f.snapshotDone(t)
			if sd.Error != "" || sd.Manifest.SnapshotID == "" {
				t.Fatalf("snapshot.done %+v: want a snapshot id, no error", sd)
			}
			exported := strings.Contains(f.podmanLog(), "volume export")
			if exported != c.volumes || (len(sd.Manifest.Volumes) == 1) != c.volumes {
				t.Errorf("volume export run %v, manifest volumes %+v: want exported %v; podman:\n%s",
					exported, sd.Manifest.Volumes, c.volumes, f.podmanLog())
			}
		})
	}
}

// snapshotDone is the first snapshot.done luxd got.
func (f *finishFixture) snapshotDone(t *testing.T) proto.SnapshotDone {
	t.Helper()
	f.mu.Lock()
	i := slices.IndexFunc(f.reports, func(fr proto.Frame) bool { return fr.Type == proto.MsgSnapshotDone })
	var data json.RawMessage
	if i >= 0 {
		data = f.reports[i].Data
	}
	f.mu.Unlock()
	var sd proto.SnapshotDone
	if err := json.Unmarshal(data, &sd); err != nil {
		t.Fatalf("snapshot.done %s: %v", data, err)
	}
	return sd
}
