package runner

import (
	"testing"
	"time"
)

// The peak is the highest rate between two consecutive readings and never
// falls; a reading too soon after the last gives no rate and keeps the
// interval open; a counter that goes back starts a new interval.
func TestCPUPeakOnlyGrows(t *testing.T) {
	var c cpuPeak
	t0 := time.Unix(1_700_000_000, 0)
	steps := []struct {
		after      time.Duration
		seconds    float64
		last, peak float64 // -1: absent
	}{
		{0, 10, -1, -1},                                    // first reading
		{10 * time.Second, 30, 2, 2},                       // 20 s over 10 s
		{20 * time.Second, 35, 0.5, 2},                     // slower: the peak stays
		{20*time.Second + 200*time.Millisecond, 36, -1, 2}, // too soon: no rate
		{30 * time.Second, 75, 4, 4},                       // 40 s over the 10 s since the last rate
		{40 * time.Second, 5, -1, 4},                       // the counter went back
		{50 * time.Second, 15, 1, 4},
	}
	for i, s := range steps {
		last, peak := c.observe(s.seconds, t0.Add(s.after))
		if !same(last, s.last) || !same(peak, s.peak) {
			t.Fatalf("step %d: last %v peak %v, want %v %v", i, val(last), val(peak), s.last, s.peak)
		}
	}
}

func same(v *float64, want float64) bool {
	if want < 0 {
		return v == nil
	}
	return v != nil && *v == want
}

func val(v *float64) any {
	if v == nil {
		return "absent"
	}
	return *v
}
