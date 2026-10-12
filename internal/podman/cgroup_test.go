package podman

import (
	"os"
	"path/filepath"
	"testing"
)

// A cgroup directory with every file a cgroup v2 kernel with PSI has: the
// pressure totals come out in seconds, cpu's "full" line included.
func TestCgroupUsagePressure(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"memory.peak":     "1048576\n",
		"pids.peak":       "12\n",
		"memory.current":  "524288\n",
		"pids.current":    "3\n",
		"cpu.stat":        "usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n",
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=1500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=700000\n",
		"memory.pressure": "some avg10=1.00 avg60=0.50 avg300=0.10 total=250000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=125000\n",
		"io.pressure":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=3000000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	})
	u := cgroupUsageAt(dir)
	if u.PeakMemoryBytes != 1048576 || u.PeakPids != 12 || u.MemoryBytes != 524288 || u.Pids != 3 || u.CPUSeconds != 2.5 {
		t.Fatalf("usage %+v", u)
	}
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"cpu some": {u.CPUStall, 1.5}, "memory some": {u.MemoryStall, 0.25}, "memory full": {u.MemoryFullStall, 0.125},
		"io some": {u.IOStall, 3}, "io full": {u.IOFullStall, 0},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s: %v, want %v", name, ptr(c.got), c.want)
		}
	}
}

// A kernel without PSI has no *.pressure files: every stall is absent, not
// 0. A pressure file with only a "some" line (cpu.pressure before Linux
// 5.13) leaves "full" absent.
func TestCgroupUsageWithoutPressure(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"cpu.stat": "usage_usec 1000000\n"})
	u := cgroupUsageAt(dir)
	if u.CPUSeconds != 1 {
		t.Fatalf("cpu seconds %v", u.CPUSeconds)
	}
	if u.CPUStall != nil || u.MemoryStall != nil || u.MemoryFullStall != nil || u.IOStall != nil || u.IOFullStall != nil {
		t.Fatalf("stalls without pressure files: %v %v %v %v %v", ptr(u.CPUStall), ptr(u.MemoryStall), ptr(u.MemoryFullStall), ptr(u.IOStall), ptr(u.IOFullStall))
	}

	write(t, dir, map[string]string{"io.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=42\n"})
	u = cgroupUsageAt(dir)
	if u.IOStall == nil || *u.IOStall != 42e-6 || u.IOFullStall != nil {
		t.Fatalf("io some %v full %v, want 4.2e-05 and absent", ptr(u.IOStall), ptr(u.IOFullStall))
	}
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func ptr(v *float64) any {
	if v == nil {
		return "absent"
	}
	return *v
}
