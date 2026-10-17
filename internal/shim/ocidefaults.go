package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
)

// OCIDefaults is `lux-shim oci-defaults MEMORY CPU_QUOTA CPU_PERIOD`, the
// precreate OCI hook nested Runs register with Podman inside them
// (docs/podman.md, Limits of nested containers). It reads the proposed OCI
// runtime config on stdin and writes it back with the Run's memory limit
// and CPU quota where the container was given none: a container in its own
// cgroup namespace cannot see the Run's limits above it, and ClickHouse or
// the JVM would size themselves to the host. An explicit -m or --cpus at or
// below the Run's is kept; one above it (or unlimited) is lowered to the
// Run's, which caps it anyway. --cgroup-conf memory.max/cpu.max is kept as
// given. 0 leaves a resource as it is.
func OCIDefaults(args []string, in io.Reader, out io.Writer) int {
	if err := ociDefaults(args, in, out); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim oci-defaults:", err)
		return 1
	}
	return 0
}

func ociDefaults(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 3 {
		return fmt.Errorf("want MEMORY CPU_QUOTA CPU_PERIOD, got %q", args)
	}
	var n [3]int64
	for i, a := range args {
		v, err := strconv.ParseInt(a, 10, 64)
		if err != nil || v < 0 {
			return fmt.Errorf("%q: want a non-negative integer", a)
		}
		n[i] = v
	}
	memory, quota, period := n[0], n[1], n[2]
	dec := json.NewDecoder(in)
	// Numbers stay as written: the config carries 64-bit values.
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return err
	}
	linux := child(cfg, "linux")
	res := child(linux, "resources")
	unified, _ := res["unified"].(map[string]any)
	mem := child(res, "memory")
	if limit, set := mem["limit"]; set && memory > 0 {
		// -m above the Run's limit, or -1 or 0 (no limit to the runtimes),
		// would read as more than the Run can give.
		if n, ok := ociInt(limit); !ok || n <= 0 || n > memory {
			mem["limit"] = memory
		}
	} else if !set && unified["memory.max"] == nil && memory > 0 {
		mem["limit"] = memory
		if _, set := mem["swap"]; !set && unified["memory.swap.max"] == nil {
			mem["swap"] = memory
		}
	}
	cpu := child(res, "cpu")
	if q, set := cpu["quota"]; set && quota > 0 && period > 0 {
		if !withinQuota(q, cpu["period"], quota, period) {
			cpu["quota"], cpu["period"] = quota, period
		}
	} else if !set && unified["cpu.max"] == nil && quota > 0 && period > 0 {
		cpu["quota"], cpu["period"] = quota, period
	}
	prune(res, "memory")
	prune(res, "cpu")
	prune(linux, "resources")
	return json.NewEncoder(out).Encode(cfg)
}

// ociInt is an OCI config number as an int64.
func ociInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// withinQuota: quota q per period p (an absent or 0 period is the kernel's
// default, 100000us) is at most runQuota per runPeriod. A quota of -1 or 0
// is unlimited, so not within.
func withinQuota(q, p any, runQuota, runPeriod int64) bool {
	quota, ok := ociInt(q)
	if !ok || quota <= 0 {
		return false
	}
	period, ok := ociInt(p)
	if !ok || period <= 0 {
		period = 100000
	}
	// quota/period <= runQuota/runPeriod, without overflow.
	lhs := new(big.Int).Mul(big.NewInt(quota), big.NewInt(runPeriod))
	return lhs.Cmp(new(big.Int).Mul(big.NewInt(runQuota), big.NewInt(period))) <= 0
}

// child is m[key] as an object, made if absent.
func child(m map[string]any, key string) map[string]any {
	c, ok := m[key].(map[string]any)
	if !ok {
		c = map[string]any{}
		m[key] = c
	}
	return c
}

// prune drops m[key] if it is an empty object, as it was before child.
func prune(m map[string]any, key string) {
	if c, ok := m[key].(map[string]any); ok && len(c) == 0 {
		delete(m, key)
	}
}
