// Package hoststat reads the machine's own CPU, memory and disk use from
// /proc and statfs: what runners report of their hosts, and luxd of its own.
package hoststat

import (
	"os"
	"strconv"
	"strings"
	"syscall"
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
	var total, avail int64
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail = n * 1024
		}
	}
	return Memory{Used: total - avail, Total: total}
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
