package runner

import (
	"math/big"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

const gib = int64(1) << 30

func mustScale(t *testing.T, memTotal, headroom, capacity int64) memoryScale {
	t.Helper()
	m, err := newMemoryScale(memTotal, headroom, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A 32 GiB machine whose Linux sees 30.5 GiB, 0.5 GiB kept for the kernel:
// 30 GiB for Runs, so two Runs asking 16 GiB each get 15 GiB.
func TestMemoryScaleSharesWhatLinuxSees(t *testing.T) {
	m := mustScale(t, 30*gib+gib/2, gib/2, 32*gib)
	if f := m.factor(); f != 0.9375 {
		t.Errorf("factor %v, want 0.9375", f)
	}
	if got := m.limit(16 * gib); got != 15*gib {
		t.Errorf("16 GiB asked: limit %d, want %d (15 GiB)", got, 15*gib)
	}
}

// Offering MemTotal (the default) still leaves the headroom to the kernel.
func TestMemoryScaleDefaultCapacityKeepsHeadroom(t *testing.T) {
	total := 61*gib + 440<<20
	m := mustScale(t, total, DefaultMemoryHeadroom, total)
	want := float64(total-DefaultMemoryHeadroom) / float64(total)
	if f := m.factor(); f != want {
		t.Errorf("factor %v, want %v", f, want)
	}
	if got := m.limit(total); got != total-DefaultMemoryHeadroom {
		t.Errorf("all of it asked: limit %d, want %d", got, total-DefaultMemoryHeadroom)
	}
}

// A host offering less than it can give is never scaled up.
func TestMemoryScaleNeverAboveOne(t *testing.T) {
	m := mustScale(t, 64*gib, gib/2, 16*gib)
	if f := m.factor(); f != 1 {
		t.Errorf("factor %v, want 1", f)
	}
	if got := m.limit(8 * gib); got != 8*gib {
		t.Errorf("limit %d, want %d", got, 8*gib)
	}
}

// wantLimit is the limit's contract in arbitrary precision: the request
// unchanged when nothing is scaled, else
// max(floor(req × (total − headroom) / capacity) down to a page, 1).
func wantLimit(req, total, headroom, capacity int64) int64 {
	allocatable := total - headroom
	if allocatable >= capacity {
		return req
	}
	n := new(big.Int).Mul(big.NewInt(req), big.NewInt(allocatable))
	n.Quo(n, big.NewInt(capacity))
	page := big.NewInt(int64(os.Getpagesize()))
	n.Mul(n.Quo(n, page), page)
	if n.Sign() == 0 {
		return 1
	}
	return n.Int64()
}

func TestMemoryScaleRoundsDown(t *testing.T) {
	page := int64(os.Getpagesize())
	for _, c := range []struct{ total, headroom, capacity int64 }{
		{30*gib + gib/2, gib / 2, 32 * gib},
		{64418888 << 10, DefaultMemoryHeadroom, 64 * gib},
		{7, 0, 9},
		{1<<62 + 3, 1, 1<<62 + 5},
		{1<<62 + 5, 3, 1<<62 - 1},
		{1<<63 - 1, 1, 1<<62 + page},
		{64 * gib, gib / 2, 16 * gib},
		{32 * gib, 0, 32 * gib},
	} {
		m := mustScale(t, c.total, c.headroom, c.capacity)
		reqs := []int64{1, 2, 3, 256 << 20, 8*gib + 7, c.capacity, c.capacity - 1, c.capacity + 1,
			1<<62 - page - 1, 1<<62 - page, 1<<62 - 1, 1 << 62, 1<<62 + 1, 1<<62 + 5, 1<<62 + page + 1}
		for _, k := range []int64{1, 2, 3, 1 << 20, 1 << 40} {
			reqs = append(reqs, k*page-1, k*page, k*page+1)
		}
		for _, req := range reqs {
			if got, want := m.limit(req), wantLimit(req, c.total, c.headroom, c.capacity); got != want {
				t.Errorf("%+v: limit(%d) = %d, want %d", c, req, got, want)
			}
		}
	}
}

func TestMemoryScaleRefusesNoRoom(t *testing.T) {
	for _, c := range []struct{ total, headroom int64 }{{gib, gib}, {gib, 2 * gib}, {0, 0}, {gib, -1}} {
		if _, err := newMemoryScale(c.total, c.headroom, 32*gib); err == nil {
			t.Errorf("MemTotal %d, headroom %d: accepted", c.total, c.headroom)
		}
	}
}

// The container is created with the scaled limit as both --memory and
// --memory-swap.
func TestContainerGetsTheScaledMemory(t *testing.T) {
	r := &Runner{mem: mustScale(t, 30*gib+gib/2, gib/2, 32*gib)}
	p := &placement{r: r, runID: "run_1", tenantID: "ten_1", state: &runState{}}
	sp := spec.RunSpec{Resources: spec.Resources{CPUs: 1, Memory: spec.Bytes(16 * gib), Pids: 64}}
	args := p.createArgs(sp, "img", podman.Network{})
	for _, flag := range []string{"--memory", "--memory-swap"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("no %s in %v", flag, args)
		}
		if args[i+1] != strconv.FormatInt(15*gib, 10) {
			t.Errorf("%s %s, want %d", flag, args[i+1], 15*gib)
		}
	}
}
