package runner

import (
	"fmt"
	"math/bits"
	"os"
)

// DefaultMemoryHeadroom is the memory left to the kernel and the host's own
// processes when --memory-headroom is not given.
const DefaultMemoryHeadroom = 512 << 20

// memoryScale turns what a Run asks for, in the terms of the memory the host
// offers (Config.Memory, the machine's gross size), into its container's
// limit: the same share of what Linux can give Runs, MemTotal - headroom.
// A host offering no more than that is not scaled, nor is anything by the
// zero value.
type memoryScale struct {
	allocatable, capacity uint64
}

func newMemoryScale(memTotal, headroom, capacity int64) (memoryScale, error) {
	if memTotal <= 0 {
		return memoryScale{}, fmt.Errorf("cannot read MemTotal from /proc/meminfo")
	}
	if headroom < 0 {
		return memoryScale{}, fmt.Errorf("--memory-headroom must not be negative")
	}
	allocatable := memTotal - headroom
	if allocatable <= 0 {
		return memoryScale{}, fmt.Errorf("--memory-headroom %d leaves no memory for Runs (MemTotal %d)", headroom, memTotal)
	}
	if capacity <= 0 || allocatable >= capacity {
		return memoryScale{allocatable: 1, capacity: 1}, nil
	}
	return memoryScale{allocatable: uint64(allocatable), capacity: uint64(capacity)}, nil
}

func (m memoryScale) factor() float64 { return float64(m.allocatable) / float64(m.capacity) }

// limit is floor(requested × factor) in exact integer arithmetic, so never
// above requested, then down to whole pages when scaled: cgroup v2 keeps
// memory.max in pages, so this is the limit the container really has. At
// least 1 byte: 0 is "no limit" to podman.
func (m memoryScale) limit(requested int64) int64 {
	if requested <= 0 || m.capacity == 0 || m.allocatable == m.capacity {
		return requested
	}
	hi, lo := bits.Mul64(uint64(requested), m.allocatable)
	q, _ := bits.Div64(hi, lo, m.capacity) // allocatable < capacity: q <= requested, no overflow
	page := uint64(os.Getpagesize())
	return int64(max(q/page*page, 1))
}
