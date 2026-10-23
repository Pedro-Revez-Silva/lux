package runner

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// nonPreemptibleVictim is victim id of a Run with placement.preemptible false.
func nonPreemptibleVictim(id string, used, rw, limit int64) diskVictim {
	v := victim(id, used, rw, limit)
	v.exempt = true
	return v
}

// With two placements over their limit, the guard never stops the
// non-preemptible one, however larger its excess; the other goes.
func TestDiskGuardNeverStopsANonPreemptiblePlacement(t *testing.T) {
	f := &fakeFS{frees: []int64{-500 * mib}, victims: []diskVictim{
		nonPreemptibleVictim("pinned", 900, 900, 100), victim("ordinary", 150, 150, 100)}}
	f.guard(t).pass(context.Background())
	if !slices.Equal(f.stopped, []string{"ordinary"}) {
		t.Fatalf("stopped %v, want [ordinary]", f.stopped)
	}
}

// With only the non-preemptible placement over its limit, nothing is
// stopped and the pressure is reported unrelieved: admissions stay blocked.
func TestDiskGuardOnlyNonPreemptibleOverReportsNoRelief(t *testing.T) {
	f := &fakeFS{frees: []int64{1 * mib}, victims: []diskVictim{nonPreemptibleVictim("pinned", 900, 900, 100)}}
	g := f.guard(t)
	g.pass(context.Background())
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v, want none", f.stopped)
	}
	if p := g.state(); p == nil || !p.NoRelief {
		t.Fatalf("state %+v, want pressure with no relief", p)
	}
}

// diskVictims marks a placement exempt from its Run's spec as assigned:
// placement.preemptible false, and only that.
func TestDiskVictimsExemptFromTheSpec(t *testing.T) {
	f := newDiskFixture(t)
	no, yes := false, true
	for id, pre := range map[string]*bool{"pinned": &no, "explicit": &yes, "default": nil} {
		sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(diskLimit), DiskGrace: spec.SetDuration(diskGrace)},
			Placement: spec.Placement{Preemptible: pre}}
		p := newPlacement(f.r, proto.Assign{RunID: id, TenantID: "t1", Epoch: 1, Spec: sp}, nil)
		p.state = &runState{RunID: id, Epoch: 1}
		p.phase = "running"
		p.lastDisk = diskOver
		f.r.placements[id] = p
	}
	got := map[string]bool{}
	for _, v := range overLimit(f.r.diskVictims()) {
		got[v.runID] = v.exempt
	}
	want := map[string]bool{"pinned": true, "explicit": false, "default": false}
	if len(got) != len(want) {
		t.Fatalf("victims %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: exempt %v, want %v", k, got[k], w)
		}
	}
}

// The Run's own disk limit still applies: a non-preemptible placement over
// it for its whole grace is reported disk.exceeded, which luxd stops.
func TestNonPreemptibleStillReportsDiskExceeded(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	no := false
	p.assign.Spec.Placement.Preemptible = &no
	t0 := time.Now()
	p.checkDisk(f.ctx, diskOver, t0)
	p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
	f.wantEvents(t, "over its grace", 1)
}
