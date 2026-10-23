package server

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Resource values use CPUs, bytes (memory and disk), or placement counts
// (runs). Kind classifies it for its consumers (the wait reason, planner
// evidence), which never read Reason's text to tell kinds apart; events
// carry planBlocker, not this.
type fitBlocker struct {
	Kind      blockerKind
	Resource  string
	Requested float64
	Used      float64
	Capacity  float64
	Available float64
	Reason    string
}

type blockerKind int

const (
	// kindOther: a planner prerequisite (secrets, snapshot, chosen host).
	kindOther blockerKind = iota
	kindResource
	kindLabel // Reason names the label and both values: never exported.
	kindNested
	kindConnected
	// kindScope: the host is not the Run's (pool, removed pool, tenant,
	// sharing, another chosen host).
	kindScope
	kindSyncModes
	kindSpotReclaim
	// kindShape: a shaped pool's Run, a host not advertising the shape
	// with disk scaling.
	kindShape
	// kindDiskPressure: the host's runner reports disk pressure; it takes
	// no new placement, whatever the Run, until the pressure ends.
	kindDiskPressure
	// kindNonPreemptible: a non-preemptible Run, a spot host or a runner
	// without CapNonPreemptible (its disk guard could stop the Run).
	kindNonPreemptible
	// kindManagedImages: the Run's image needs proto.CapManagedImages.
	kindManagedImages
)

func shapeText(cpus float64, memory, disk int64) string {
	return fmt.Sprintf("cpus %g, memory %s, disk %s", cpus, spec.BytesText(memory), spec.BytesText(disk))
}

// hostFit is independent of snapshot locality and scheduler affinity. A chosen
// host bypasses pool and required labels, but not isolation or nested support.
func hostFit(r pendingRun, h *candidateHost) []fitBlocker {
	var blockers []fitBlocker
	constraint := func(kind blockerKind, reason string) {
		blockers = append(blockers, fitBlocker{Kind: kind, Reason: reason})
	}
	if !h.Connected {
		constraint(kindConnected, "host is not connected")
	}
	chosen := r.PlaceOn != ""
	if chosen && h.ID != r.PlaceOn {
		constraint(kindScope, "waiting for its chosen host "+r.PlaceOn)
	}
	if !chosen {
		if r.PoolID == nil || h.PoolID != *r.PoolID {
			constraint(kindScope, "host is not in its pool "+r.Spec.Placement.Pool)
		}
		if h.Retired {
			constraint(kindScope, "its pool was removed")
		}
	}
	if h.TenantID != nil && *h.TenantID != r.TenantID {
		constraint(kindScope, "host belongs to another tenant")
	}
	if h.TenantID == nil && !h.Shared && slices.ContainsFunc(h.Tenants, func(t string) bool { return t != r.TenantID }) {
		constraint(kindScope, "non-shared host is used by another tenant")
	}
	// After the scope checks: spot classification takes the first
	// non-resource blocker, so a host outside the Run's pool, tenancy or
	// chosen host reads as a constraint, never as its pressure.
	if h.DiskPressure != nil {
		constraint(kindDiskPressure, diskPressureReason(h.DiskPressure))
	}
	if !chosen {
		requires := r.Spec.Placement.Requires
		for _, k := range slices.Sorted(maps.Keys(requires)) {
			if v := requires[k]; h.Labels[k] != v {
				constraint(kindLabel, fmt.Sprintf("requires label %s=%s (host has %q)", k, v, h.Labels[k]))
			}
		}
	}
	if r.Spec.Sandbox.NestedContainers && h.Labels["nested"] != "true" {
		constraint(kindNested, "host does not support nested containers")
	}
	// An older runner would run a fast-forward or fetch resume as move.
	if proto.SafeSyncModes(r.PendingSync) && !slices.Contains(h.Caps, proto.CapSyncModes) {
		constraint(kindSyncModes, "host's lux-runner predates sync modes")
	}
	// An older runner would build without the context, or not publish.
	if (r.ManagedAuth || needsManagedImagesRunner(r.Spec)) && !slices.Contains(h.Caps, proto.CapManagedImages) {
		constraint(kindManagedImages, "host's lux-runner predates managed images (build contexts, publish, managed registry credential)")
	}
	if r.Spec.Spot && !slices.Contains(h.Caps, proto.CapSpotReclaim) {
		constraint(kindSpotReclaim, "host's lux-runner lacks spot reclamation support")
	}
	// A chosen host is held to it too: the guarantee is the Run's.
	if r.Spec.NonPreemptible() {
		if h.Market == MarketSpot {
			constraint(kindNonPreemptible, "host is a spot instance")
		}
		if !slices.Contains(h.Caps, proto.CapNonPreemptible) {
			constraint(kindNonPreemptible, "host's lux-runner lacks non-preemptible support")
		}
	}
	// A shaped pool's sizes are fractions of its nominal host: only a
	// runner advertising exactly that host, and scaling disk to it, takes
	// one. A larger host advertises the shape, not its hardware.
	if sh := r.Shape; sh != nil {
		if !slices.Contains(h.Caps, proto.CapDiskScale) {
			constraint(kindShape, "host's lux-runner does not scale disk (no --disk-headroom)")
		}
		if c := h.Capacity; c.CPUs != sh.CPUs || c.Memory != int64(sh.Memory) || c.Disk != int64(sh.Disk) {
			constraint(kindShape, fmt.Sprintf("host advertises %s, not its pool's shape %s",
				shapeText(c.CPUs, c.Memory, c.Disk), shapeText(sh.CPUs, int64(sh.Memory), int64(sh.Disk))))
		}
	}
	res := r.Spec.Resources
	resource := func(name string, requested, used, capacity float64, blocked bool) {
		if blocked {
			blockers = append(blockers, fitBlocker{Kind: kindResource, Resource: name, Requested: requested, Used: used, Capacity: capacity, Available: capacity - used})
		}
	}
	resource("cpus", res.CPUs, h.UsedCPUs, h.Capacity.CPUs,
		h.Capacity.CPUs > 0 && h.UsedCPUs+res.CPUs > h.Capacity.CPUs)
	// Compare byte counts as integers before converting diagnostic values.
	resource("memory", float64(res.Memory), float64(h.UsedMem), float64(h.Capacity.Memory),
		h.Capacity.Memory > 0 && h.UsedMem+int64(res.Memory) > h.Capacity.Memory)
	// A host advertising no disk reserves none. The shape gate above
	// already refuses a disk-0 host to a shaped Run; r.Shape keeps that
	// Run's disk reserved should the gate change.
	resource("disk", float64(res.Disk), float64(h.UsedDisk), float64(h.Capacity.Disk),
		(h.Capacity.Disk > 0 || r.Shape != nil) && h.UsedDisk+int64(res.Disk) > h.Capacity.Disk)
	resource("runs", 1, float64(h.UsedRuns), float64(h.Capacity.Runs),
		h.Capacity.Runs > 0 && h.UsedRuns >= h.Capacity.Runs)
	return blockers
}

func reserveHost(h *candidateHost, r pendingRun) {
	h.reserve(usageOf(r.Spec.Resources))
	h.Tenants = append(h.Tenants, r.TenantID)
}

// waitCapacity counts, per blocker kind, the hosts a waiting Run could
// otherwise use (its own pool and tenancy, or its chosen host) that lack it.
// It feeds runs.state_reason, which the tenant reads: it never names hosts
// and never carries per-host usage, so a usage change on a host that still
// lacks the same resource does not rewrite the reason.
type waitCapacity struct {
	hosts  int
	counts [len(waitKinds)]int
}

// waitKinds is the reason's fixed order: the resource kinds, then constraints.
var waitKinds = [...]string{"cpus", "memory", "disk", "runs", "labels", "nested", "syncModes", "nonPreemptible", "shape", "diskPressure", "managedImages"}

// add counts h's blockers, unless one of them rules h out for this Run on
// pool, tenancy or chosen-host grounds, or h is not connected to this luxd
// (another may hold it, so counting it would make the reason depend on
// which luxd wrote it): such a host is not the Run's to wait for here.
func (w *waitCapacity) add(blockers []fitBlocker) {
	var hit [len(waitKinds)]bool
	for _, b := range blockers {
		var kind string
		switch b.Kind {
		case kindResource:
			kind = b.Resource
		case kindLabel:
			kind = "labels"
		case kindNested:
			kind = "nested"
		case kindSyncModes:
			kind = "syncModes"
		case kindNonPreemptible:
			kind = "nonPreemptible"
		case kindShape:
			kind = "shape"
		case kindDiskPressure:
			kind = "diskPressure"
		case kindManagedImages:
			kind = "managedImages"
		default:
			return
		}
		hit[slices.Index(waitKinds[:], kind)] = true
	}
	w.hosts++
	for i, h := range hit {
		if h {
			w.counts[i]++
		}
	}
}

// maxWaitReason bounds runs.state_reason for a Run waiting on capacity.
const maxWaitReason = 512

// reason is e.g. "waiting for capacity: 3 hosts in its pool lack cpus
// (requested 4), 1 lacks memory (requested 16.0 GiB)"; "" when no host counted.
func (w *waitCapacity) reason(r pendingRun) string {
	if w.hosts == 0 {
		return ""
	}
	res := r.Spec.Resources
	var parts []string
	for i, kind := range waitKinds {
		n := w.counts[i]
		if n == 0 {
			continue
		}
		lack := plural(n, "lacks", "lack")
		var what string
		switch kind {
		case "cpus":
			what = fmt.Sprintf("%s cpus (requested %g)", lack, res.CPUs)
		case "memory":
			what = fmt.Sprintf("%s memory (requested %s)", lack, spec.BytesText(int64(res.Memory)))
		case "disk":
			what = fmt.Sprintf("%s disk (requested %s)", lack, spec.BytesText(int64(res.Disk)))
		case "runs":
			what = plural(n, "is at its Run limit", "are at their Run limit")
		case "labels":
			what = lack + " its required labels"
		case "nested":
			what = plural(n, "does", "do") + " not support nested containers"
		case "syncModes":
			what = plural(n, "runs a lux-runner", "run lux-runners") + " without sync modes (its sync asks for fast-forward or fetch)"
		case "nonPreemptible":
			what = "cannot take a non-preemptible Run (a spot instance, or a lux-runner without its support)"
		case "shape":
			sh := r.Shape
			what = plural(n, "does", "do") + " not advertise its pool's shape (" + shapeText(sh.CPUs, int64(sh.Memory), int64(sh.Disk)) + ") with disk scaling"
		case "diskPressure":
			what = plural(n, "is", "are") + " under disk pressure (low free disk)"
		case "managedImages":
			what = plural(n, "runs a lux-runner", "run lux-runners") + " without managed images (its image is published, built with a context, or in the managed registry)"
		}
		// The subject is named once: the first part says which hosts, later
		// parts only how many (nothing for the chosen host).
		var subject string
		switch {
		case r.PlaceOn != "" && len(parts) == 0:
			subject = "its chosen host "
		case r.PlaceOn != "":
		case len(parts) == 0:
			subject = fmt.Sprintf("%d %s in its pool ", n, plural(n, "host", "hosts"))
		default:
			subject = fmt.Sprintf("%d ", n)
		}
		parts = append(parts, subject+what)
	}
	return truncate("waiting for capacity: "+strings.Join(parts, ", "), maxWaitReason)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// needsManagedImagesRunner: the Run's image build uses what only a runner
// with proto.CapManagedImages does (a context, publishing), whatever luxd's
// configuration. A Run that needs the managed registry's credential (an
// image or FROM in it) is pendingRun.ManagedAuth, which only luxd's
// configuration decides.
func needsManagedImagesRunner(sp spec.RunSpec) bool {
	b := sp.Image.Build
	return b != nil && (b.Publish || len(b.ContextFiles) > 0 || len(b.Contexts) > 0)
}
