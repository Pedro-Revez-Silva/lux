package server

import (
	"cmp"
	"math"
	"net/http"
)

// A pool's placement policy (pools.placement): how pickHost orders the
// hosts a Run fits.
const (
	PlacementSpread = "spread"
	PlacementPack   = "pack"
)

// ValidPlacement refuses a placement other than spread, pack or "" (the
// pool's default) with 422 invalid_pool.
func ValidPlacement(p string) error {
	switch p {
	case "", PlacementSpread, PlacementPack:
		return nil
	}
	return errf(http.StatusUnprocessableEntity, "invalid_pool", "placement %q: want spread or pack", p)
}

// packs reports whether a pool with stored placement p ("" for none)
// packs: pack when set so, else when the pool is shaped.
func packs(p string, shaped bool) bool {
	return p == PlacementPack || p == "" && shaped
}

// share is the share of h's tightest resource that cpus, mem and disk
// take: the largest of used/capacity over the resources h advertises (a
// resource it does not limit counts 0). Run counts are not resources.
func (h *candidateHost) share(cpus float64, mem, disk int64) float64 {
	of := func(used, capacity float64) float64 {
		if capacity <= 0 {
			return 0
		}
		return used / capacity
	}
	return max(of(cpus, h.Capacity.CPUs),
		of(float64(mem), float64(h.Capacity.Memory)),
		of(float64(disk), float64(h.Capacity.Disk)))
}

// allocated is the share of h reserved by normal work. Unclaimed spot
// placements count as free: a host holding only spot work is idle to
// scale-down, so pack must not rank it full and keep it alive with normal
// Runs. pickHost and the planner's reserve (whose readyHosts drops spot
// usage) thus rank by the same allocation, though they need not pick the
// same host: the planner's fit set treats spot space as free, pickHost's
// (hostFit) does not. A claimed spot placement (ReclaimFor set) counts as
// allocated: its space is promised to a normal Run.
func (h *candidateHost) allocated() float64 {
	cpus, mem, disk := h.UsedCPUs, h.UsedMem, h.UsedDisk
	for _, sp := range h.Spots {
		if sp.ReclaimFor != "" {
			continue
		}
		cpus -= sp.Res.CPUs
		mem -= int64(sp.Res.Memory)
		disk -= int64(sp.Res.Disk)
	}
	return h.share(cpus, mem, disk)
}

// occupied is the share of h reserved by any work, spot included.
func (h *candidateHost) occupied() float64 {
	return h.share(h.UsedCPUs, h.UsedMem, h.UsedDisk)
}

// packCandidate is a host a Run fits, as pack orders it.
type packCandidate struct {
	h     *candidateHost
	avoid bool    // the Run must avoid it: last, whatever its allocation
	used  float64 // allocated()
	// occupied() breaks ties on used: sharing a host with spot work keeps
	// an empty host free for a whole-host Run without a reclaim.
	occupied float64
	affinity int // snapshot copy, cached image and mirrors, preferred labels
}

// packCandidateOf is h as pack orders it for a Run avoiding avoidHost
// ("": none, so a planned host without an id is never avoided).
func packCandidateOf(h *candidateHost, avoidHost string, affinity int) packCandidate {
	return packCandidate{h: h, avoid: avoidHost != "" && h.ID == avoidHost, used: h.allocated(), occupied: h.occupied(), affinity: affinity}
}

// packOrder puts the host to choose first: hosts the Run need not avoid,
// then the most allocated by normal work, then the most occupied, then the
// highest affinity, then the lowest id.
// Shares compare as math.Round(share*1e9), one key per host so the order
// stays total; float sums such as 0.1+0.2 and 0.3 tie and the next key
// decides.
func packOrder(a, b packCandidate) int {
	if a.avoid != b.avoid {
		if a.avoid {
			return 1
		}
		return -1
	}
	if c := cmp.Compare(math.Round(b.used*1e9), math.Round(a.used*1e9)); c != 0 {
		return c
	}
	if c := cmp.Compare(math.Round(b.occupied*1e9), math.Round(a.occupied*1e9)); c != 0 {
		return c
	}
	if c := cmp.Compare(b.affinity, a.affinity); c != 0 {
		return c
	}
	return cmp.Compare(a.h.ID, b.h.ID)
}
