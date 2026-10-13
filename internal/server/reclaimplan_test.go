package server

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func spotOn(id, tenant string, cpus float64) spotPlacement {
	return spotPlacement{ID: id, RunID: "run-" + id, TenantID: tenant, Res: spec.Resources{CPUs: cpus, Memory: spec.Bytes(cpus * 10), Disk: spec.Bytes(cpus * 10)}}
}

// planHost is a 4-cpu host in pool "pool" whose live placements are spots
// (oldest first) plus normalCPUs of normal work (tenant t1).
func planHost(id string, normalCPUs float64, spots ...spotPlacement) *candidateHost {
	h := &candidateHost{ID: id, PoolID: "pool", Connected: true, Shared: true, Capacity: proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 8}}
	add := func(cpus float64, tenant string) {
		h.UsedCPUs += cpus
		h.UsedMem += int64(cpus * 10)
		h.UsedDisk += int64(cpus * 10)
		h.UsedRuns++
		h.Tenants = append(h.Tenants, tenant)
	}
	if normalCPUs > 0 {
		add(normalCPUs, "t1")
	}
	for _, sp := range spots {
		add(sp.Res.CPUs, sp.TenantID)
		h.Spots = append(h.Spots, sp)
	}
	return h
}

func normalRun(cpus float64) pendingRun {
	r := pendingRun{ID: "n", TenantID: "t1", PoolID: new("pool")}
	r.Spec.Placement.Pool = "pool"
	r.Spec.Resources = spec.Resources{CPUs: cpus, Memory: spec.Bytes(cpus * 10), Disk: spec.Bytes(cpus * 10)}
	return r
}

func victimIDs(v []spotPlacement) []string {
	var ids []string
	for _, sp := range v {
		ids = append(ids, sp.ID)
	}
	return ids
}

// packedHost is one shared host full of n one-cpu spot placements, of one
// tenant or each of its own (distinct), and a normal Run that needs all of
// it: every placement is a victim, the pruning's worst case.
func packedHost(n int, distinct bool) (*candidateHost, pendingRun) {
	spots := make([]spotPlacement, n)
	for i := range spots {
		tenant := "t1"
		if distinct {
			tenant = fmt.Sprint("t-", i)
		}
		spots[i] = spotOn(fmt.Sprint(i), tenant, 1)
	}
	h := planHost("h", 0, spots...)
	h.Capacity = proto.Capacity{CPUs: float64(n), Memory: int64(n * 10), Disk: int64(n * 10), Runs: n}
	return h, normalRun(float64(n))
}

// Planning on a host packed with n victims, all required, costs a bounded
// amount of memory per victim: the host is simulated once, each victim
// released and restored in turn, not the whole host rebuilt per victim
// (which copied its tenant list each time: quadratic bytes, cubic time).
// The bound holds at 2,048 victims as at 128, also when every victim is
// another tenant's (each fit check then sees the host's tenancy in
// constant space, not as a list of every tenant present).
func TestReclaimPlanScalesWithVictims(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		for _, n := range []int{128, 1024, 2048} {
			h, r := packedHost(n, distinct)
			var victims []spotPlacement
			var host *candidateHost
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			host, victims = reclaimPlan(r, []*candidateHost{h})
			runtime.ReadMemStats(&after)
			if host != h || len(victims) != n {
				t.Fatalf("n=%d distinct=%v: plan host %v, %d victims; want all %d", n, distinct, host, len(victims), n)
			}
			if perVictim := (after.TotalAlloc - before.TotalAlloc) / uint64(n); perVictim > 4096 {
				t.Fatalf("n=%d distinct=%v: %d bytes allocated per victim, want a bound independent of n (<= 4096)", n, distinct, perVictim)
			}
			if !normalWants(r, h) {
				t.Fatalf("n=%d distinct=%v: the normal Run should fit once every spot is released", n, distinct)
			}
		}
	}
}

func BenchmarkReclaimPlanPacked(b *testing.B) {
	for _, distinct := range []bool{false, true} {
		for _, n := range []int{1024, 2048} {
			b.Run(fmt.Sprintf("%d/distinct=%v", n, distinct), func(b *testing.B) {
				h, r := packedHost(n, distinct)
				b.ReportAllocs()
				for b.Loop() {
					if _, v := reclaimPlan(r, []*candidateHost{h}); len(v) != n {
						b.Fatal(len(v))
					}
				}
			})
		}
	}
}

// normalWants reuses one simulation of a host without its spot placements
// across queued normal Runs, and drops it when the host's usage changes: a
// Run placed there, or a pledge made or dropped.
func TestNormalWantsFollowsHostChanges(t *testing.T) {
	h := planHost("h", 2, spotOn("a", "t1", 1))
	n := normalRun(2)
	if !normalWants(n, h) {
		t.Fatal("2 cpus fit once the 1-cpu spot is released")
	}
	other := normalRun(1)
	other.ID = "o"
	reserveHost(h, other)
	if normalWants(n, h) {
		t.Fatal("after a placement there, 2 cpus no longer fit")
	}
	h = planHost("h", 2, spotOn("a", "t1", 1))
	_ = normalWants(n, h)
	h.addPledge("p", freeVec{CPUs: 1, Runs: 1})
	if normalWants(n, h) {
		t.Fatal("after a pledge there, 2 cpus no longer fit")
	}
	h.dropPledge("p")
	if !normalWants(n, h) {
		t.Fatal("once the pledge is dropped, 2 cpus fit again")
	}
}

func TestReclaimPlan(t *testing.T) {
	for _, c := range []struct {
		name    string
		r       func() pendingRun
		hosts   func() []*candidateHost
		host    string
		victims []string
	}{
		{"youngest sufficient victim", func() pendingRun { return normalRun(1) },
			func() []*candidateHost {
				return []*candidateHost{planHost("h", 0, spotOn("old", "t1", 2), spotOn("young", "t1", 2))}
			}, "h", []string{"young"}},
		{"minimal: a large old victim instead of every small young one", func() pendingRun { return normalRun(3) },
			func() []*candidateHost {
				return []*candidateHost{planHost("h", 0, spotOn("big", "t1", 3), spotOn("s1", "t1", 0.5), spotOn("s2", "t1", 0.5))}
			}, "h", []string{"big"}},
		{"several victims when one is not enough", func() pendingRun { return normalRun(2) },
			func() []*candidateHost {
				return []*candidateHost{planHost("h", 2, spotOn("a", "t1", 1), spotOn("b", "t1", 1))}
			}, "h", []string{"b", "a"}},
		{"misfit after every victim: no eviction", func() pendingRun { return normalRun(3) },
			func() []*candidateHost {
				return []*candidateHost{planHost("h", 2, spotOn("a", "t1", 1), spotOn("b", "t1", 1))}
			}, "", nil},
		{"normal work is never a victim", func() pendingRun { return normalRun(4) },
			func() []*candidateHost { return []*candidateHost{planHost("h", 4)} }, "", nil},
		{"resources of two hosts are never combined", func() pendingRun { return normalRun(3) },
			func() []*candidateHost {
				// Each host frees 2 of the 3 cpus needed; together they would do.
				return []*candidateHost{planHost("a", 2, spotOn("x", "t1", 1)), planHost("b", 2, spotOn("y", "t1", 1))}
			}, "", nil},
		{"first host in id order whose victims suffice", func() pendingRun { return normalRun(2) },
			func() []*candidateHost {
				return []*candidateHost{planHost("b", 2, spotOn("y", "t1", 2)), planHost("a", 2, spotOn("x", "t1", 2))}
			}, "a", []string{"x"}},
		{"an already claimed victim is not claimed again", func() pendingRun { return normalRun(2) },
			func() []*candidateHost {
				h := planHost("h", 0, spotOn("a", "t1", 2), spotOn("b", "t1", 2))
				h.Spots[1].ReclaimFor = "other"
				return []*candidateHost{h}
			}, "h", []string{"a"}},
		{"required label on the victim host", func() pendingRun {
			r := normalRun(2)
			r.Spec.Placement.Requires = map[string]string{"gpu": "yes"}
			return r
		}, func() []*candidateHost {
			return []*candidateHost{planHost("plain", 2, spotOn("a", "t1", 2))}
		}, "", nil},
		{"nested support on the victim host", func() pendingRun {
			r := normalRun(2)
			r.Spec.Sandbox.NestedContainers = true
			return r
		}, func() []*candidateHost {
			plain := planHost("a", 2, spotOn("x", "t1", 2))
			nested := planHost("b", 2, spotOn("y", "t1", 2))
			nested.Labels = map[string]string{"nested": "true"}
			return []*candidateHost{plain, nested}
		}, "b", []string{"y"}},
		{"another tenant's dedicated host", func() pendingRun { return normalRun(2) },
			func() []*candidateHost {
				h := planHost("h", 0, spotOn("a", "t2", 2), spotOn("b", "t2", 2))
				h.TenantID = new("t2")
				return []*candidateHost{h}
			}, "", nil},
		{"non-shared host: victims of another tenant must all go", func() pendingRun { return normalRun(1) },
			func() []*candidateHost {
				h := planHost("h", 0, spotOn("a", "t2", 1), spotOn("b", "t2", 1))
				h.Shared = false
				return []*candidateHost{h}
			}, "h", []string{"b", "a"}},
		{"non-shared host: only the other tenant's victims go", func() pendingRun { return normalRun(1) },
			func() []*candidateHost {
				h := planHost("h", 0, spotOn("a", "t2", 1), spotOn("b", "t1", 1), spotOn("c", "t2", 1))
				h.Shared = false
				return []*candidateHost{h}
			}, "h", []string{"c", "a"}},
		{"non-shared host still used by another tenant's normal work", func() pendingRun { return normalRun(1) },
			func() []*candidateHost {
				h := planHost("h", 0, spotOn("a", "t1", 3))
				h.Shared = false
				h.UsedRuns++
				h.Tenants = append(h.Tenants, "t2")
				return []*candidateHost{h}
			}, "", nil},
		{"old runner is still a valid host for a normal Run", func() pendingRun { return normalRun(2) },
			func() []*candidateHost { return []*candidateHost{planHost("h", 2, spotOn("a", "t1", 2))} }, "h", []string{"a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			hosts := c.hosts()
			before := make([]candidateHost, len(hosts))
			for i, h := range hosts {
				before[i] = *h.clone()
			}
			h, victims := reclaimPlan(c.r(), hosts)
			got := ""
			if h != nil {
				got = h.ID
			}
			if got != c.host || !reflect.DeepEqual(victimIDs(victims), c.victims) {
				t.Fatalf("plan: host %q victims %v, want %q %v", got, victimIDs(victims), c.host, c.victims)
			}
			for i, h := range hosts {
				if !reflect.DeepEqual(*h, before[i]) {
					t.Fatalf("reclaimPlan modified host %s", h.ID)
				}
			}
		})
	}
}
