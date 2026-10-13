// Package disklimit decides when a placement over its resources.disk is
// reported: once it has been over for its resources.diskGrace, or at once
// at CeilingFactor times the limit. The runner feeds it its disk samples.
package disklimit

import (
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// CeilingFactor: a Run using this many times its resources.disk is
// reported at once, its diskGrace notwithstanding. The host's disk is
// shared, and a write left unbounded for the whole grace could fill it for
// every other Run on it. An interim stand-in, until the runner guards the
// host's free space.
const CeilingFactor = 2

// Watch is one placement's disk-limit state. A new placement (a resume, a
// move) starts with a new Watch, and so its grace from zero.
type Watch struct {
	overSince time.Time // first sample of the current stretch over the limit; zero when under
	reported  bool
}

type Step int

const (
	Quiet     Step = iota // under, or already reported
	WentOver              // first sample over: the grace starts
	StillOver             // over, within the grace
	BackUnder             // at or under again before the grace ran out
	Exceeded              // report it; never returned twice by one Watch
)

type Verdict struct {
	Step Step
	// Limit and Grace: what the sample was held to.
	Limit int64
	Grace time.Duration
	// Remaining: the grace left, for WentOver and StillOver.
	Remaining time.Duration
	// Ceiling: Exceeded on a sample at CeilingFactor times the limit or
	// more, whether or not the grace had also run out.
	Ceiling bool
}

// Sample holds used bytes, measured at now, to res.Disk and res.DiskGrace.
// No disk limit (Disk 0) is never over.
func (w *Watch) Sample(res spec.Resources, used int64, now time.Time) Verdict {
	if res.Disk <= 0 {
		return Verdict{}
	}
	limit, grace := int64(res.Disk), res.DiskGrace.Duration.Duration
	v := w.observe(used, limit, grace, now)
	v.Limit, v.Grace = limit, grace
	return v
}

// EventData is the disk.exceeded event's data for v.
func (v Verdict) EventData(used int64) map[string]any {
	return map[string]any{"usedBytes": used, "limitBytes": v.Limit, "graceSeconds": v.Grace.Seconds(), "ceiling": v.Ceiling}
}

// observe takes one sample of used bytes against limit at now; Sample
// fills the verdict's Limit and Grace.
func (w *Watch) observe(used, limit int64, grace time.Duration, now time.Time) Verdict {
	if w.reported {
		return Verdict{}
	}
	if used <= limit {
		if w.overSince.IsZero() {
			return Verdict{}
		}
		w.overSince = time.Time{}
		return Verdict{Step: BackUnder}
	}
	first := w.overSince.IsZero()
	if first {
		w.overSince = now
	}
	// The ceiling is checked first: it is the stronger reason, and is
	// reported as such even when the grace has run out too.
	// used/CeilingFactor >= limit is used >= CeilingFactor×limit without
	// overflowing int64.
	if used/CeilingFactor >= limit {
		w.reported = true
		return Verdict{Step: Exceeded, Ceiling: true}
	}
	over := now.Sub(w.overSince)
	if over >= grace {
		w.reported = true
		return Verdict{Step: Exceeded}
	}
	step := StillOver
	if first {
		step = WentOver
	}
	return Verdict{Step: step, Remaining: grace - over}
}
