package server

import (
	"fmt"
	"slices"
	"strings"
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
)

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
	if !chosen {
		keys := make([]string, 0, len(r.Spec.Placement.Requires))
		for k := range r.Spec.Placement.Requires {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			v := r.Spec.Placement.Requires[k]
			if h.Labels[k] != v {
				constraint(kindLabel, fmt.Sprintf("requires label %s=%s (host has %q)", k, v, h.Labels[k]))
			}
		}
	}
	if r.Spec.Sandbox.NestedContainers && h.Labels["nested"] != "true" {
		constraint(kindNested, "host does not support nested containers")
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
	resource("disk", float64(res.Disk), float64(h.UsedDisk), float64(h.Capacity.Disk),
		h.Capacity.Disk > 0 && h.UsedDisk+int64(res.Disk) > h.Capacity.Disk)
	resource("runs", 1, float64(h.UsedRuns), float64(h.Capacity.Runs),
		h.Capacity.Runs > 0 && h.UsedRuns >= h.Capacity.Runs)
	return blockers
}

func reserveHost(h *candidateHost, r pendingRun) {
	h.UsedCPUs += r.Spec.Resources.CPUs
	h.UsedMem += int64(r.Spec.Resources.Memory)
	h.UsedDisk += int64(r.Spec.Resources.Disk)
	h.UsedRuns++
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
var waitKinds = [...]string{"cpus", "memory", "disk", "runs", "labels", "nested"}

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
			what = fmt.Sprintf("%s memory (requested %s)", lack, bytesText(int64(res.Memory)))
		case "disk":
			what = fmt.Sprintf("%s disk (requested %s)", lack, bytesText(int64(res.Disk)))
		case "runs":
			what = plural(n, "is at its Run limit", "are at their Run limit")
		case "labels":
			what = lack + " its required labels"
		case "nested":
			what = plural(n, "does", "do") + " not support nested containers"
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

// bytesText is a byte count in binary units, as the CLI prints it.
func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
