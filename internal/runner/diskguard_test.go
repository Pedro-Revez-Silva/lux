package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/runner/disklimit"
	"github.com/marcioapm/lux/internal/spec"
)

const mib = int64(1) << 20

// fakeFS drives a diskGuard: free is what the next measurements answer
// (the last value repeats), and every guard action is logged in order.
type fakeFS struct {
	frees    []int64
	measured int
	calls    []string
	reclaims bool // what reclaim answers
	copies   []localCopy
	victims  []diskVictim
	stopped  []string
	gen      uint64
	// onStop and onDiscard, when set, run after each stop or discard (the
	// filesystem changing between two actions).
	onStop    func(f *fakeFS)
	onDiscard func(f *fakeFS)
}

func (f *fakeFS) free() (int64, error) {
	i := min(f.measured, len(f.frees)-1)
	f.measured++
	return f.frees[i], nil
}

// guard is a guard over a 1000 MiB filesystem, low 50 MiB, recover 100
// MiB, acting on f.
func (f *fakeFS) guard(t *testing.T) *diskGuard {
	t.Helper()
	g, err := newDiskGuard(slog.New(slog.DiscardHandler), FreeMark{Bytes: 50 * mib}, FreeMark{Bytes: 100 * mib}, 1000*mib)
	if err != nil {
		t.Fatal(err)
	}
	g.free = f.free
	g.publish = func(context.Context) {
		state := "clear"
		if p := g.state(); p != nil {
			state = fmt.Sprintf("pressure noRelief=%v", p.NoRelief)
		}
		f.calls = append(f.calls, "publish "+state)
	}
	g.reclaim = func(_ context.Context, need int64) bool {
		f.calls = append(f.calls, fmt.Sprintf("reclaim %d MiB", need/mib))
		return f.reclaims
	}
	g.copies = func() []localCopy { return f.copies }
	g.discard = func(_ context.Context, runID string) bool {
		f.calls = append(f.calls, "discard "+runID)
		f.copies = slices.DeleteFunc(f.copies, func(c localCopy) bool { return c.runID == runID })
		if f.onDiscard != nil {
			f.onDiscard(f)
		}
		return true
	}
	g.victims = func() []diskVictim { return f.victims }
	g.stop = func(_ context.Context, v diskVictim, _ proto.DiskPressure) {
		f.calls = append(f.calls, "stop "+v.runID)
		f.stopped = append(f.stopped, v.runID)
		if f.onStop != nil {
			f.onStop(f)
		}
	}
	g.gen = func() uint64 { return f.gen }
	return g
}

// victim is a placement held to limit MiB that used used MiB, rw MiB of
// them its writable layer.
func victim(id string, used, rw, limit int64) diskVictim {
	return diskVictim{runID: id, used: used * mib, rw: rw * mib, limit: disklimit.Limit{Nominal: limit * mib, Effective: limit * mib}}
}

// Pressure starts only below the low mark, holds between the marks, and
// ends only at the recovery mark; each change is published at once.
func TestDiskGuardHysteresis(t *testing.T) {
	f := &fakeFS{}
	g := f.guard(t)
	for _, step := range []struct {
		free     int64
		pressure bool
	}{
		{200, false},
		{50, false}, // at the low mark is not below it
		{49, true},  // below: pressure starts
		{60, true},  // above low, below recover: holds
		{99, true},
		{100, false}, // at the recovery mark: ends
		{60, false},  // between the marks again: no pressure until below low
		{10, true},
	} {
		f.frees = []int64{step.free * mib}
		g.pass(context.Background())
		if got := g.state() != nil; got != step.pressure {
			t.Fatalf("free %d MiB: pressure %v, want %v", step.free, got, step.pressure)
		}
		if p := g.state(); p != nil && p.FreeBytes != step.free*mib {
			t.Errorf("free %d MiB: state says %d", step.free, p.FreeBytes)
		}
	}
	want := []string{"publish pressure noRelief=false", "publish clear", "publish pressure noRelief=false"}
	var published []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "publish") {
			published = append(published, c)
		}
	}
	// noRelief flips to true and back as passes find nothing: count only
	// the transitions of pressure itself here.
	published = slices.DeleteFunc(published, func(s string) bool { return s == "publish pressure noRelief=true" })
	if !slices.Equal(published, want) {
		t.Errorf("published %v, want %v", published, want)
	}
	p := g.state()
	if p.LowFreeBytes != 50*mib || p.RecoverFreeBytes != 100*mib || p.LowFree != "50.0 MiB" || p.RecoverFree != "100.0 MiB" {
		t.Errorf("state %+v", p)
	}
}

// On entering pressure the guard blocks admissions first (publishes),
// then reclaims images and S3-held local copies toward the recovery mark,
// then stops over-limit placements; in that order, on the same pass.
func TestDiskGuardOrderBlockReclaimStop(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib}, reclaims: true, victims: []diskVictim{victim("over", 300, 280, 100)},
		copies: []localCopy{{runID: "gone", reclaimable: true}}}
	f.guard(t).pass(context.Background())
	want := []string{"publish pressure noRelief=false", "reclaim 60 MiB", "discard gone", "stop over"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
}

// Reclaim that reaches the recovery mark stops nothing: the filesystem is
// measured again after it.
func TestDiskGuardReclaimEnoughStopsNothing(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib, 120 * mib}, reclaims: true, victims: []diskVictim{victim("over", 300, 300, 100)},
		copies: []localCopy{{runID: "gone", reclaimable: true}}}
	f.guard(t).pass(context.Background())
	if len(f.stopped) != 0 || slices.Contains(f.calls, "discard gone") {
		t.Fatalf("calls %v after reclaiming images to the recovery mark, want no discard and no stop", f.calls)
	}
}

// Free space is measured again after the image reclaim even when it
// removed nothing: a stopping container's removal may have freed enough
// meanwhile, and then nothing is stopped.
func TestDiskGuardRemeasuresAfterAFruitlessReclaim(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib, 120 * mib}, victims: []diskVictim{victim("over", 300, 300, 100)}}
	f.guard(t).pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v on a stale measurement", f.stopped)
	}
}

// Local copies go oldest exit first, only those S3 holds, and only while
// the filesystem lacks space: a copy whose upload is pending is never
// discarded, however low the disk.
func TestDiskGuardReclaimsOnlyUploadedCopies(t *testing.T) {
	f := &fakeFS{frees: []int64{40 * mib}, copies: []localCopy{
		{runID: "old", exitAt: 1, reclaimable: true},
		{runID: "pending", exitAt: 2, awaiting: 10 * mib},
		{runID: "newer", exitAt: 3, reclaimable: true},
		{runID: "newest", exitAt: 4, reclaimable: true},
	}}
	// The second discard brings free space back to the recovery mark.
	f.onDiscard = func(f *fakeFS) {
		if len(f.calls) == 4 {
			f.frees = []int64{100 * mib}
		}
	}
	f.guard(t).pass(context.Background())
	want := []string{"publish pressure noRelief=false", "reclaim 60 MiB", "discard old", "discard newer", "publish clear"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
}

// Victims are taken largest excess first; after each the filesystem is
// measured again, and the guard stops only as many as what their stops
// free at once (their writable layers) leaves lacking.
func TestDiskGuardLargestExcessFirstWithRemeasure(t *testing.T) {
	vs := []diskVictim{victim("small", 110, 110, 100), victim("large", 400, 300, 100), victim("mid", 150, 150, 100)}
	t.Run("writable layer alone", func(t *testing.T) {
		// 60 MiB short of recover: large's 300 MiB layer covers it.
		f := &fakeFS{frees: []int64{40 * mib}, victims: vs}
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large"}) {
			t.Fatalf("stopped %v, want [large]", f.stopped)
		}
	})
	t.Run("in order of excess", func(t *testing.T) {
		// Layers 300, 150, 110 MiB do not cover a 600 MiB shortfall:
		// every one goes, largest excess first.
		f := &fakeFS{frees: []int64{-500 * mib}, victims: vs}
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large", "mid", "small"}) {
			t.Fatalf("stopped %v, want [large mid small]", f.stopped)
		}
	})
	t.Run("measured again between victims", func(t *testing.T) {
		// 600 MiB short; after the first stop the filesystem has freed
		// enough (another Run ended): no second victim.
		f := &fakeFS{frees: []int64{-500 * mib}, victims: vs}
		f.onStop = func(f *fakeFS) { f.frees = []int64{150 * mib} }
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"large"}) {
			t.Fatalf("stopped %v, want [large]: the second measurement shows enough free", f.stopped)
		}
	})
	t.Run("excess is not what a stop frees", func(t *testing.T) {
		// 60 MiB short. big is 300 MiB over, but its writable layer is
		// 10 MiB: the rest is state volumes, which stay as its local copy.
		// So the guard stops the next one too.
		f := &fakeFS{frees: []int64{40 * mib}, victims: []diskVictim{victim("big", 400, 10, 100), victim("next", 200, 80, 100)}}
		f.guard(t).pass(context.Background())
		if !slices.Equal(f.stopped, []string{"big", "next"}) {
			t.Fatalf("stopped %v, want [big next]", f.stopped)
		}
	})
}

// A placement already stopping counts what its stop frees at once (its
// writable layer) toward what is needed: with that covering the
// shortfall, no other placement is stopped; with it short, only what
// remains. Once it has ended (no longer live) its local copy, still
// uploading, counts its volumes; once uploaded, the copy is discarded
// before any other Run is stopped.
func TestDiskGuardCountsPlacementsAlreadyStopping(t *testing.T) {
	going := victim("going", 300, 70, 100)
	going.stopping = true
	f := &fakeFS{frees: []int64{40 * mib}, victims: []diskVictim{going, victim("other", 400, 400, 100)}}
	g := f.guard(t)
	g.pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v while a stopping placement's writable layer covers the need", f.stopped)
	}
	if g.state().NoRelief {
		t.Error("a stopping placement is relief, reported as none")
	}
	going.rw = 20 * mib // 40 MiB still lacking
	f.victims = []diskVictim{going, victim("other", 400, 400, 100), victim("third", 200, 200, 100)}
	f.gen++
	g.pass(context.Background())
	if !slices.Equal(f.stopped, []string{"other"}) {
		t.Fatalf("stopped %v, want [other]", f.stopped)
	}

	// going has ended: no longer live, its container gone but not its
	// volumes (230 MiB), which wait for their upload. Free space is still
	// short: the guard waits for that upload rather than stopping third.
	f.victims = []diskVictim{victim("third", 200, 200, 100)}
	f.copies = []localCopy{{runID: "going", awaiting: 230 * mib}}
	f.calls, f.stopped = nil, nil
	f.gen++
	g.pass(context.Background())
	if len(f.stopped) != 0 || slices.Contains(f.calls, "discard going") {
		t.Fatalf("calls %v while going's copy uploads, want no discard and no stop", f.calls)
	}
	// Uploaded: its copy goes first, which is enough.
	f.copies = []localCopy{{runID: "going", reclaimable: true}}
	f.onDiscard = func(f *fakeFS) { f.frees = []int64{110 * mib} }
	f.calls = nil
	f.gen++
	g.pass(context.Background())
	if !slices.Equal(f.calls, []string{"reclaim 60 MiB", "discard going", "publish clear"}) || len(f.stopped) != 0 {
		t.Fatalf("calls %v stopped %v, want going's copy discarded and nothing stopped", f.calls, f.stopped)
	}
}

// A placement at or under its effective limit is never stopped, however
// low the disk; with nothing to reclaim and no over-limit placement, the
// pressure is reported unrelieved and admissions stay blocked. Until
// something changes the guard does not look again, for at most
// guardIdleRetry.
func TestDiskGuardNeverStopsConformingAndReportsNoRelief(t *testing.T) {
	f := &fakeFS{frees: []int64{1 * mib}, victims: []diskVictim{victim("at", 100, 100, 100), victim("under", 10, 10, 100)}}
	g := f.guard(t)
	g.pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped conforming placements %v", f.stopped)
	}
	p := g.state()
	if p == nil || !p.NoRelief {
		t.Fatalf("state %+v, want pressure with no relief", p)
	}
	want := []string{"publish pressure noRelief=false", "reclaim 99 MiB", "publish pressure noRelief=true"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	// Nothing changed: the next pass neither reclaims nor publishes.
	g.pass(context.Background())
	if g.state() == nil || !g.state().NoRelief || len(f.calls) != 3 {
		t.Fatalf("second pass, nothing changed: state %+v calls %v", g.state(), f.calls)
	}
	// Something changed (an image, a placement, a record): it looks again.
	f.gen++
	g.pass(context.Background())
	if len(f.calls) != 4 || f.calls[3] != "reclaim 99 MiB" {
		t.Fatalf("third pass after a change: calls %v", f.calls)
	}
	// Nothing changed, but the last look is older than guardIdleRetry.
	g.idleSince = time.Now().Add(-guardIdleRetry)
	g.pass(context.Background())
	if len(f.calls) != 5 || f.calls[4] != "reclaim 99 MiB" {
		t.Fatalf("pass after guardIdleRetry: calls %v", f.calls)
	}
}

// Watermarks are refused unless 0 < low < recover < capacity, as resolved
// against the filesystem; same-unit marks are checked before it is read.
func TestDiskGuardMarks(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		low, rec string
		capacity int64
		ok       bool
	}{
		{"5%", "10%", 100 * gib, true},
		{"8GiB", "16GiB", 100 * gib, true},
		{"5%", "16GiB", 100 * gib, true},
		{"10%", "5%", 100 * gib, false},
		{"5%", "5%", 100 * gib, false},
		{"8GiB", "8GiB", 100 * gib, false},
		{"8GiB", "5%", 100 * gib, false},     // 8 GiB is above 5 GiB
		{"1GiB", "200GiB", 100 * gib, false}, // recover past the capacity
		{"5%", "10%", 10, false},             // 0 bytes low
	} {
		low, err := ParseFreeMark(c.low)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := ParseFreeMark(c.rec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = newDiskGuard(logger, low, rec, c.capacity)
		if (err == nil) != c.ok {
			t.Errorf("low %s recover %s on %d bytes: err %v, want ok %v", c.low, c.rec, c.capacity, err, c.ok)
		}
	}
	for _, s := range []string{"0%", "100%", "-1%", "x%", "0", "-5", "lots", ""} {
		if m, err := ParseFreeMark(s); err == nil {
			t.Errorf("ParseFreeMark(%q) = %+v, want refused", s, m)
		}
	}
	if m, _ := ParseFreeMark("8GiB"); m.Bytes != 8*gib {
		t.Errorf("8GiB = %+v", m)
	}
	if m, _ := ParseFreeMark(" 2.5% "); m.Percent != 2.5 {
		t.Errorf("2.5%% = %+v", m)
	}
	if err := CheckFreeMarks(FreeMark{Percent: 10}, FreeMark{Percent: 5}); err == nil {
		t.Error("CheckFreeMarks 10% over 5%: accepted")
	}
}

// The guard's victims (overLimit of diskVictims) are only live placements
// whose latest sample is over their effective limit, marking those already
// on their way out: stopping, asked to stop, or reported over their grace.
func TestDiskVictimsOnlyOverTheLimit(t *testing.T) {
	f := newDiskFixture(t)
	f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib) // 10 GiB asked holds to 9.375
	const effective = diskLimit / 16 * 15
	add := func(id string, used int64, phase string) *placement {
		sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(diskLimit), DiskGrace: spec.SetDuration(diskGrace)}}
		p := newPlacement(f.r, proto.Assign{RunID: id, TenantID: "t1", Epoch: 1, Spec: sp}, nil)
		p.state = &runState{RunID: id, Epoch: 1}
		p.phase = phase
		p.lastDisk = used
		f.r.placements[id] = p
		return p
	}
	add("at", effective, "running")
	add("over", effective+1, "running")
	add("stopping", effective+5, "stopping")
	asked := add("asked", effective+7, "running")
	asked.stopWhy = "stop"
	add("nolimit", 1<<40, "running").assign.Spec.Resources.Disk = 0
	// Over for its whole grace: reported as disk.exceeded, so luxd stops it.
	reported := add("reported", 0, "running")
	t0 := time.Now()
	reported.checkDisk(f.ctx, effective+3, t0)
	reported.checkDisk(f.ctx, effective+3, t0.Add(diskGrace))
	got := map[string]bool{}
	for _, v := range overLimit(f.r.diskVictims()) {
		got[v.runID] = v.stopping
		if v.limit.Effective != effective {
			t.Errorf("%s held to %d, want %d", v.runID, v.limit.Effective, effective)
		}
	}
	want := map[string]bool{"over": false, "stopping": true, "asked": true, "reported": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("victims %v, want %v", got, want)
	}
}

// A placement once over its limit, whose next sample is inconclusive (a
// probe failed, what was measured under the limit), is no victim: its
// size is unknown, and it may have gone back under.
func TestDiskVictimsForgetAnInconclusiveSample(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	withVolume(p)
	f.r.placements["run1"] = p
	f.size(t, diskOver)
	p.sampleSlow(f.ctx)
	if vs := overLimit(f.r.diskVictims()); len(vs) != 1 || vs[0].used != diskOver || vs[0].rw != diskOver {
		t.Fatalf("victims %+v after a sample over, want run1 at %d", vs, diskOver)
	}
	// The writable layer's probe fails; the volume measures 0, under it.
	f.flag(t, "failsize")
	p.sampleSlow(f.ctx)
	if vs := overLimit(f.r.diskVictims()); len(vs) != 0 {
		t.Fatalf("victims %+v after an inconclusive sample under the limit, want none", vs)
	}
}

// pressureStop marks the placement (so the next pass counts it as
// stopping) and reports disk.pressure_stop with what it used and was held
// to, the excess and the filesystem's marks it is given.
func TestPressureStopReportsToLuxd(t *testing.T) {
	f := newDiskFixture(t)
	f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib)
	p := f.placement(1)
	f.r.placements["run1"] = p
	p.lastDisk = 20 * gib
	vs := overLimit(f.r.diskVictims())
	if len(vs) != 1 || vs[0].stopping {
		t.Fatalf("victims %+v", vs)
	}
	f.r.pressureStop(f.ctx, vs[0], proto.DiskPressure{FreeBytes: gib, LowFreeBytes: 8 * gib, RecoverFreeBytes: 16 * gib})
	if vs := overLimit(f.r.diskVictims()); len(vs) != 1 || !vs[0].stopping {
		t.Fatalf("after the stop: victims %+v, want it stopping", vs)
	}
	var ev proto.RunEvent
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if evs := f.runEvents(proto.EvDiskPressureStop); len(evs) > 0 {
			ev = evs[0]
			break
		}
	}
	effective := diskLimit / 16 * 15
	want := map[string]any{"usedBytes": float64(20 * gib), "limitBytes": float64(effective), "excessBytes": float64(20*gib - effective),
		"freeBytes": float64(gib), "lowFreeBytes": float64(8 * gib), "recoverFreeBytes": float64(16 * gib),
		"nominalLimitBytes": float64(diskLimit), "limitFactor": 0.9375}
	if fmt.Sprint(ev.Data) != fmt.Sprint(want) {
		t.Fatalf("event %+v, want data %v", ev, want)
	}
}

// A Run the guard stopped keeps its state volumes as its local copy. Under
// pressure, the next pass leaves that copy while its snapshot's upload is
// pending, and removes it (its volumes, its run state, its snapshot files)
// once S3 holds it; the next heartbeat lists no local copy for luxd.
func TestDiskGuardReclaimsAPressureStoppedRunsUploadedCopy(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	data := volumeName("run1", "data")
	script, _ := os.ReadFile(f.bin)
	writeFile(t, f.bin, strings.Replace(string(script), `"volume rm")`, `"volume ls") echo `+data+` ;;
"volume rm")`, 1))
	if err := os.Chmod(f.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	f.p.mu.Lock()
	f.p.pressureStop = true
	f.p.mu.Unlock()
	f.release()
	f.exitedAsSupervised(context.Background())
	mustEnd(t, f.p, "the placement never ended")

	g, err := newDiskGuard(f.r.log, FreeMark{Bytes: 50 * mib}, FreeMark{Bytes: 100 * mib}, 1000*mib)
	if err != nil {
		t.Fatal(err)
	}
	f.r.attachGuard(g, func() (int64, error) { return 10 * mib, nil })
	f.r.guardPass(context.Background())
	if g.state() == nil {
		t.Fatal("no pressure at 10 MiB free")
	}
	if log := f.podmanLog(); strings.Contains(log, "volume rm") {
		t.Fatalf("the copy was removed with its upload pending; podman:\n%s", log)
	}
	if got := f.r.localSnapshots(); len(got) != 1 {
		t.Fatalf("local copies %+v with the upload pending, want run1's", got)
	}
	if p := g.state(); p.NoRelief {
		t.Error("a stopped Run's copy awaiting its upload is relief, reported as none")
	}

	f.r.uploads.pass(context.Background())
	f.r.guardPass(context.Background())
	if log := f.podmanLog(); !strings.Contains(log, "volume rm -f "+data) {
		t.Fatalf("the uploaded copy's volume was not removed; podman:\n%s", log)
	}
	if got := f.r.localSnapshots(); len(got) != 0 {
		t.Fatalf("local copies %+v once discarded, want none", got)
	}
	if recs := f.r.snapshotRecords(); len(recs) != 0 {
		t.Fatalf("snapshot records %+v kept after the discard", recs)
	}
}

// podmanUsage is a host's usage with its filesystem's free bytes.
func podmanUsage() podman.HostUsage { return podman.HostUsage{DiskBytes: 1, DiskFree: 5, DiskOK: true} }

// guardRig is a diskFixture runner with its guard on (low 5%, recover 10%
// of a 160 GiB filesystem), whose free bytes are free; heartbeats go to
// the fixture's fake luxd.
func guardRig(t *testing.T, free *atomic.Int64) *diskFixture {
	t.Helper()
	f := newDiskFixture(t)
	f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib)
	f.r.git = gitws.New(f.r.cfg.DataDir)
	f.r.hbNow = make(chan struct{}, 1)
	f.r.images = newImageUse(f.r.cfg.DataDir)
	g, err := newDiskGuard(f.r.log, DefaultDiskLowFree, DefaultDiskRecoverFree, 160*gib)
	if err != nil {
		t.Fatal(err)
	}
	f.r.attachGuard(g, func() (int64, error) { return free.Load(), nil })
	return f
}

// pressureBeats is the disk pressure of every heartbeat luxd got so far.
func (f *diskFixture) pressureBeats() []*proto.DiskPressure {
	f.luxd.mu.Lock()
	defer f.luxd.mu.Unlock()
	var out []*proto.DiskPressure
	for _, fr := range f.luxd.frames {
		var hb proto.Heartbeat
		if fr.Type == proto.MsgHeartbeat && json.Unmarshal(fr.Data, &hb) == nil && hb.Usage != nil {
			out = append(out, hb.Usage.DiskPressure)
		}
	}
	return out
}

// A drop below the low mark is published while a round of disk samples is
// stuck (a podman that ignores its cancel): the guard watches the
// filesystem on its own ticker, not after the samples.
func TestDiskGuardPublishesWhileSamplingIsStuck(t *testing.T) {
	var free atomic.Int64
	free.Store(100 * gib)
	f := guardRig(t, &free)
	f.r.cfg.UsageEvery = 50 * time.Millisecond
	f.r.guardTick = 20 * time.Millisecond
	// container inspect hangs, deaf to SIGTERM, until $0.hang goes.
	hang := strings.Replace(podmanForDiskSamples, `"container inspect")`,
		`"container inspect")
  touch "$0.hanging"; trap '' TERM; while [ -e "$0.hang" ]; do sleep 0.02; done`, 1)
	writeFile(t, f.bin, hang)
	f.flag(t, "hang")
	t.Cleanup(func() { _ = os.Remove(f.bin + ".hang") })
	f.size(t, 1)
	p := f.placement(1)
	f.r.placements["run1"] = p

	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); _ = os.Remove(f.bin + ".hang"); wg.Wait() })
	f.r.startGuard(ctx)
	wg.Go(func() { f.r.heartbeatLoop(ctx) })
	wg.Go(func() { f.r.usageLoop(ctx) })
	waitFor(t, "the sample round never reached podman", func() bool {
		_, err := os.Stat(f.bin + ".hanging")
		return err == nil
	})
	free.Store(gib) // under 5% of 160 GiB
	waitFor(t, "no heartbeat carried the pressure while the samples were stuck", func() bool {
		for _, dp := range f.pressureBeats() {
			if dp != nil && dp.FreeBytes == gib {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(f.bin + ".hang"); err != nil {
		t.Fatal("the sample round ended before the pressure was published")
	}
}

// A runner restarted under pressure measures the filesystem before its
// first heartbeat: that heartbeat already carries the pressure, so luxd
// never sees it end for a while.
func TestDiskGuardRestartReportsPressureAtOnce(t *testing.T) {
	var free atomic.Int64
	free.Store(gib)
	f := guardRig(t, &free)
	f.r.guardTick = time.Hour
	if u := f.r.hostUsage(podmanUsage()); u.DiskPressure != nil {
		t.Fatalf("pressure %+v before the guard started", u.DiskPressure)
	}
	f.r.startGuard(f.ctx)
	if u := f.r.hostUsage(podmanUsage()); u.DiskPressure == nil || u.DiskPressure.FreeBytes != gib {
		t.Fatalf("first heartbeat's pressure %+v, want it at %d free", u.DiskPressure, gib)
	}
}

// A runner without disk scaling has no guard: its heartbeat never carries
// pressure, and New sets none up.
func TestNoGuardWithoutDiskScale(t *testing.T) {
	off := &Runner{cfg: Config{Disk: 160 * gib}}
	if u := off.hostUsage(podmanUsage()); u.DiskPressure != nil {
		t.Fatalf("legacy runner reported pressure %+v", u.DiskPressure)
	}
	off.guardPass(context.Background()) // no guard: nothing to do, no panic
	on := &Runner{cfg: Config{Disk: 160 * gib}, disk: mustDiskScale(t, 160*gib, 10*gib, 160*gib)}
	g, err := newDiskGuard(slog.New(slog.DiscardHandler), DefaultDiskLowFree, DefaultDiskRecoverFree, 160*gib)
	if err != nil {
		t.Fatal(err)
	}
	on.guard = g
	g.observe(gib)
	u := on.hostUsage(podmanUsage())
	if u.DiskPressure == nil || u.DiskPressure.FreeBytes != gib || u.DiskPressure.LowFree != "5%" || u.DiskPressure.RecoverFree != "10%" {
		t.Fatalf("pressure %+v", u.DiskPressure)
	}
}

// copyRun writes a reported Run's state, exited at exitAt, whose volumes
// hold volSnap ("" none) under resume policy policy.
func copyRun(t *testing.T, r *Runner, runID string, exitAt int64, volSnap, policy string) {
	t.Helper()
	st := &runState{RunID: runID, Epoch: 1, Phase: "reported", LastExitAt: exitAt, VolumesSnapshot: volSnap,
		Spec: &spec.RunSpec{ResumePolicy: policy}}
	if err := writeRunState(r.runDir(runID), st); err != nil {
		t.Fatal(err)
	}
}

func saveRecord(t *testing.T, r *Runner, snapID string, rec *snapshotRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(r.cfg.DataDir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.saveSnapshotRecord(snapID, rec); err != nil {
		t.Fatal(err)
	}
}

// A local copy is reclaimable only when S3 holds everything it has: its
// volumes are the snapshot of a record of its own Run, and every record of
// the Run is reported and uploaded; or its Run is never resumed. Volumes
// whose export failed are the Run's only copy. A copy whose volumes'
// snapshot is uploading awaits that snapshot's blob sizes.
func TestLocalCopiesHeldInS3(t *testing.T) {
	done := []pendingUpload{{BlobID: "b1", Size: 3 * mib, Done: true}}
	for _, c := range []struct {
		name        string
		volSnap     string
		policy      string
		records     map[string]*snapshotRecord
		reclaimable bool
		awaiting    int64
	}{
		{name: "resumable, export failed, no records", policy: spec.ResumeAuto},
		{name: "never resumed, no volumes snapshot", policy: spec.ResumeNever, reclaimable: true},
		{name: "reported and uploaded", volSnap: "s1", policy: spec.ResumeAuto, reclaimable: true,
			records: map[string]*snapshotRecord{"s1": {RunID: "run1", Reported: true, Uploads: done}}},
		{name: "uploaded, report not acked", volSnap: "s1", policy: spec.ResumeAuto,
			records: map[string]*snapshotRecord{"s1": {RunID: "run1", Uploads: done}}},
		{name: "another Run's record", volSnap: "s2", policy: spec.ResumeAuto,
			records: map[string]*snapshotRecord{"s1": {RunID: "run1", Reported: true, Uploads: done},
				"s2": {RunID: "run2", Reported: true, Uploads: done}}},
		{name: "missing record", volSnap: "s9", policy: spec.ResumeAuto,
			records: map[string]*snapshotRecord{"s1": {RunID: "run1", Reported: true, Uploads: done}}},
		{name: "uploading", volSnap: "s1", policy: spec.ResumeAuto, awaiting: 7 * mib,
			records: map[string]*snapshotRecord{"s1": {RunID: "run1", Reported: true,
				Uploads: []pendingUpload{{BlobID: "b1", Size: 3 * mib, Done: true}, {BlobID: "b2", Size: 4 * mib}}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDiskFixture(t)
			copyRun(t, f.r, "run1", 1, c.volSnap, c.policy)
			for id, rec := range c.records {
				saveRecord(t, f.r, id, rec)
			}
			got := f.r.localCopies()
			if len(got) != 1 || got[0].runID != "run1" || got[0].reclaimable != c.reclaimable || got[0].awaiting != c.awaiting {
				t.Fatalf("copies %+v, want run1 reclaimable %v awaiting %d", got, c.reclaimable, c.awaiting)
			}
		})
	}
	t.Run("oldest exit first", func(t *testing.T) {
		f := newDiskFixture(t)
		copyRun(t, f.r, "run-a", 2, "", spec.ResumeNever)
		copyRun(t, f.r, "run-b", 1, "", spec.ResumeNever)
		var order []string
		for _, c := range f.r.localCopies() {
			order = append(order, c.runID)
		}
		if !slices.Equal(order, []string{"run-b", "run-a"}) {
			t.Fatalf("copies %v, want [run-b run-a]", order)
		}
	})
}

// discardIdle never discards a Run with a live placement here: an
// assignment that arrived after the guard listed the copy keeps it. Once
// the placement is no longer live, it discards.
func TestDiscardIdleKeepsALivePlacement(t *testing.T) {
	f := newFinishFixture(t)
	f.p.setPhase("running")
	if f.r.discardIdle(context.Background(), "run1") {
		t.Fatal("discarded a Run with a running placement")
	}
	if log := f.podmanLog(); strings.Contains(log, "volume rm") {
		t.Fatalf("podman removed a volume of a running placement:\n%s", log)
	}
	if f.r.placements["run1"] != f.p {
		t.Fatal("the running placement was dropped")
	}
	f.p.setPhase("done")
	if !f.r.discardIdle(context.Background(), "run1") || f.r.placements["run1"] != nil {
		t.Fatal("an idle Run's copy was not discarded")
	}
}

// After a runner restart, a copy whose snapshot is still uploading counts
// its blobs as pending, though the readopted placement has no pressure
// flag: no other over-limit placement is stopped for space it will free.
func TestDiskGuardCountsAnUploadingCopyAfterARestart(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.exitedAsSupervised(context.Background())
	mustEnd(t, f.p, "the placement never ended")

	// Restart: the placements are rebuilt from disk.
	f.r.mu.Lock()
	f.r.placements = map[string]*placement{}
	f.r.mu.Unlock()
	f.r.readopt(context.Background())
	if p := f.r.placements["run1"]; p == nil || p.pressureStop || p.liveState() != "" {
		t.Fatalf("readopted placement %+v, want run1 ended and not flagged", p)
	}
	st, err := readRunState(f.r.runDir("run1"))
	if err != nil {
		t.Fatal(err)
	}
	rec := f.r.snapshotRecords()[st.VolumesSnapshot]
	if rec == nil || allUploaded(rec) {
		t.Fatalf("record %+v of the volumes' snapshot %q, want one uploading", rec, st.VolumesSnapshot)
	}
	var blobs int64
	for _, up := range rec.Uploads {
		blobs += up.Size
	}
	if blobs < 2 {
		t.Fatalf("blobs of %d bytes", blobs)
	}

	// Over its limit, its stop would free 1 byte: the guard would stop it
	// unless run1's blobs count. The shortfall is half of them.
	sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(10 * mib)}}
	other := newPlacement(f.r, proto.Assign{RunID: "run2", TenantID: "t1", Epoch: 1, Spec: sp}, nil)
	other.state = &runState{RunID: "run2", Epoch: 1}
	other.phase, other.lastDisk, other.lastRw = "running", 20*mib, 1
	f.r.mu.Lock()
	f.r.placements["run2"] = other
	f.r.mu.Unlock()

	const recov = 1 << 30
	g, err := newDiskGuard(f.r.log, FreeMark{Bytes: recov - blobs/2}, FreeMark{Bytes: recov}, 2*recov)
	if err != nil {
		t.Fatal(err)
	}
	f.r.attachGuard(g, func() (int64, error) { return recov - blobs/2 - 1, nil })
	f.r.guardPass(context.Background())
	if g.state() == nil || g.state().NoRelief {
		t.Fatalf("state %+v, want pressure relieved by the uploading copy", g.state())
	}
	other.mu.Lock()
	stopped := other.pressureStop
	other.mu.Unlock()
	if stopped {
		t.Fatal("stopped run2 although run1's uploading copy covers the shortfall")
	}
}

// A pass that found nothing, racing the end of one pressure episode, does
// not carry its idle flag into the next: the first pass under the new
// pressure reclaims.
func TestDiskGuardNewPressureForgetsIdle(t *testing.T) {
	f := &fakeFS{frees: []int64{49 * mib}}
	g := f.guard(t)
	g.observe(49 * mib)
	g.setIdle(true, f.gen)
	g.observe(100 * mib)
	g.setIdle(true, f.gen)
	g.observe(49 * mib)
	g.pass(context.Background())
	if !slices.Contains(f.calls, "reclaim 51 MiB") {
		t.Fatalf("calls %v, want the first pass under new pressure to reclaim", f.calls)
	}
}

// A disk sample is cut off only against a hung podman, not at
// --usage-every: a probe that outlasts the interval still concludes, and
// the guard sees the placement over its limit.
func TestUsageLoopSampleOutlastsTheInterval(t *testing.T) {
	f := newDiskFixture(t)
	f.r.cfg.UsageEvery = 50 * time.Millisecond
	slow := strings.Replace(podmanForDiskSamples, `  cat "$0.size" ;;`, `  sleep 0.3; cat "$0.size" ;;`, 1)
	writeFile(t, f.bin, slow)
	f.size(t, diskOver)
	p := f.placement(1)
	f.r.placements["run1"] = p
	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	wg.Go(func() { f.r.usageLoop(ctx) })
	waitFor(t, "a sample longer than --usage-every never concluded", func() bool {
		vs := overLimit(f.r.diskVictims())
		return len(vs) == 1 && vs[0].used == diskOver
	})
}

// When only the writable layer's probe fails and the volumes alone are
// over the limit, the sample is conclusive and keeps the last known
// writable layer: a stop is not counted as freeing nothing.
func TestDiskVictimsKeepTheLastLayerWhenItsProbeFails(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	withVolume(p)
	f.r.placements["run1"] = p
	big := filepath.Join(f.bin+".vol", "big")
	writeFile(t, big, "")
	if err := os.Truncate(big, diskOver); err != nil {
		t.Fatal(err)
	}
	f.size(t, 5*mib)
	p.sampleSlow(f.ctx)
	f.flag(t, "failsize")
	p.sampleSlow(f.ctx)
	vs := overLimit(f.r.diskVictims())
	if len(vs) != 1 || vs[0].used != diskOver || vs[0].rw != 5*mib {
		t.Fatalf("victims %+v, want run1 at %d with its last layer %d", vs, diskOver, 5*mib)
	}
}
