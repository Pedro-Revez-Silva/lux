package runner

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// holding is a pinned container's set and the CPUs of time it was given
// (resources.cpus), spread evenly over the set as its demand.
type holding struct {
	set  []int
	cpus float64
}

// demandEpsilon absorbs float rounding in sums of fractional demands
// (0.1 + 0.9 is a full CPU).
const demandEpsilon = 1e-9

// cpusetFor picks ceil(cpus) of the host's CPUs for a Run, given what the
// host's pinned containers hold. Each CPU's demand is the sum, over the
// holdings containing it, of cpus/len(set). A fractional Run (cpus < 1)
// goes to the CPU whose demand is highest yet still fits it within one CPU
// (best fit), so fractions pack together and leave free CPUs to whole-CPU
// Runs; with no such CPU it takes the least-demand one. Any other Run takes
// the least-demand CPUs: entirely free ones first, then the least loaded.
// Ties go to the lowest CPU number. Sets are chosen at placement only and
// never moved after (no re-pinning), so a released set is only reused by
// later placements. Never fails: with no CPUs (or cpus <= 0) it returns
// nil, and the Run is not pinned.
func cpusetFor(host []int, held []holding, cpus float64) []int {
	n := int(math.Ceil(cpus))
	if n <= 0 || len(host) == 0 {
		return nil
	}
	n = min(n, len(host))
	demand := make(map[int]float64, len(host))
	for _, h := range held {
		for _, c := range h.set {
			demand[c] += h.cpus / float64(len(h.set))
		}
	}
	order := slices.Clone(host)
	slices.Sort(order)
	slices.SortStableFunc(order, func(a, b int) int { return cmpDemand(demand[a], demand[b]) })
	if cpus < 1 {
		best := -1
		for _, c := range order {
			d := demand[c]
			if d > demandEpsilon && d+cpus <= 1+demandEpsilon && (best < 0 || d > demand[best]+demandEpsilon) {
				best = c
			}
		}
		if best >= 0 {
			return []int{best}
		}
	}
	out := slices.Clone(order[:n])
	slices.Sort(out)
	return out
}

func cmpDemand(a, b float64) int {
	switch {
	case a < b-demandEpsilon:
		return -1
	case a > b+demandEpsilon:
		return 1
	}
	return 0
}

// cpuList is a set in the kernel's cpu-list form ("0-3,6"), as
// --cpuset-cpus takes it.
func cpuList(set []int) string {
	var parts []string
	for i := 0; i < len(set); {
		j := i
		for j+1 < len(set) && set[j+1] == set[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(set[i]))
		} else {
			parts = append(parts, strconv.Itoa(set[i])+"-"+strconv.Itoa(set[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// parseCPUList reads the kernel's cpu-list form; nil if malformed.
func parseCPUList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// cpuset is the set this placement's container is pinned to; "" if none.
func (p *placement) cpuset() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == nil {
		return ""
	}
	return p.state.Cpuset
}

// pinnedHolding is what p's container holds of the host's CPUs: its set
// while that container may still run, whether or not the placement is
// fenced off (stale) or replaced by a later epoch. A placement holds
// nothing once its container is known to have exited (phase exited or
// done: supervise saw podman wait return, or none ever started).
func (p *placement) pinnedHolding() (holding, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == nil || p.phase == "exited" || p.phase == "done" {
		return holding{}, false
	}
	set := parseCPUList(p.state.Cpuset)
	if set == nil {
		return holding{}, false
	}
	cpus := float64(len(set))
	if p.state.Spec != nil && p.state.Spec.Resources.CPUs > 0 {
		cpus = p.state.Spec.Resources.CPUs
	}
	return holding{set: set, cpus: cpus}, true
}

// pinnedHoldings is every pinned container's holding but p's: the
// placements in r.placements and, through prev, the earlier epochs they
// replaced whose containers have not exited yet (a kill pending or
// failed). r.mu must be held.
func (r *Runner) pinnedHoldings(p *placement) []holding {
	var held []holding
	seen := map[*placement]bool{}
	for _, o := range r.placements {
		for o != nil && !seen[o] {
			seen[o] = true
			if h, ok := o.pinnedHolding(); ok && o != p {
				held = append(held, h)
			}
			o.mu.Lock()
			next := o.prev
			o.mu.Unlock()
			o = next
		}
	}
	return held
}

// allocateCpuset pins p, about to get a container, to its share of the
// host's CPUs, given what the host's pinned containers hold (their run
// state, so a re-adopted placement counts as before the restart). Under
// r.mu, so two placements starting at once do not pick from the same view.
// The choice is saved with the container's id (createContainer).
func (r *Runner) allocateCpuset(p *placement, cpus float64) {
	if r.hostCPUs == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := cpuList(cpusetFor(r.hostCPUs, r.pinnedHoldings(p), cpus))
	p.mu.Lock()
	p.state.Cpuset = set
	p.mu.Unlock()
}

// withCpusetEnv adds LUX_CPUSET, the set in cpu-list form. A spec cannot
// set it: validation reserves the LUX_ prefix (internal/spec/spec.go).
func withCpusetEnv(env map[string]string, set string) map[string]string {
	if set == "" {
		return env
	}
	env["LUX_CPUSET"] = set
	return env
}
