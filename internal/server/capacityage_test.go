package server

import (
	"testing"
	"time"
)

func TestCapacityAgeBoundariesAndHoles(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var c capacityAge
	small := freeVec{1, 10, 10, 1}
	c.observe("boot", at, small)
	for _, sec := range []int{59, 60, 61} {
		got := c.aged(at.Add(time.Duration(sec)*time.Second)).covers(1, 10, 10)
		if got != (sec > 60) {
			t.Fatalf("at %ds: aged=%v", sec, got)
		}
	}
	c.observe("boot", at.Add(70*time.Second), freeVec{8, 80, 80, 8})
	if c.aged(at.Add(71*time.Second)).covers(2, 20, 20) {
		t.Fatal("fresh large gap aged")
	}
	if !c.aged(at.Add(71*time.Second)).covers(1, 10, 10) {
		t.Fatal("old small hole lost")
	}
	c.observe("boot", at.Add(72*time.Second), freeVec{7, 70, 70, 7})
	if !c.aged(at.Add(73*time.Second)).covers(1, 10, 10) {
		t.Fatal("unrelated allocation reset old hole")
	}
	for _, key := range []string{"reconnect", "capacity-change", "shape-change"} {
		c.observe(key, at.Add(140*time.Second), small)
		if c.aged(at.Add(141*time.Second)).covers(1, 10, 10) {
			t.Fatal("generation inherited age")
		}
	}
}

func TestCapacityAgeEachDimensionAndBound(t *testing.T) {
	at := time.Now()
	for _, low := range []freeVec{{0, 10, 10, 1}, {1, 0, 10, 1}, {1, 10, 0, 1}, {1, 10, 10, 0}} {
		var c capacityAge
		c.observe("a", at, low)
		c.observe("a", at.Add(70*time.Second), freeVec{1, 10, 10, 1})
		if c.aged(at.Add(71*time.Second)).covers(1, 10, 10) {
			t.Fatalf("fresh dimension aged: %+v", low)
		}
		if !c.aged(at.Add(131*time.Second)).covers(1, 10, 10) {
			t.Fatal("settled dimension refused")
		}
	}
	var c capacityAge
	for i := 0; i < 100; i++ {
		c.observe("a", at.Add(time.Duration(i)*time.Millisecond), freeVec{float64(2 + i%2), 20, 20, 2})
	}
	if len(c.History) > maxAgeHistory {
		t.Fatal("unbounded history")
	}
	if !c.aged(at.Add(61*time.Second)).covers(1, 10, 10) {
		t.Fatal("bounded merge lost conservative old gap")
	}
}

// Overflow merges keep the component-wise minimum: an old 1-cpu hole
// (at..at+1ms), followed by 40 transitions between 4 and 5 free cpus,
// overflows the bound, and the hole's entry is merged into later ones.
// Asked while the hole is still inside the 60s window, aged must not
// exceed it: a merge that kept the newer entry would claim 4 cpus.
func TestCapacityAgeOverflowKeepsMinimum(t *testing.T) {
	at := time.Now()
	var c capacityAge
	c.observe("a", at, freeVec{1, 10, 10, 1})
	for i := 1; i <= 40; i++ {
		c.observe("a", at.Add(time.Duration(i)*time.Millisecond), freeVec{float64(4 + i%2), 40, 40, 4})
	}
	if len(c.History) != maxAgeHistory {
		t.Fatalf("history of %d, want it merged to %d", len(c.History), maxAgeHistory)
	}
	got := c.aged(at.Add(spotSettle + 500*time.Microsecond))
	if got.covers(2, 20, 20) {
		t.Fatalf("aged %+v: the overflow merge forgot the older 1-cpu hole", got)
	}
	if !got.covers(1, 10, 10) {
		t.Fatalf("aged %+v: want the 1-cpu hole", got)
	}
}
