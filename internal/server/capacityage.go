package server

import (
	"encoding/json"
	"time"
)

// spotSettle is how long capacity must have been continuously free before a
// spot Run may use it: strictly more than this.
const spotSettle = 60 * time.Second

// maxAgeHistory bounds a host's capacity-age history. Overflow merges the two
// oldest entries into their component-wise minimum, which only understates
// what was free.
const maxAgeHistory = 32

// freeVec is a host's unreserved capacity: cpus, memory and disk bytes, and
// run slots. A component of an unlimited dimension (capacity 0) is
// unlimitedFree.
type freeVec struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk"`
	Runs   int     `json:"runs"`
}

const unlimitedFree = 1 << 60

func (a freeVec) min(b freeVec) freeVec {
	return freeVec{min(a.CPUs, b.CPUs), min(a.Memory, b.Memory), min(a.Disk, b.Disk), min(a.Runs, b.Runs)}
}

// covers reports whether a has room for a Run reserving cpus, memory, disk
// and one run slot.
func (a freeVec) covers(cpus float64, memory, disk int64) bool {
	return a.CPUs >= cpus && a.Memory >= memory && a.Disk >= disk && a.Runs >= 1
}

type ageEntry struct {
	At   time.Time `json:"at"`
	Free freeVec   `json:"free"`
}

// capacityAge is a host's free vector over time: each entry holds from its At
// until the next entry's. Key identifies what the history was built under
// (runner connection epoch, capacity and pool); a different key discards it.
type capacityAge struct {
	Key     string     `json:"-"`
	History []ageEntry `json:"history"`
	dirty   bool
}

// observe records free as the host's free vector from now on. A changed key
// starts a fresh history at now: nothing before it is known.
func (c *capacityAge) observe(key string, now time.Time, free freeVec) {
	if c.Key != key {
		c.Key, c.History, c.dirty = key, []ageEntry{{now, free}}, true
		return
	}
	if n := len(c.History); n > 0 && c.History[n-1].Free == free {
		return
	}
	if n := len(c.History); n > 0 && now.Before(c.History[n-1].At) {
		now = c.History[n-1].At
	}
	c.History = append(c.History, ageEntry{now, free})
	c.dirty = true
	c.compact(now)
}

// compact drops entries that ended before the settle window and merges the
// oldest entries while the history is over its bound.
func (c *capacityAge) compact(now time.Time) {
	cutoff := now.Add(-spotSettle)
	k := 0
	for i := 1; i < len(c.History); i++ {
		if c.History[i].At.Before(cutoff) {
			k = i
		}
	}
	c.History = c.History[k:]
	for len(c.History) > maxAgeHistory {
		c.History[1] = ageEntry{c.History[0].At, c.History[0].Free.min(c.History[1].Free)}
		c.History = c.History[1:]
	}
}

// aged is the vector continuously free for more than spotSettle up to now:
// the minimum over every entry in force since now-spotSettle. With no entry
// older than that, nothing is known to be aged.
func (c *capacityAge) aged(now time.Time) freeVec {
	cutoff := now.Add(-spotSettle)
	k := -1
	for i, e := range c.History {
		if e.At.Before(cutoff) {
			k = i
		}
	}
	if k < 0 {
		return freeVec{}
	}
	v := c.History[k].Free
	for _, e := range c.History[k+1:] {
		v = v.min(e.Free)
	}
	return v
}

// settlesIn is how long until a request the current free vector covers
// would be admissible if nothing changed (aging needs strictly more than
// spotSettle, so it rounds up by a second); ok is false when the current
// vector does not cover it at all.
func (c *capacityAge) settlesIn(now time.Time, cpus float64, memory, disk int64) (time.Duration, bool) {
	if c.aged(now).covers(cpus, memory, disk) {
		return 0, true
	}
	var since time.Time
	for i := len(c.History) - 1; i >= 0 && c.History[i].Free.covers(cpus, memory, disk); i-- {
		since = c.History[i].At
	}
	if since.IsZero() {
		return 0, false
	}
	return max(0, since.Add(spotSettle).Sub(now)).Truncate(time.Second) + time.Second, true
}

func (c *capacityAge) marshal() []byte {
	b, _ := json.Marshal(c.History)
	return b
}

// freeOf is h's free vector from its capacity and current reservations.
func freeOf(h *candidateHost) freeVec {
	v := freeVec{unlimitedFree, unlimitedFree, unlimitedFree, unlimitedFree}
	if h.Capacity.CPUs > 0 {
		v.CPUs = h.Capacity.CPUs - h.UsedCPUs
	}
	if h.Capacity.Memory > 0 {
		v.Memory = h.Capacity.Memory - h.UsedMem
	}
	if h.Capacity.Disk > 0 {
		v.Disk = h.Capacity.Disk - h.UsedDisk
	}
	if h.Capacity.Runs > 0 {
		v.Runs = h.Capacity.Runs - h.UsedRuns
	}
	return v
}
