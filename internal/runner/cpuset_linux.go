package runner

import (
	"errors"

	"golang.org/x/sys/unix"
)

// affinityCPUs are the CPUs the runner itself may run on: the host's, or
// fewer when an operator confines the runner (taskset, a systemd
// CPUAffinity=), which then confines its Runs too.
func affinityCPUs() ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil, err
	}
	var out []int
	for c := range len(set) * 64 {
		if set.IsSet(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no CPUs in the runner's affinity mask")
	}
	return out, nil
}
