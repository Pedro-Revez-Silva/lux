package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// overlapFixture is finishFixture's run1 epoch 1 exporting its snapshot
// (blocked) while luxd, having given up on it, assigns epoch 2 to the same
// host. Epoch 1's reports are held until nackHeld.
func newOverlapFixture(t *testing.T) *finishFixture {
	t.Helper()
	f := newFinishFixture(t)
	f.holdReports(1)
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".exporting")
	return f
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond for up to 10 s, and fails the test with msg if it
// never holds.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
	}
}

// mustEnd fails the test with msg unless p ends within 10 s.
func mustEnd(t *testing.T, p *placement, msg string) {
	t.Helper()
	if !p.waitDone(10 * time.Second) {
		t.Fatal(msg)
	}
}

// holdReports has luxd hold epoch's reports unanswered.
func (f *finishFixture) holdReports(epoch int) {
	f.mu.Lock()
	f.holdEpoch = epoch
	f.mu.Unlock()
}

// sendEpoch1Report has epoch 1 send a report, which luxd holds unanswered.
func (f *finishFixture) sendEpoch1Report(t *testing.T) {
	t.Helper()
	f.p.event(context.Background(), "test.ping", nil)
	f.waitHeld(t)
}

// waitHeld waits for luxd to hold a report of holdEpoch's.
func (f *finishFixture) waitHeld(t *testing.T) {
	t.Helper()
	waitFor(t, "epoch 1 sent no report", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.held) > 0
	})
}

// takeHeld ends the hold and returns the reports held so far; accepted,
// they join f.reports.
func (f *finishFixture) takeHeld(accepted bool) []proto.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.held
	f.held, f.holdEpoch = nil, 0
	if accepted {
		f.reports = append(f.reports, held...)
	}
	return held
}

// ackHeld answers every held report as luxd answers an accepted one.
func (f *finishFixture) ackHeld() {
	for _, fr := range f.takeHeld(true) {
		f.r.conn.dispatch(context.Background(), proto.Frame{Type: proto.MsgAck, ID: fr.ID})
	}
}

// nackHeld answers every held report as luxd answers a fenced-off epoch's.
func (f *finishFixture) nackHeld() {
	for _, fr := range f.takeHeld(false) {
		f.r.conn.dispatch(context.Background(), proto.Frame{Type: proto.MsgNack, ID: fr.ID,
			Data: proto.Marshal(proto.Nack{Error: "stale epoch", Stale: true})})
	}
}

// serveUploads has the runner upload blobs to a fake luxd that records
// their ids in f.uploaded.
func (f *finishFixture) serveUploads(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodPut {
			f.mu.Lock()
			f.uploaded = append(f.uploaded, path.Base(r.URL.Path))
			f.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	f.r.api = newAPI(srv.URL, "token", "h1")
}

func (f *finishFixture) podmanLog() string {
	b, _ := os.ReadFile(f.bin + ".log")
	return string(b)
}

// started: epoch 2 has run its first podman command (its network).
func (f *finishFixture) epoch2Started() bool {
	return strings.Contains(f.podmanLog(), "network create")
}

// wantStartedAfterRemoval requires the epoch that started (its network)
// to have done so only after epoch 1 removed its container.
func (f *finishFixture) wantStartedAfterRemoval(t *testing.T, epoch int) {
	t.Helper()
	log := f.podmanLog()
	rm, network := strings.Index(log, "removed ctr-1"), strings.Index(log, "network create")
	if rm < 0 || network < rm {
		t.Fatalf("epoch %d's network at %d, epoch 1's removal at %d: want epoch %d after; podman:\n%s", epoch, network, rm, epoch, log)
	}
}

// newRemovingFixture is finishFixture's run1 epoch 1 after a normal stop:
// luxd has acked its snapshot.done and its status, and it is removing its
// container (held until releaseRemove). Its blobs upload to a fake luxd.
func newRemovingFixture(t *testing.T) *finishFixture {
	t.Helper()
	f := newFinishFixture(t)
	f.serveUploads(t)
	writeFile(t, f.bin+".holdrm", "")
	t.Cleanup(f.releaseRemove)
	f.release()
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".removing")
	if got := f.types(); len(got) != 2 || got[0] != proto.MsgSnapshotDone || got[1] != proto.MsgStatus {
		t.Fatalf("reports %v, want snapshot.done then status acked", got)
	}
	return f
}

func (f *finishFixture) releaseRemove() { _ = os.Remove(f.bin + ".holdrm") }

func (f *finishFixture) assignEpoch(t *testing.T, epoch int) *placement {
	t.Helper()
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: epoch})
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	p := f.r.placements["run1"]
	if p == nil || p.epoch != epoch {
		t.Fatalf("the runner holds %+v, want epoch %d", p, epoch)
	}
	return p
}

// waitLease waits for a heartbeat that leases run1's epoch in state.
func (f *finishFixture) waitLease(t *testing.T, epoch int, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for n := 0; time.Now().Before(deadline); {
		var leases []proto.LivePlacement
		leases, n = f.heartbeatAfter(t, n)
		for _, l := range leases {
			if l.RunID == "run1" && l.Epoch == epoch && l.State == state {
				return
			}
		}
	}
	t.Fatalf("no heartbeat leased run1 epoch %d %s", epoch, state)
}

func (f *finishFixture) wantRunState(t *testing.T, epoch int, stale bool) {
	t.Helper()
	st, err := readRunState(f.p.dir)
	if err != nil || st.Epoch != epoch || st.Stale != stale {
		t.Fatalf("run state %+v (%v), want epoch %d's, stale %v", st, err, epoch, stale)
	}
}

// lastStatus is the last status luxd got about run1's epoch.
func (f *finishFixture) lastStatus(epoch int) *proto.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got *proto.Status
	for _, fr := range f.reports {
		if fr.Type == proto.MsgStatus && fr.Epoch == epoch {
			var st proto.Status
			if err := json.Unmarshal(fr.Data, &st); err == nil {
				got = &st
			}
		}
	}
	return got
}

// wantUploaded runs an upload pass and requires every blob of every
// snapshot record to have reached luxd.
func (f *finishFixture) wantUploaded(t *testing.T) {
	t.Helper()
	f.r.uploads.pass(context.Background())
	recs := f.r.snapshotRecords()
	if len(recs) == 0 {
		t.Fatal("no snapshot record")
	}
	f.mu.Lock()
	uploaded := slices.Clone(f.uploaded)
	f.mu.Unlock()
	for id, rec := range recs {
		for _, up := range rec.Uploads {
			if !slices.Contains(uploaded, up.BlobID) {
				t.Errorf("blob %s of snapshot %s (reported %v) not uploaded; uploaded %v", up.BlobID, id, rec.Reported, uploaded)
			}
		}
	}
}

func (f *finishFixture) wantNoSnapshotRecord(t *testing.T) {
	t.Helper()
	for id, rec := range f.r.snapshotRecords() {
		t.Errorf("snapshot record %s %+v of an abandoned export", id, rec)
	}
}

// The stale nack of an older epoch whose report was in flight when the
// next epoch was assigned kills only the container that epoch made, never
// the Run's name. Here ctr-2 holds the name, as an epoch's container would
// on the readopt of a run state written before the container id was
// recorded; with the handover, a live next epoch has none yet.
func TestStaleEpochDoesNotKillTheNextEpochsContainer(t *testing.T) {
	f := newOverlapFixture(t)
	f.sendEpoch1Report(t)
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	writeFile(t, f.bin+".ctr", "ctr-2")
	f.nackHeld()
	// The fence's kill runs in its own goroutine: wait for it, then
	// require it to be the only one.
	f.waitFile(t, f.bin+".killed")
	if b, _ := os.ReadFile(f.bin + ".killed"); string(b) != "ctr-1\n" {
		t.Errorf("killed %q, want epoch 1's own ctr-1 only; podman:\n%s", b, f.podmanLog())
	}
}

// The next epoch assigned while the previous one, its snapshot and end
// acked by luxd, still removes its container here touches nothing of the
// Run's (no network, volume, container or run state) until that one is
// done; then it starts, and the run state is its own.
func TestNextEpochWaitsForTheFinishingOne(t *testing.T) {
	f := newRemovingFixture(t)
	p2 := f.assignEpoch(t, 2)
	// Epoch 2's first act: its lease in a heartbeat, as starting. A second
	// heartbeat later its goroutine has had time to get past waitPrevious.
	f.waitLease(t, 2, "starting")
	f.heartbeatAfter(t, f.count())
	if f.epoch2Started() {
		t.Fatalf("epoch 2 started while epoch 1 was removing its container; podman:\n%s", f.podmanLog())
	}
	f.wantRunState(t, 1, false)
	f.releaseRemove()
	mustEnd(t, f.p, "epoch 1 never ended")
	mustEnd(t, p2, "epoch 2 never ended")
	f.wantStartedAfterRemoval(t, 2)
	f.wantRunState(t, 2, false)
}

// An earlier epoch that never ends here fails the next epoch's start once
// the bounded wait is over, with the reason; the next epoch never ran, and
// the earlier one's snapshot, which luxd has, still uploads.
func TestNextEpochFailsWhenThePreviousNeverEnds(t *testing.T) {
	f := newRemovingFixture(t)
	f.r.handover = 300 * time.Millisecond
	p2 := f.assignEpoch(t, 2)
	mustEnd(t, p2, "epoch 2 never ended")
	if f.epoch2Started() {
		t.Errorf("epoch 2 ran beside an epoch 1 still removing its container; podman:\n%s", f.podmanLog())
	}
	got := f.lastStatus(2)
	if got == nil || got.State != "failed" || !strings.Contains(got.Message, "epoch 1") {
		t.Fatalf("epoch 2's last status %+v, want failed naming epoch 1", got)
	}
	f.wantRunState(t, 1, false)
	f.releaseRemove()
	mustEnd(t, f.p, "epoch 1 never ended")
	f.wantUploaded(t)
}

// A stop of the next epoch while it waits ends it reported stopped and
// leaves the previous epoch's run state, snapshot and upload alone: the
// snapshot luxd acked is uploaded. So with no stop, while it waits.
func TestAckedSnapshotUploadsAcrossTheHandover(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(map[bool]string{true: "stopped while waiting", false: "while waiting"}[stop], func(t *testing.T) {
			f := newRemovingFixture(t)
			p2 := f.assignEpoch(t, 2)
			if stop {
				p2.requestStop(context.Background(), "stop")
				mustEnd(t, p2, "epoch 2 never ended")
				f.releaseRemove()
				mustEnd(t, f.p, "epoch 1 never ended")
			} else {
				f.waitLease(t, 2, "starting")
			}
			if f.r.staleEpoch("run1") == 1 {
				t.Error("epoch 1's acked snapshot is skipped by the uploader")
			}
			f.wantUploaded(t)
			if !stop {
				f.releaseRemove()
				mustEnd(t, p2, "epoch 2 never ended")
			}
		})
	}
}

// The next epoch assigned while the previous one exports a snapshot luxd
// has not acked (luxd assigns over an unreported placement only once it is
// lost, and refuses its snapshot) does not wait for that export: it is
// abandoned, and its snapshot never reported.
func TestNextEpochAbandonsAnUnackedExport(t *testing.T) {
	f := newOverlapFixture(t)
	f.r.handover = time.Minute
	start := time.Now()
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	if !f.p.waitDone(5 * time.Second) {
		t.Fatal("epoch 1's export was not abandoned")
	}
	for !f.epoch2Started() {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("epoch 2 did not start; podman:\n%s", f.podmanLog())
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	sent := append(slices.Clone(f.held), f.reports...)
	f.mu.Unlock()
	for _, fr := range sent {
		if fr.Epoch == 1 && fr.Type == proto.MsgSnapshotDone {
			t.Error("epoch 1 reported the snapshot it abandoned")
		}
		if fr.Epoch == 1 && fr.Type == proto.MsgStatus {
			t.Error("epoch 1 reported its end after abandoning its snapshot")
		}
	}
	f.wantNoSnapshotRecord(t)
}

// A snapshot.done already sent when the next epoch is assigned is not
// abandoned, though luxd has not acked it yet: only an unsent snapshot is.
// luxd's later ack makes it luxd's, and its blobs upload.
func TestASentSnapshotDoneSurvivesTheNextAssign(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	f.holdReports(1)
	f.release()
	f.exitedAsSupervised(context.Background())
	f.waitHeld(t)
	p2 := f.assignEpoch(t, 2)
	f.ackHeld()
	mustEnd(t, f.p, "epoch 1 never ended")
	if got := f.types(); len(got) < 1 || got[0] != proto.MsgSnapshotDone {
		t.Fatalf("reports %v, want epoch 1's snapshot.done first", got)
	}
	f.wantUploaded(t)
	mustEnd(t, p2, "epoch 2 never ended")
}

// A snapshot abandoned after some of its volumes were exported leaves none
// of their blobs: no record lists them, so nothing else would remove them.
func TestAbandonedExportRemovesEarlierVolumesBlobs(t *testing.T) {
	f := newFinishFixture(t)
	f.r.handover = time.Minute
	first := volumeName("run1", "data")
	f.p.state.Volumes = append(f.p.state.Volumes, volumeRef{Name: "data2", Volume: volumeName("run1", "data2"), Path: "/data2", Kind: "state"})
	writeFile(t, f.bin+".fast."+first, "")
	f.holdReports(1)
	f.exitedAsSupervised(context.Background())
	// The first volume's export is done once the second's blocks.
	f.waitFile(t, f.bin+".exporting")
	p2 := f.assignEpoch(t, 2)
	mustEnd(t, f.p, "epoch 1's export was not abandoned")
	mustEnd(t, p2, "epoch 2 never ended")
	if !strings.Contains(f.podmanLog(), "volume export "+first+"\n") {
		t.Fatalf("the first volume was not exported; podman:\n%s", f.podmanLog())
	}
	entries, err := os.ReadDir(f.r.cfg.DataDir + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zst") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("%s left in snapshots by an abandoned export", e.Name())
		}
	}
	f.wantNoSnapshotRecord(t)
}

// A placement fenced off while it exports its snapshot writes no run state
// from then on: the Run's next placement here owns it.
func TestFencedPlacementLeavesTheRunStateAlone(t *testing.T) {
	f := newFinishFixture(t)
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".exporting")
	f.p.markStale()
	next := &runState{RunID: "run1", TenantID: "t1", Epoch: 2, Phase: "started", Container: "ctr-2"}
	if err := writeRunState(f.p.dir, next); err != nil {
		t.Fatal(err)
	}
	f.p.requestStop(context.Background(), "stop")
	f.p.mark("late")
	f.release()
	mustEnd(t, f.p, "epoch 1 never ended")
	st, err := readRunState(f.p.dir)
	if err != nil || st.Epoch != 2 || st.Container != "ctr-2" || st.Stale || st.VolumesSnapshot != "" || st.StopReason != "" {
		t.Fatalf("run state %+v (%v), want epoch 2's as written", st, err)
	}
}

// A placement fenced off before its container was created (its create
// failed, or never came) kills nothing: not the Run's name, which another
// epoch's container may hold.
func TestKillWithoutAContainerKillsNothing(t *testing.T) {
	f := newFinishFixture(t)
	f.p.mu.Lock()
	f.p.state.Container = ""
	f.p.mu.Unlock()
	f.p.markStale()
	f.p.kill(context.Background())
	f.p.sendStop(context.Background(), "stop")
	time.Sleep(200 * time.Millisecond)
	for _, l := range strings.Split(f.podmanLog(), "\n") {
		if strings.HasPrefix(l, "kill ") || strings.HasPrefix(l, "stop ") {
			t.Errorf("podman %q for a placement with no container", l)
		}
	}
}

// A stop or a terminate of the next epoch while it waits for the previous
// one ends it at once, reported stopped; it ran nothing and wrote nothing.
func TestNextEpochStoppedWhileWaiting(t *testing.T) {
	for _, reason := range []string{"stop", "terminate"} {
		t.Run(reason, func(t *testing.T) {
			f := newRemovingFixture(t)
			p2 := f.assignEpoch(t, 2)
			p2.requestStop(context.Background(), reason)
			mustEnd(t, p2, "epoch 2 kept waiting after its stop")
			if got := f.lastStatus(2); got == nil || got.State != "exited" || got.Reason != "stopped" {
				t.Errorf("epoch 2's last status %+v, want exited, stopped", got)
			}
			if f.epoch2Started() {
				t.Errorf("a stopped epoch 2 ran; podman:\n%s", f.podmanLog())
			}
			f.wantRunState(t, 1, false)
		})
	}
}

// A third epoch assigned while the second waits for the first fences the
// second off (it reports nothing) and waits for the first itself.
func TestThirdEpochWaitsForTheFirst(t *testing.T) {
	f := newRemovingFixture(t)
	p2 := f.assignEpoch(t, 2)
	p3 := f.assignEpoch(t, 3)
	mustEnd(t, p2, "the fenced epoch 2 kept waiting")
	// A heartbeat later, epoch 3 still waits.
	f.heartbeatAfter(t, f.count())
	if f.epoch2Started() {
		t.Fatalf("epoch 3 started while epoch 1 was removing its container; podman:\n%s", f.podmanLog())
	}
	f.waitLease(t, 3, "starting")
	f.wantRunState(t, 1, false)
	f.releaseRemove()
	mustEnd(t, p3, "epoch 3 never ended")
	f.wantStartedAfterRemoval(t, 3)
	if got := f.lastStatus(2); got != nil {
		t.Errorf("the fenced epoch 2 reported %+v", got)
	}
	f.wantRunState(t, 3, false)
}

// A placement acts on its own container by the id it created, never by the
// Run's name: kill, the podman stop of sendStop, sampleSlow's probes, and
// supervise's wait and OOM inspect.
func TestPlacementTargetsItsContainerByID(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.kill(context.Background())
	f.p.sendStop(context.Background(), "stop")
	f.p.sampleSlow(context.Background())
	f.p.setPhase("running")
	go func() {
		defer close(f.p.done)
		f.p.supervise(context.Background())
	}()
	mustEnd(t, f.p, "the placement never ended")
	want := []string{"kill -s KILL", "stop -t", "container inspect --size", "stats --no-stream", "wait", "container inspect", "rm -f"}
	seen := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < len(want) && time.Now().Before(deadline) {
		for _, l := range strings.Split(f.podmanLog(), "\n") {
			for _, w := range want {
				if strings.HasPrefix(l, w+" ") && strings.HasSuffix(l, " ctr-1") {
					seen[w] = true
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("no podman %q on ctr-1", w)
		}
	}
	for _, l := range strings.Split(f.podmanLog(), "\n") {
		if strings.HasSuffix(l, " "+containerName("run1")) {
			t.Errorf("podman %q by the Run's name", l)
		}
	}
}

// A run state written before the container id was recorded is re-adopted
// by the Run's container name: its container is supervised and killed.
func TestReadoptWithoutAContainerIDUsesTheRunsName(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.state.Container = ""
	if err := writeRunState(f.p.dir, f.p.state); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.bin+".inspect."+containerName("run1"), `[{"State":{"Status":"exited"}}]`)
	f.r.mu.Lock()
	f.r.placements = map[string]*placement{}
	f.r.mu.Unlock()
	f.r.readopt(context.Background())
	p := f.r.placement("run1", 1)
	if p == nil {
		t.Fatalf("the started placement was not re-adopted; podman:\n%s", f.podmanLog())
	}
	mustEnd(t, p, "the re-adopted placement never ended")
	p.kill(context.Background())
	if !strings.Contains(f.podmanLog(), "wait "+containerName("run1")+"\n") {
		t.Errorf("the re-adopted container was not waited on by name; podman:\n%s", f.podmanLog())
	}
	if b, _ := os.ReadFile(f.bin + ".killed"); string(b) != containerName("run1")+"\n" {
		t.Errorf("killed %q, want the Run's name", b)
	}
}

// A placement fenced off while it starts stops what is under way (here,
// its network's creation): it would make a container for nothing.
func TestFencedStartingPlacementStopsItsStart(t *testing.T) {
	f := newFinishFixture(t)
	f.r.mu.Lock()
	f.r.placements = map[string]*placement{}
	f.r.mu.Unlock()
	writeFile(t, f.bin+".holdnet", "")
	t.Cleanup(func() { _ = os.Remove(f.bin + ".holdnet") })
	p2 := f.assignEpoch(t, 2)
	f.waitFile(t, f.bin+".netcreating")
	p2.markStale()
	mustEnd(t, p2, "the fenced placement's start went on")
	if got := f.lastStatus(2); got != nil && got.State != "starting" {
		t.Errorf("the fenced placement reported its end: %+v", got)
	}
}
