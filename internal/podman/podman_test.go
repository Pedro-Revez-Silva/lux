package podman

import (
	"io"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/hoststat"
)

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

// RunIO's stderr keeps only its last 4 KiB, however much is written.
func TestRunIOStderrIsBounded(t *testing.T) {
	p := &Podman{Bin: "sh"}
	err := p.RunIO(t.Context(), nil, io.Discard, "-c", `head -c 50000000 /dev/zero | tr '\0' x >&2; printf END >&2; exit 3`)
	if err == nil || !strings.HasSuffix(err.Error(), "xEND") || len(err.Error()) > 4096+200 {
		t.Fatalf("%.200s… (%d bytes)", err, len(err.Error()))
	}
}
