package runner

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/hoststat"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
)

// statfsOf is a statfs that answers d for any directory.
func statfsOf(d hoststat.Disk) func(string) (hoststat.Disk, error) {
	return func(string) (hoststat.Disk, error) { return d, nil }
}

// fsOf is a filesystem whose capacity (used + free) is capacity bytes, some
// of it in use and some reserved for root, which counts in neither.
func fsOf(capacity int64) hoststat.Disk {
	used := capacity / 3
	return hoststat.Disk{Used: used, Free: capacity - used, Total: capacity + capacity/20}
}

func mustDiskScale(t *testing.T, nominal, headroom, capacity int64) diskScale {
	t.Helper()
	d, err := newDiskScale(nominal, &headroom, "/data", statfsOf(fsOf(capacity)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The design's table: 160 GiB nominal, 160 GiB usable, 10 GiB headroom.
func TestDiskScaleDesignTable(t *testing.T) {
	d := mustDiskScale(t, 160*gib, 10*gib, 160*gib)
	if f := d.factor(); f != 0.9375 {
		t.Errorf("factor %v, want 0.9375", f)
	}
	for _, c := range []struct{ nominal, effective int64 }{
		{10 * gib, 9*gib + 3*gib/8},  // 9.375
		{20 * gib, 18*gib + 3*gib/4}, // 18.75
		{40 * gib, 37*gib + gib/2},   // 37.5
		{80 * gib, 75 * gib},
		{160 * gib, 150 * gib},
	} {
		if got := d.limit(c.nominal); got != c.effective {
			t.Errorf("%d GiB nominal: limit %d, want %d", c.nominal/gib, got, c.effective)
		}
	}
}

// A filesystem larger than the nominal disk leaves every limit as asked:
// never scaled up.
func TestDiskScaleClampsToOne(t *testing.T) {
	for _, capacity := range []int64{170 * gib, 1 << 50, 160*gib + 10*gib} {
		d := mustDiskScale(t, 160*gib, 10*gib, capacity)
		if !d.on || d.factor() != 1 {
			t.Errorf("capacity %d: on %v factor %v, want on at 1", capacity, d.on, d.factor())
		}
		if got := d.limit(40 * gib); got != 40*gib {
			t.Errorf("capacity %d: limit %d, want 40 GiB", capacity, got)
		}
	}
}

// A headroom that leaves nothing is refused, naming both numbers.
func TestDiskScaleRefusesNoRoom(t *testing.T) {
	for _, c := range []struct{ headroom, capacity int64 }{{10 * gib, 10 * gib}, {11 * gib, 10 * gib}, {0, 0}} {
		_, err := newDiskScale(160*gib, &c.headroom, "/data", statfsOf(fsOf(c.capacity)))
		if err == nil {
			t.Errorf("headroom %d on %d bytes: accepted", c.headroom, c.capacity)
			continue
		}
		for _, n := range []int64{c.headroom, c.capacity} {
			if !strings.Contains(err.Error(), big.NewInt(n).String()) {
				t.Errorf("error %q does not name %d", err, n)
			}
		}
	}
	neg := int64(-1)
	if _, err := newDiskScale(160*gib, &neg, "/data", statfsOf(fsOf(gib))); err == nil {
		t.Error("negative headroom: accepted")
	}
	zero := int64(0)
	if _, err := newDiskScale(160*gib, &zero, "/data", func(string) (hoststat.Disk, error) { return hoststat.Disk{}, errors.New("no fs") }); err == nil {
		t.Error("statfs failure: accepted")
	}
}

// Without a nominal disk, or without a headroom, nothing is scaled and
// the filesystem is not even read: a runner started as before keeps its
// limits.
func TestDiskScaleLegacyIsOff(t *testing.T) {
	never := func(string) (hoststat.Disk, error) { t.Fatal("statfs read"); return hoststat.Disk{}, nil }
	ten := 10 * gib
	for _, c := range []struct {
		name     string
		nominal  int64
		headroom *int64
	}{
		{"legacy --disk, no headroom", 160 * gib, nil},
		{"headroom without --disk", 0, &ten},
		{"neither", 0, nil},
	} {
		d, err := newDiskScale(c.nominal, c.headroom, "/data", never)
		if err != nil || d.on {
			t.Fatalf("%s: on %v err %v, want off", c.name, d.on, err)
		}
		for _, n := range []int64{0, 1, 40 * gib, math.MaxInt64} {
			if got := d.limit(n); got != n {
				t.Errorf("%s: limit(%d) = %d", c.name, n, got)
			}
		}
	}
}

// A headroom of 0 is scaling on: limits follow the filesystem's capacity.
func TestDiskScaleZeroHeadroom(t *testing.T) {
	d := mustDiskScale(t, 160*gib, 0, 150*gib)
	if !d.on || d.limit(160*gib) != 150*gib {
		t.Errorf("on %v limit %d, want on and 150 GiB", d.on, d.limit(160*gib))
	}
}

// wantDiskLimit is the contract in arbitrary precision:
// floor(req × min(1, (capacity − headroom) / nominal)).
func wantDiskLimit(req, nominal, headroom, capacity int64) int64 {
	alloc := capacity - headroom
	if alloc >= nominal {
		return req
	}
	n := new(big.Int).Mul(big.NewInt(req), big.NewInt(alloc))
	n.Quo(n, big.NewInt(nominal)) // both positive: Quo truncates, which is floor
	return n.Int64()
}

// Exact for every size up to MaxInt64, always rounded down, never above
// the request.
func TestDiskScaleRoundsDownWithoutOverflow(t *testing.T) {
	for _, c := range []struct{ nominal, headroom, capacity int64 }{
		{160 * gib, 10 * gib, 160 * gib},
		{160 * gib, 10 * gib, 149*gib + 7},
		{9, 0, 7},
		{3, 1, 3},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MaxInt64, 0, math.MaxInt64 - 1},
		{1<<62 + 5, 3, 1<<62 + 1},
		{math.MaxInt64, math.MaxInt64 - 2, math.MaxInt64},
	} {
		d := mustDiskScale(t, c.nominal, c.headroom, c.capacity)
		for _, req := range []int64{1, 2, 3, 7, gib - 1, 10 * gib, 40*gib + 1, c.nominal - 1, c.nominal, 1<<62 + 1, math.MaxInt64 - 1, math.MaxInt64} {
			got, want := d.limit(req), wantDiskLimit(req, c.nominal, c.headroom, c.capacity)
			if got != want {
				t.Errorf("%+v: limit(%d) = %d, want %d", c, req, got, want)
			}
			if got > req {
				t.Errorf("%+v: limit(%d) = %d, above the request", c, req, got)
			}
		}
	}
	// 3 bytes at 2/3 is exactly 2; 1 byte at 2/3 rounds down to 0.
	d := mustDiskScale(t, 3, 1, 3)
	if d.limit(3) != 2 || d.limit(2) != 1 || d.limit(1) != 0 {
		t.Errorf("2/3: limit(3,2,1) = %d,%d,%d, want 2,1,0", d.limit(3), d.limit(2), d.limit(1))
	}
}

// The filesystem's capacity is its used plus unprivileged-free bytes:
// blocks reserved for root count in neither, and free bytes alone (which
// shrink as Runs write) are never the capacity.
func TestDiskScaleReadsCapacityNotFree(t *testing.T) {
	h := 10 * gib
	d, err := newDiskScale(160*gib, &h, "/data", statfsOf(hoststat.Disk{Used: 100 * gib, Free: 60 * gib, Total: 168 * gib}))
	if err != nil {
		t.Fatal(err)
	}
	if d.fsCapacity != 160*gib || d.limit(160*gib) != 150*gib {
		t.Errorf("fsCapacity %d limit %d, want 160 GiB and 150 GiB", d.fsCapacity, d.limit(160*gib))
	}
}

// The runner advertises disk-scale, and the nominal disk, headroom and
// capacity in Hello, only with scaling on.
func TestHelloAdvertisesDiskScale(t *testing.T) {
	on := &Runner{cfg: Config{Disk: 160 * gib}, disk: mustDiskScale(t, 160*gib, 10*gib, 155*gib)}
	if !slices.Contains(on.capabilities(), proto.CapDiskScale) {
		t.Errorf("scaling on: capabilities %v lack %s", on.capabilities(), proto.CapDiskScale)
	}
	off := &Runner{cfg: Config{Disk: 160 * gib}}
	if slices.Contains(off.capabilities(), proto.CapDiskScale) {
		t.Errorf("legacy --disk: capabilities %v carry %s", off.capabilities(), proto.CapDiskScale)
	}
	free := int64(5)
	usage := podman.HostUsage{DiskBytes: 1, DiskFree: free, DiskOK: true}
	u := on.hostUsage(usage)
	if u.FSCapacityBytes == nil || *u.FSCapacityBytes != 155*gib || u.DiskHeadroomBytes == nil || *u.DiskHeadroomBytes != 10*gib ||
		u.FSFreeBytes == nil || *u.FSFreeBytes != free {
		t.Errorf("scaling on: heartbeat usage %+v", u)
	}
	u = off.hostUsage(usage)
	if u.FSCapacityBytes != nil || u.DiskHeadroomBytes != nil || u.FSFreeBytes == nil || *u.FSFreeBytes != free {
		t.Errorf("legacy: heartbeat usage %+v, want free bytes only", u)
	}
	if want := (proto.Capacity{Disk: 160 * gib, DiskHeadroom: 10 * gib, FSCapacity: 155 * gib}); on.capacity() != want {
		t.Errorf("scaling on: capacity %+v, want %+v", on.capacity(), want)
	}
	if want := (proto.Capacity{Disk: 160 * gib}); off.capacity() != want {
		t.Errorf("legacy: capacity %+v, want %+v", off.capacity(), want)
	}
}
