package disklimit

import (
	"reflect"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

const (
	gib   = int64(1) << 30
	limit = 10 * gib
	under = limit - 1
	over  = limit + gib
	every = 15 * time.Second
)

// clock hands out sample times every 15s, the runner's default
// --usage-every, starting at an arbitrary instant.
type clock struct{ t time.Time }

func newClock() *clock { return &clock{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *clock) next() time.Time { c.t = c.t.Add(every); return c.t }

// feed observes each sample on a fresh clock and returns the steps.
func feed(w *Watch, grace time.Duration, samples ...int64) []Verdict {
	c := newClock()
	var out []Verdict
	for _, u := range samples {
		out = append(out, w.observe(u, limit, grace, c.next()))
	}
	return out
}

func exceeded(vs []Verdict) (n, at int) {
	at = -1
	for i, v := range vs {
		if v.Step == Exceeded {
			n++
			if at < 0 {
				at = i
			}
		}
	}
	return n, at
}

// Over, then back under before a 5m grace runs out: never reported; the
// first sample over says how long is left, the one back under says so.
func TestOverThenUnderWithinGraceIsNotReported(t *testing.T) {
	var w Watch
	grace := 5 * time.Minute
	// 19 samples over: 18 intervals of 15s, 4m30s, under the grace.
	samples := []int64{under}
	for range 19 {
		samples = append(samples, over)
	}
	samples = append(samples, under, under)
	vs := feed(&w, grace, samples...)
	if n, _ := exceeded(vs); n != 0 {
		t.Fatalf("reported %d times, want none: %+v", n, vs)
	}
	if vs[1].Step != WentOver || vs[1].Remaining != grace {
		t.Errorf("first sample over: %+v, want WentOver with %s left", vs[1], grace)
	}
	if vs[2].Step != StillOver || vs[2].Remaining != grace-every {
		t.Errorf("second sample over: %+v, want StillOver with %s left", vs[2], grace-every)
	}
	if vs[20].Step != BackUnder || vs[21].Step != Quiet {
		t.Errorf("back under: %+v then %+v, want BackUnder then Quiet", vs[20], vs[21])
	}
}

// Over on every sample for the whole grace: reported once, on the first
// sample at least the grace after the first one over, and with no ceiling.
func TestOverForTheWholeGraceIsReportedOnce(t *testing.T) {
	var w Watch
	grace := 5 * time.Minute
	samples := make([]int64, 40)
	for i := range samples {
		samples[i] = over
	}
	vs := feed(&w, grace, samples...)
	n, at := exceeded(vs)
	// Sample 0 starts the grace; sample 20 is 20×15s = 5m after it.
	if n != 1 || at != 20 {
		t.Fatalf("reported %d times, first at sample %d; want once, at 20", n, at)
	}
	if vs[at].Ceiling {
		t.Error("reported as the ceiling; want the grace")
	}
	if vs[19].Step != StillOver || vs[19].Remaining != every {
		t.Errorf("sample 19: %+v, want StillOver with %s left", vs[19], every)
	}
}

// Over, under, over again: the second stretch gets the whole grace again,
// not what the first one left.
func TestGraceRestartsAfterDroppingUnder(t *testing.T) {
	var w Watch
	grace := time.Minute // 4 intervals
	vs := feed(&w, grace,
		over, over, over, over, // 45s over
		under,
		over, over, over, over, // 45s over: still within a fresh grace
		over, // 60s into the second stretch
	)
	n, at := exceeded(vs)
	if n != 1 || at != 9 {
		t.Fatalf("reported %d times, first at sample %d; want once, at 9 (a minute into the second stretch): %+v", n, at, vs)
	}
	if vs[5].Step != WentOver || vs[5].Remaining != grace {
		t.Errorf("second stretch's first sample: %+v, want WentOver with the whole %s", vs[5], grace)
	}
}

// Grace 0 is the behaviour without a grace: the first sample over is
// reported, and a sample exactly at the limit is not over.
func TestZeroGraceReportsTheFirstSampleOver(t *testing.T) {
	var w Watch
	vs := feed(&w, 0, under, limit, over, over)
	n, at := exceeded(vs)
	if n != 1 || at != 2 {
		t.Fatalf("reported %d times, first at sample %d; want once, at 2: %+v", n, at, vs)
	}
	if vs[1].Step != Quiet {
		t.Errorf("at the limit exactly: %+v, want Quiet", vs[1])
	}
	if vs[2].Ceiling {
		t.Error("grace 0 reported as the ceiling")
	}
}

// At CeilingFactor times the limit a Run is reported on that sample,
// however much of its grace is left; just under it, the grace holds.
func TestCeilingReportsAtOnceDespiteTheGrace(t *testing.T) {
	grace := time.Hour
	ceiling := CeilingFactor * limit
	t.Run("first sample", func(t *testing.T) {
		var w Watch
		vs := feed(&w, grace, ceiling)
		if vs[0].Step != Exceeded || !vs[0].Ceiling {
			t.Fatalf("%+v, want Exceeded at the ceiling", vs[0])
		}
	})
	t.Run("during the grace", func(t *testing.T) {
		var w Watch
		vs := feed(&w, grace, over, ceiling-1, ceiling+gib, ceiling+gib)
		n, at := exceeded(vs)
		if n != 1 || at != 2 || !vs[2].Ceiling {
			t.Fatalf("reported %d times, first at %d: %+v; want once, at 2, as the ceiling", n, at, vs)
		}
		if vs[1].Step != StillOver {
			t.Errorf("one byte under the ceiling: %+v, want StillOver", vs[1])
		}
	})
	t.Run("no overflow near MaxInt64", func(t *testing.T) {
		var w Watch
		const big = int64(1) << 62
		if v := w.observe(big+1, big, grace, newClock().next()); v.Step != WentOver {
			t.Fatalf("%+v, want WentOver: 2×limit overflows int64 and must not trip the ceiling", v)
		}
	})
}

// A sample at the ceiling is reported as the ceiling even when the grace
// has also run out, or is 0; one between the limit and the ceiling after
// the grace has run out is reported as the grace.
func TestCeilingWinsOverAnElapsedGrace(t *testing.T) {
	ceiling := CeilingFactor * limit
	for _, c := range []struct {
		name    string
		grace   time.Duration
		samples []int64
		ceiling bool
	}{
		{"grace 0, the first sample at the ceiling", 0, []int64{ceiling}, true},
		{"the grace runs out on a sample at the ceiling", time.Minute, []int64{over, over, over, over, ceiling}, true},
		{"the grace runs out on a sample between 1× and 2×", time.Minute, []int64{over, over, over, over, ceiling - 1}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var w Watch
			vs := feed(&w, c.grace, c.samples...)
			last := len(vs) - 1
			if n, at := exceeded(vs); n != 1 || at != last {
				t.Fatalf("reported %d times, first at %d: %+v; want once, at %d", n, at, vs, last)
			}
			if vs[last].Ceiling != c.ceiling {
				t.Errorf("ceiling %t, want %t", vs[last].Ceiling, c.ceiling)
			}
		})
	}
}

// One Watch reports at most once, whatever follows: more samples over,
// a drop under and a new stretch over, the ceiling.
func TestOneReportPerPlacement(t *testing.T) {
	var w Watch
	vs := feed(&w, 0, over, over, under, over, 3*limit, over)
	if n, _ := exceeded(vs); n != 1 {
		t.Fatalf("reported %d times, want once: %+v", n, vs)
	}
	for i, v := range vs[1:] {
		if v.Step != Quiet {
			t.Errorf("sample %d after the report: %+v, want Quiet", i+1, v)
		}
	}
}

// Unscaled reads the limit and grace from a Run's resources (a spec as luxd
// assigns it) and the event carries them: graceSeconds and ceiling, and no
// nominal limit or factor.
func TestSampleUsesTheSpecAndFillsTheEvent(t *testing.T) {
	res := Unscaled(spec.Resources{Disk: spec.Bytes(limit), DiskGrace: spec.SetDuration(5 * time.Minute)})
	c := newClock()
	var w Watch
	if v := w.Sample(res, over, c.next()); v.Step != WentOver || v.Remaining != 5*time.Minute {
		t.Fatalf("%+v, want WentOver with 5m left", v)
	}
	c.t = c.t.Add(5*time.Minute - every)
	v := w.Sample(res, over, c.next())
	if v.Step != Exceeded {
		t.Fatalf("%+v, want Exceeded once 5m over", v)
	}
	d := v.EventData(over)
	want := map[string]any{"usedBytes": over, "limitBytes": limit, "graceSeconds": 300.0, "ceiling": false}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("event data %v, want %v", d, want)
	}

	var cw Watch
	cv := cw.Sample(res, 2*limit, newClock().next())
	if d := cv.EventData(2 * limit); cv.Step != Exceeded || d["ceiling"] != true || d["graceSeconds"] != 300.0 {
		t.Errorf("at the ceiling: %+v %v", cv, d)
	}

	// No disk limit is never over.
	var nw Watch
	if v := nw.Sample(Unscaled(spec.Resources{}), 1<<50, newClock().next()); v.Step != Quiet {
		t.Errorf("no limit: %+v, want Quiet", v)
	}
}

// A scaled limit: the grace and the ceiling both count from the effective
// limit, never the nominal one, and the event says both and the factor.
func TestScaledLimitHoldsToTheEffective(t *testing.T) {
	// 160 GiB nominal on a 160 GiB filesystem with 10 GiB headroom: 40 GiB
	// asked holds to 37.5 GiB.
	const nominal, effective = 40 * gib, 75 * gib / 2
	l := Limit{Nominal: nominal, Effective: effective, Factor: 0.9375, Scaled: true, Grace: time.Minute}
	t.Run("positive nominal scaled to zero", func(t *testing.T) {
		var w Watch
		zero := Limit{Nominal: 1, Effective: 0, Factor: 0.5, Scaled: true, Grace: time.Hour}
		if v := w.Sample(zero, 0, newClock().next()); v.Step != Quiet {
			t.Fatalf("empty: %+v, want Quiet", v)
		}
		if v := w.Sample(zero, 1, newClock().next()); v.Step != Exceeded || !v.Ceiling {
			t.Fatalf("one byte: %+v, want Exceeded at the zero-byte ceiling", v)
		}
	})
	t.Run("between effective and nominal goes over", func(t *testing.T) {
		var w Watch
		if v := w.Sample(l, effective+1, newClock().next()); v.Step != WentOver {
			t.Fatalf("%+v, want WentOver one byte over the effective limit", v)
		}
	})
	t.Run("at the effective limit is not over", func(t *testing.T) {
		var w Watch
		if v := w.Sample(l, effective, newClock().next()); v.Step != Quiet {
			t.Fatalf("%+v, want Quiet at the effective limit", v)
		}
	})
	t.Run("the grace runs from the effective limit", func(t *testing.T) {
		var w Watch
		c := newClock()
		var vs []Verdict
		for range 5 {
			vs = append(vs, w.Sample(l, nominal-1, c.next()))
		}
		if n, at := exceeded(vs); n != 1 || at != 4 || vs[4].Ceiling {
			t.Fatalf("reported %d times, first at %d: %+v; want once, at 4 (a minute), not the ceiling", n, at, vs)
		}
		want := map[string]any{"usedBytes": nominal - 1, "limitBytes": effective, "graceSeconds": 60.0, "ceiling": false,
			"nominalLimitBytes": nominal, "limitFactor": 0.9375}
		if d := vs[4].EventData(nominal - 1); !reflect.DeepEqual(d, want) {
			t.Errorf("event data %v, want %v", d, want)
		}
	})
	t.Run("the ceiling is twice the effective limit", func(t *testing.T) {
		var w Watch
		if v := w.Sample(l, CeilingFactor*effective-1, newClock().next()); v.Step != WentOver {
			t.Fatalf("one byte under 2× effective: %+v, want WentOver", v)
		}
		var cw Watch
		v := cw.Sample(l, CeilingFactor*effective, newClock().next())
		if v.Step != Exceeded || !v.Ceiling {
			t.Fatalf("at 2× effective (under 2× nominal): %+v, want Exceeded at the ceiling", v)
		}
	})
}
