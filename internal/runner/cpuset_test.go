package runner

import (
	"context"
	"slices"
	"testing"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

func cpus(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// place runs cpusetFor for each request in turn, each set held by the
// next, as a host placing them one after another does.
func place(host []int, reqs ...float64) [][]int {
	var held []holding
	var sets [][]int
	for _, c := range reqs {
		s := cpusetFor(host, held, c)
		held = append(held, holding{s, c})
		sets = append(sets, s)
	}
	return sets
}

// demandOf sums each CPU's demand over sets placed for reqs.
func demandOf(sets [][]int, reqs []float64) map[int]float64 {
	d := map[int]float64{}
	for i, s := range sets {
		for _, c := range s {
			d[c] += reqs[i] / float64(len(s))
		}
	}
	return d
}

// The allocator's placements, request by request, with exact sets.
func TestCpusetPlacesByDemand(t *testing.T) {
	for _, c := range []struct {
		name string
		host []int
		reqs []float64
		want [][]int
	}{
		{"whole-CPU Runs are disjoint while CPUs are free", cpus(8), []float64{2, 2, 3}, [][]int{{0, 1}, {2, 3}, {4, 5, 6}}},
		// 0.9 joins 0.1 on CPU 1 (1.0); CPU 0 keeps the whole Run alone.
		{"1, 0.1, 0.9 on 2 CPUs: no CPU above 1", cpus(2), []float64{1, 0.1, 0.9}, [][]int{{0}, {1}, {1}}},
		// The fractions pack two to a CPU; the 2-CPU Run gets the two
		// CPUs with no fractional demand.
		{"4×0.5 then 2 on 4 CPUs", cpus(4), []float64{0.5, 0.5, 0.5, 0.5, 2}, [][]int{{0}, {0}, {1}, {1}, {2, 3}}},
		{"a fraction that fits nowhere takes a free CPU", cpus(3), []float64{0.6, 0.6, 1}, [][]int{{0}, {1}, {2}}},
		// Best fit: 0.3 fits beside 0.6 (0.9) and beside 0.5 (0.8); it
		// takes the fuller CPU.
		{"best fit packs the fullest CPU that still fits", cpus(4), []float64{0.6, 0.5, 0.3}, [][]int{{0}, {1}, {0}}},
		{"1.5 takes free CPUs first", cpus(4), []float64{0.5, 1.5}, [][]int{{0}, {1, 2}}},
		{"a non-contiguous affinity mask", []int{2, 3, 6, 7}, []float64{2, 1, 0.5}, [][]int{{2, 3}, {6}, {7}}},
		// Oversubscribed: the 2-CPU Run takes the least-demand CPUs, never fails.
		{"full host: least demand", cpus(4), []float64{2, 2, 0.5, 2}, [][]int{{0, 1}, {2, 3}, {0}, {1, 2}}},
		{"more CPUs than the host has", cpus(4), []float64{16}, [][]int{{0, 1, 2, 3}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := place(c.host, c.reqs...)
			if !slices.EqualFunc(got, c.want, slices.Equal) {
				t.Fatalf("sets %v, want %v", got, c.want)
			}
		})
	}
	// As demand: no CPU above one CPU of time.
	for _, reqs := range [][]float64{{1, 0.1, 0.9}, {0.5, 0.5, 0.5, 0.5, 2}} {
		for c, d := range demandOf(place(cpus(len(reqs)), reqs...), reqs) {
			if d > 1+demandEpsilon {
				t.Errorf("%v: CPU %d has demand %g", reqs, c, d)
			}
		}
	}
	if got := cpusetFor(nil, nil, 2); got != nil {
		t.Fatalf("no CPUs: %v", got)
	}
	if got := cpusetFor(cpus(4), nil, 0); got != nil {
		t.Fatalf("no cpus asked: %v", got)
	}
}

// Held CPUs outside the runner's current mask (a restart under a narrower
// affinity) count for nothing: only the mask is placed from.
func TestCpusetIgnoresHeldCPUsOutsideTheMask(t *testing.T) {
	if got := cpusetFor([]int{2, 3, 6, 7}, []holding{{[]int{0, 1}, 2}, {[]int{2}, 1}}, 2); !slices.Equal(got, []int{3, 6}) {
		t.Fatalf("%v, want [3 6]", got)
	}
}

// Sets are never moved once chosen: three 2-CPU Runs on 4 CPUs get 0-1,
// 2-3, 0-1; when the middle one ends, the other two still share 0-1 and
// 2-3 sits idle until the next placement takes it (documented in
// docs/runspec.md).
func TestCpusetReleaseIsReusedByTheNextPlacementOnly(t *testing.T) {
	host := cpus(4)
	sets := place(host, 2, 2, 2)
	if want := [][]int{{0, 1}, {2, 3}, {0, 1}}; !slices.EqualFunc(sets, want, slices.Equal) {
		t.Fatalf("sets %v, want %v", sets, want)
	}
	left := []holding{{sets[0], 2}, {sets[2], 2}}
	if got := cpusetFor(host, left, 2); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("after releasing 2-3: %v, want [2 3]", got)
	}
}

// What re-adoption does: the sets read back from run state, in cpu-list
// form, give the next Run what it would have got before the restart.
func TestCpusetRebuiltFromStateMatches(t *testing.T) {
	host := cpus(6)
	reqs := []float64{2, 1, 0.5}
	sets := place(host, reqs...)
	var before, after []holding
	for i, s := range sets {
		before = append(before, holding{s, reqs[i]})
		after = append(after, holding{parseCPUList(cpuList(s)), reqs[i]})
	}
	if a, b := cpusetFor(host, before, 2), cpusetFor(host, after, 2); !slices.Equal(a, b) || !slices.Equal(a, []int{4, 5}) {
		t.Fatalf("next Run: %v before restart, %v after", a, b)
	}
}

func TestCPUList(t *testing.T) {
	for s, want := range map[string][]int{"0-3,6": {0, 1, 2, 3, 6}, "5": {5}, "1,3,5": {1, 3, 5}, "0-1,1-2": {0, 1, 2}} {
		got := parseCPUList(s)
		if !slices.Equal(got, want) {
			t.Errorf("parse %q: %v", s, got)
		}
		if s != "0-1,1-2" && cpuList(got) != s {
			t.Errorf("format %v: %q", got, cpuList(got))
		}
	}
	for _, bad := range []string{"", "a", "3-1", "-1", "1,"} {
		if got := parseCPUList(bad); got != nil {
			t.Errorf("parse %q: %v", bad, got)
		}
	}
}

// With --cpuset, each new placement is pinned beside the host's live ones,
// those re-adopted from run state included, and its container is created
// with --cpuset-cpus and LUX_CPUSET; without it nothing is pinned.
func TestPlacementsArePinnedBesideLiveOnes(t *testing.T) {
	r := &Runner{hostCPUs: cpus(4), placements: map[string]*placement{}}
	// As readopt leaves a running one: its state as saved.
	r.placements["old"] = &placement{r: r, runID: "old", phase: "running", state: &runState{Cpuset: "0-1"}}
	r.placements["gone"] = &placement{r: r, runID: "gone", phase: "done", state: &runState{Cpuset: "2-3"}}
	p := &placement{r: r, runID: "new", phase: "starting", state: &runState{}}
	r.placements["new"] = p
	r.allocateCpuset(p, 2)
	if got := p.cpuset(); got != "2-3" {
		t.Fatalf("pinned to %q, want 2-3 (0-1 is held; a done placement holds nothing)", got)
	}
	sp := spec.RunSpec{Resources: spec.Resources{CPUs: 2, Memory: 1 << 30, Pids: 64}}
	args := p.createArgs(sp, "img", podman.Network{})
	if i := slices.Index(args, "--cpuset-cpus"); i < 0 || args[i+1] != "2-3" {
		t.Fatalf("create args %v", args)
	}
	if env := withCpusetEnv(map[string]string{}, p.cpuset()); env["LUX_CPUSET"] != "2-3" {
		t.Fatalf("env %v", env)
	}

	off := &Runner{placements: map[string]*placement{}}
	q := &placement{r: off, runID: "q", state: &runState{}}
	off.allocateCpuset(q, 2)
	if args := q.createArgs(sp, "img", podman.Network{}); slices.Contains(args, "--cpuset-cpus") || q.cpuset() != "" {
		t.Fatalf("without --cpuset: %v", args)
	}
	if env := withCpusetEnv(map[string]string{}, ""); len(env) != 0 {
		t.Fatalf("without --cpuset: env %v", env)
	}
}

// Which placements hold their CPUs: any whose container may still run
// (starting with a set, running, stopping, fenced off as stale), and none
// whose container is known to have exited (exited, done).
func TestCpusetHeldUntilTheContainerExits(t *testing.T) {
	for _, c := range []struct {
		phase string
		stale bool
		want  string // the next 2-CPU Run's set on 4 CPUs, beside 0-1
	}{
		{"starting", false, "2-3"},
		{"running", false, "2-3"},
		{"stopping", false, "2-3"},
		{"running", true, "2-3"},
		{"exited", false, "0-1"},
		{"exited", true, "0-1"},
		{"done", false, "0-1"},
	} {
		r := &Runner{hostCPUs: cpus(4), placements: map[string]*placement{}}
		r.placements["a"] = &placement{r: r, runID: "a", phase: c.phase, stale: c.stale, state: &runState{Cpuset: "0-1"}}
		p := &placement{r: r, runID: "b", phase: "starting", state: &runState{}}
		r.placements["b"] = p
		r.allocateCpuset(p, 2)
		if got := p.cpuset(); got != c.want {
			t.Errorf("beside a %s placement (stale %v) on 0-1: %q, want %q", c.phase, c.stale, got, c.want)
		}
	}
}

// The handover: epoch 2 of run1 replaces epoch 1, which is fenced off and
// killed, but whose container has not exited (the kill pending or failed).
// Its CPUs stay held through the new placement's prev until that container
// exits; only then does another Run get them.
func TestCpusetOfAReplacedEpochHeldUntilItExits(t *testing.T) {
	f := newFinishFixture(t)
	f.r.hostCPUs = cpus(4)
	f.p.mu.Lock()
	f.p.phase = "running"
	f.p.state.Cpuset = "0-1"
	f.p.mu.Unlock()
	p2 := f.assignEpoch(t, 2)
	if !f.p.isStale() || p2.prev != f.p {
		t.Fatalf("epoch 1 stale %v, epoch 2's prev %p, want it fenced and linked", f.p.isStale(), p2.prev)
	}
	other := func() string {
		q := &placement{r: f.r, runID: "run2", phase: "starting", state: &runState{}}
		f.r.allocateCpuset(q, 2)
		return q.cpuset()
	}
	if got := other(); got != "2-3" {
		t.Fatalf("while epoch 1's container may still run: %q, want 2-3", got)
	}
	// As supervise does once podman wait returns.
	f.release()
	f.exitedAsSupervised(context.Background())
	if got := other(); got != "0-1" {
		t.Fatalf("after epoch 1's container exited: %q, want 0-1", got)
	}
	// Both epochs write their run state in the background: let them end
	// before the fixture's temp dir is removed, as the epoch-overlap tests do.
	f.releaseRemove()
	mustEnd(t, f.p, "epoch 1 never ended")
	mustEnd(t, p2, "epoch 2 never ended")
}

// Re-adoption through the run state file: a started placement's set and
// its spec's CPUs are saved as JSON, read back by readopt after a restart,
// and held as that demand, so the next 0.5-CPU placement packs beside it
// on CPU 2 (held as a whole CPU, or not held at all, it would take CPU 0).
func TestCpusetReadoptedFromRunState(t *testing.T) {
	g := newReclaimRig(t, false)
	p := g.running(t)
	p.state.Cpuset = "2"
	p.state.Spec.Resources.CPUs = 0.5
	p.state.Egress = &egressState{Unrestricted: true, Interface: "lux0", Gateway: "10.88.0.1"}
	if err := p.saveState(); err != nil {
		t.Fatal(err)
	}
	g.r.placements = map[string]*placement{}
	g.r.hostCPUs = cpus(4)
	g.r.readopt(g.ctx)
	q := g.r.placements["run1"]
	if q == nil || q == p {
		t.Fatal("run1 was not re-adopted from its run state")
	}
	next := &placement{r: g.r, runID: "run2", phase: "starting", state: &runState{}}
	g.r.allocateCpuset(next, 0.5)
	got := next.cpuset()
	q.kill(g.ctx)
	mustEnd(t, q, "the re-adopted placement never ended")
	if got != "2" {
		t.Fatalf("beside the re-adopted 0.5 on CPU 2: %q, want 2 (packed)", got)
	}
}

// A live fractional Run holds its spec's demand, not its whole set: the
// next fraction packs beside it.
func TestCpusetPacksBesideALiveFractionalRun(t *testing.T) {
	r := &Runner{hostCPUs: cpus(2), placements: map[string]*placement{}}
	half := &spec.RunSpec{Resources: spec.Resources{CPUs: 0.5}}
	r.placements["a"] = &placement{r: r, runID: "a", phase: "running", state: &runState{Cpuset: "0", Spec: half}}
	p := &placement{r: r, runID: "b", phase: "starting", state: &runState{}}
	r.placements["b"] = p
	r.allocateCpuset(p, 0.5)
	if got := p.cpuset(); got != "0" {
		t.Fatalf("a 0.5 Run beside a live 0.5 on CPU 0: %q, want 0 (packed)", got)
	}
}
