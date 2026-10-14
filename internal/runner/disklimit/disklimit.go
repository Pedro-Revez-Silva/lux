// Package disklimit decides when a placement over its disk limit is
// reported: once it has been over for its resources.diskGrace, or, on a
// host that does not scale disk, at once at CeilingFactor times the limit.
// The runner feeds it its disk samples.
package disklimit

import (
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// CeilingFactor: on a host that does not scale disk, a Run using this many
// times its disk limit is reported at once, its diskGrace notwithstanding.
// The host's disk is shared, and a write left unbounded for the whole grace
// could fill it for every other Run on it. A host that scales disk
// (Limit.Scaled) has no ceiling: its runner's low-free-space guard stops
// over-limit Runs when the filesystem runs low instead.
const CeilingFactor = 2

// Limit is what a placement's disk is held to. Effective is the limit
// itself: Nominal (resources.disk) on a host that does not scale disk,
// else the host's share of it (Factor, at most 1). Scaled says which.
type Limit struct {
	Nominal, Effective int64
	Factor             float64
	Scaled             bool
	Grace              time.Duration
}

// Unscaled is res's limit on a host that does not scale disk.
func Unscaled(res spec.Resources) Limit {
	return Limit{Nominal: int64(res.Disk), Effective: int64(res.Disk), Factor: 1, Grace: res.DiskGrace.Duration.Duration}
}

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
	// Of: what the sample was held to.
	Of Limit
	// Remaining: the grace left, for WentOver and StillOver.
	Remaining time.Duration
	// Ceiling: Exceeded on a sample at CeilingFactor times the effective
	// limit or more, whether or not the grace had also run out. Never set
	// for a scaled limit.
	Ceiling bool
}

// Sample holds used bytes, measured at now, to l. A nominal request of
// zero has no limit; a positive request scaled to zero is still enforced.
func (w *Watch) Sample(l Limit, used int64, now time.Time) Verdict {
	if l.Nominal <= 0 {
		return Verdict{}
	}
	v := w.observe(used, l.Effective, l.Grace, !l.Scaled, now)
	v.Of = l
	return v
}

// Reported: this Watch has returned Exceeded (luxd is stopping the
// placement).
func (w *Watch) Reported() bool { return w.reported }

// EventData is the disk.exceeded event's data for v. limitBytes is the
// limit the Run was held to; on a host that scales disk, nominalLimitBytes
// (its resources.disk) and limitFactor say how it was derived.
func (v Verdict) EventData(used int64) map[string]any {
	d := map[string]any{"usedBytes": used, "limitBytes": v.Of.Effective, "graceSeconds": v.Of.Grace.Seconds(), "ceiling": v.Ceiling}
	if v.Of.Scaled {
		d["nominalLimitBytes"], d["limitFactor"] = v.Of.Nominal, v.Of.Factor
	}
	return d
}

// observe takes one sample of used bytes against limit at now, with or
// without the ceiling; Sample fills the verdict's limit.
func (w *Watch) observe(used, limit int64, grace time.Duration, ceiling bool, now time.Time) Verdict {
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
	if ceiling && used/CeilingFactor >= limit {
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
