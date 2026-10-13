package runner

import "math/bits"

// share is allocatable/capacity, never above 1: what part of a nominal
// amount a host really has. The zero value scales nothing.
type share struct {
	allocatable, capacity uint64
}

// newShare is allocatable over capacity; a capacity of 0 or one not above
// allocatable is not scaled. allocatable must be positive.
func newShare(allocatable, capacity int64) share {
	if capacity <= 0 || allocatable >= capacity {
		return share{allocatable: 1, capacity: 1}
	}
	return share{allocatable: uint64(allocatable), capacity: uint64(capacity)}
}

func (s share) factor() float64 {
	if s.capacity == 0 {
		return 1
	}
	return float64(s.allocatable) / float64(s.capacity)
}

func (s share) scaled() bool { return s.capacity != 0 && s.allocatable != s.capacity }

// of is floor(n × allocatable / capacity) in exact integer arithmetic: never
// above n. n not positive, or nothing scaled, is n.
func (s share) of(n int64) int64 {
	if n <= 0 || !s.scaled() {
		return n
	}
	hi, lo := bits.Mul64(uint64(n), s.allocatable)
	q, _ := bits.Div64(hi, lo, s.capacity) // allocatable < capacity: q <= n, no overflow
	return int64(q)
}
