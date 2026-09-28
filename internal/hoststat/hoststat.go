// Package hoststat reads the machine's own CPU, memory and disk use from
// /proc and statfs, and the calling process's: what runners report of their
// hosts and themselves, and luxd of its own.
package hoststat

import (
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// CPU is the CPU time used since boot, over all cores, and how many cores
// there are.
type CPU struct {
	Seconds float64
	Cores   int
}

// ReadCPU reads /proc/stat.
func ReadCPU() (CPU, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return CPU{}, err
	}
	return parseStat(string(b)), nil
}

// parseStat: "cpu  user nice system idle iowait irq softirq steal guest
// guest_nice" in USER_HZ (100 on Linux). Busy is everything but idle and
// iowait; guest time is already counted in user. Each "cpuN" line is a core.
func parseStat(s string) CPU {
	var c CPU
	for _, line := range strings.Split(s, "\n") {
		name, rest, _ := strings.Cut(line, " ")
		switch {
		case name == "cpu":
			for i, f := range strings.Fields(rest) {
				if i == 3 || i == 4 || i >= 8 {
					continue
				}
				n, _ := strconv.ParseInt(f, 10, 64)
				c.Seconds += float64(n) / 100
			}
		case strings.HasPrefix(name, "cpu"):
			c.Cores++
		}
	}
	return c
}

// Memory is the memory in use (MemTotal - MemAvailable) and the total, in
// bytes.
type Memory struct {
	Used, Total int64
}

// ReadMemory reads /proc/meminfo.
func ReadMemory() (Memory, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	return parseMeminfo(string(b)), nil
}

func parseMeminfo(s string) Memory {
	kb := parseKB(s, "MemTotal:", "MemAvailable:")
	return Memory{Used: kb["MemTotal:"] - kb["MemAvailable:"], Total: kb["MemTotal:"]}
}

// parseKB reads "Key: N kB" lines (/proc/meminfo, /proc/self/status), in
// bytes, for the keys asked for.
func parseKB(s string, keys ...string) map[string]int64 {
	out := make(map[string]int64, len(keys))
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || !slices.Contains(keys, f[0]) {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		out[f[0]] = n * 1024
	}
	return out
}

// Disk is the filesystem holding a directory, in bytes. Free is what an
// unprivileged process can still write (blocks reserved for root excluded),
// so Used + Free can be less than Total.
type Disk struct {
	Used, Free, Total int64
}

// ReadDisk statfs's dir.
func ReadDisk(dir string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Disk{}, err
	}
	bs := int64(st.Bsize)
	return Disk{
		Used:  int64(st.Blocks-st.Bfree) * bs,
		Free:  int64(st.Bavail) * bs,
		Total: int64(st.Blocks) * bs,
	}, nil
}

var started = time.Now()

// ProcessSampler reads the calling process's own use (what luxd reports of
// itself, and a runner of itself, not of the podman and conmon processes
// it starts) for one sampler: luxd's system tick, the runner's heartbeat.
// It owns the peak RSS window: each Read resets the kernel's high-water
// mark at once and keeps the peak read pending, so nothing between reads
// is missed; a reading that was not stored leaves its peak to the next
// one, until Stored. A process has one.
type ProcessSampler struct {
	mu      sync.Mutex
	pending int64 // highest peak read since the last Stored
}

// Read reads getrusage, /proc/self/status and the Go runtime. The peak is
// left out when the kernel's mark cannot be reset (a non-dumpable process,
// some sandboxes): it would be the lifetime peak.
func (s *ProcessSampler) Read() (*proto.ProcessUsage, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return nil, err
	}
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	// 5 resets the high-water mark to the RSS now (proc(5), clear_refs).
	reset := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
	kb := parseKB(string(b), "VmRSS:", "VmHWM:")
	m := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(m)
	p := &proto.ProcessUsage{Started: started, CPUSeconds: seconds(ru.Utime) + seconds(ru.Stime), RSSBytes: kb["VmRSS:"],
		HeapBytes: int64(m[0].Value.Uint64()), Goroutines: int64(runtime.NumGoroutine())}
	if reset {
		s.mu.Lock()
		s.pending = max(s.pending, kb["VmHWM:"])
		peak := s.pending
		s.mu.Unlock()
		p.PeakRSSBytes = &peak
	}
	return p, nil
}

// Stored says the last Read's reading was stored: the next peak starts
// from what the process holds then.
func (s *ProcessSampler) Stored() {
	s.mu.Lock()
	s.pending = 0
	s.mu.Unlock()
}

func seconds(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
