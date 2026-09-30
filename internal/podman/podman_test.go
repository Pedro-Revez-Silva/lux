package podman

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/hoststat"
)

// A build runs under the cgroupfs manager, set before the subcommand (a
// global flag), whatever the host's containers.conf defaults to: the
// runner's --cgroup-parent is a cgroupfs path, which the systemd manager
// cannot place a build step under.
func TestBuildUsesCgroupfs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Podman{Bin: bin}
	if _, err := p.Build(context.Background(), "--cgroup-parent", "/lux.slice/build-run1", "-f", "Containerfile", "ctx"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(bin + ".args")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	build := slices.Index(args, "build")
	if build < 0 || !slices.Contains(args[:build], "--cgroup-manager=cgroupfs") {
		t.Fatalf("podman %q: want --cgroup-manager=cgroupfs before build", args)
	}
	if !slices.Equal(args[build+1:], []string{"--cgroup-parent", "/lux.slice/build-run1", "-f", "Containerfile", "ctx"}) {
		t.Fatalf("podman %q: build's own arguments changed", args)
	}
}

// The runner's heartbeat usage is hoststat's: CPU busy seconds, memory used
// and the disk used on dir's filesystem. The machine keeps changing, so each
// value must lie between hoststat's reads just before and just after (memory
// and disk, which can also shrink, within slack; a wrong mapping such as
// total or free for used is off by far more).
func TestReadHostUsage(t *testing.T) {
	dir := t.TempDir()
	read := func() (hoststat.CPU, hoststat.Memory, hoststat.Disk) {
		t.Helper()
		c, err := hoststat.ReadCPU()
		if err != nil {
			t.Fatal(err)
		}
		m, err := hoststat.ReadMemory()
		if err != nil {
			t.Fatal(err)
		}
		d, err := hoststat.ReadDisk(dir)
		if err != nil {
			t.Fatal(err)
		}
		return c, m, d
	}
	const slack = 32 << 20
	within := func(v, a, b int64) bool { return v > 0 && v >= min(a, b)-slack && v <= max(a, b)+slack }
	c0, m0, d0 := read()
	u, err := ReadHostUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	c1, m1, d1 := read()
	if u.CPUSeconds <= 0 || u.CPUSeconds < c0.Seconds || u.CPUSeconds > c1.Seconds {
		t.Errorf("cpu seconds %v, hoststat read %v then %v", u.CPUSeconds, c0.Seconds, c1.Seconds)
	}
	if !within(u.MemoryBytes, m0.Used, m1.Used) {
		t.Errorf("memory %d, hoststat used %d then %d", u.MemoryBytes, m0.Used, m1.Used)
	}
	if !within(u.DiskBytes, d0.Used, d1.Used) {
		t.Errorf("disk %d, hoststat used %d then %d", u.DiskBytes, d0.Used, d1.Used)
	}
}
