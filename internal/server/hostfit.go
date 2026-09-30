package server

import (
	"fmt"
	"slices"
	"strings"
)

// Resource values use CPUs, bytes (memory and disk), or placement counts (runs).
type fitBlocker struct {
	Resource  string  `json:"resource,omitempty"`
	Requested float64 `json:"requested,omitempty"`
	Used      float64 `json:"used,omitempty"`
	Capacity  float64 `json:"capacity,omitempty"`
	Available float64 `json:"available,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// hostFit is independent of snapshot locality and scheduler affinity. A chosen
// host bypasses pool and required labels, but not isolation or nested support.
func hostFit(r pendingRun, h *candidateHost) []fitBlocker {
	var blockers []fitBlocker
	constraint := func(reason string) { blockers = append(blockers, fitBlocker{Reason: reason}) }
	if !h.Connected {
		constraint("host is not connected")
	}
	chosen := r.PlaceOn != ""
	if chosen && h.ID != r.PlaceOn {
		constraint("waiting for its chosen host " + r.PlaceOn)
	}
	if !chosen {
		if r.PoolID == nil || h.PoolID != *r.PoolID {
			constraint("host is not in its pool " + r.Spec.Placement.Pool)
		}
		if h.Retired {
			constraint("its pool was removed")
		}
	}
	if h.TenantID != nil && *h.TenantID != r.TenantID {
		constraint("host belongs to another tenant")
	}
	if h.TenantID == nil && !h.Shared && slices.ContainsFunc(h.Tenants, func(t string) bool { return t != r.TenantID }) {
		constraint("non-shared host is used by another tenant")
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
				constraint(fmt.Sprintf("requires label %s=%s (host has %q)", k, v, h.Labels[k]))
			}
		}
	}
	if r.Spec.Sandbox.NestedContainers && h.Labels["nested"] != "true" {
		constraint("host does not support nested containers")
	}
	res := r.Spec.Resources
	resource := func(name string, requested, used, capacity float64, blocked bool) {
		if blocked {
			blockers = append(blockers, fitBlocker{Resource: name, Requested: requested, Used: used, Capacity: capacity, Available: capacity - used})
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

func hostFitReason(h *candidateHost, blockers []fitBlocker) string {
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b.Resource == "" {
			parts = append(parts, b.Reason)
			continue
		}
		unit := ""
		if b.Resource == "memory" || b.Resource == "disk" {
			unit = " bytes"
		}
		parts = append(parts, fmt.Sprintf("%s requested %g%s, used %g%s, capacity %g%s, available %g%s",
			b.Resource, b.Requested, unit, b.Used, unit, b.Capacity, unit, b.Available, unit))
	}
	return fmt.Sprintf("waiting for host %s: %s", h.ID, strings.Join(parts, "; "))
}
