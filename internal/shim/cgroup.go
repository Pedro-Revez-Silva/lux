package shim

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/marcioapm/lux/internal/proto"
)

// A private cgroup namespace makes /sys/fs/cgroup the Run's root.
// Runs with sandbox.cgroupDelegation split it before tenant processes start:
//
//	/                 Run limits; no processes; controllers enabled below
//	├── lux.shim/     the shim (PID 1) and its own helpers
//	└── workload/     no processes; controllers enabled below; delegated
//	    └── main/     delegated; every tenant process starts here
//
// cgroup v2 requires internal groups to have no processes. Engines create
// siblings of main, whose subtree_control stays Run-root owned to keep it
// a leaf. Only workload's subtree_control is delegated; resource files and
// the descendant bound remain Run-root owned. See docs/podman.md.
const (
	cgroupRoot     = "/sys/fs/cgroup"
	shimCgroup     = "lux.shim"
	workloadCgroup = "workload"
	mainCgroup     = "workload/main"
	// maxWorkloadDescendants bounds the cgroups the workload can make
	// below workload/: an engine makes one or two per container, so this
	// leaves room for hundreds of containers while keeping a workload from
	// filling kernel memory with empty cgroups.
	maxWorkloadDescendants = 1024
)

// delegatedControllers are enabled below the Run's cgroup and below
// workload/, those the Run's cgroup has; requiredControllers must all be
// there, or the shim does not split the cgroup.
var (
	delegatedControllers = []string{"cpu", "memory", "pids", "io"}
	requiredControllers  = []string{"cpu", "memory", "pids"}
)

// workload/main keeps cgroup.subtree_control Run-root owned so it remains
// a leaf. Engines enable controllers in siblings under workload/.
var delegationFiles = []string{"cgroup.procs", "cgroup.threads"}

var (
	moveCgroupProcs    = moveProcs
	removeCgroup       = unix.Rmdir
	giveCgroup         = chownDelegated
	writeCgroupControl = os.WriteFile
)

// errNotDelegable means the Run's cgroup cannot be split here.
var errNotDelegable = errors.New("cgroup not delegable")

// delegableControllers checks that the cgroup at root can be split: its
// cgroup.subtree_control is writable and it has every required controller.
// It returns the controllers to enable, in delegatedControllers' order.
func delegableControllers(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotDelegable, err)
	}
	have := strings.Fields(string(b))
	var missing []string
	for _, c := range requiredControllers {
		if !slices.Contains(have, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: controllers %s missing (have %q)", errNotDelegable, strings.Join(missing, ", "), strings.TrimSpace(string(b)))
	}
	if err := unix.Access(filepath.Join(root, "cgroup.subtree_control"), unix.W_OK); err != nil {
		return nil, fmt.Errorf("%w: cgroup.subtree_control: %v", errNotDelegable, err)
	}
	var enable []string
	for _, c := range delegatedControllers {
		if slices.Contains(have, c) {
			enable = append(enable, c)
		}
	}
	return enable, nil
}

// subtreeControl is what cgroup.subtree_control is written to enable cs.
func subtreeControl(cs []string) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = "+" + c
	}
	return strings.Join(parts, " ")
}

// cgroupPids are the pids in a cgroup.procs file.
func cgroupPids(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// delegateCgroup splits the cgroup at root for a workload running as
// uid:gid and returns an open descriptor of workload/main, for
// CLONE_INTO_CGROUP. probe starts and waits for a process cloned into a
// cgroup by descriptor: one that fails means this kernel or seccomp
// profile refuses it, and nothing is changed. Errors trigger best-effort
// rollback; tenant processes always fall back to the shim's cgroup.
func delegateCgroup(root string, uid, gid int, probe func(fd int) error) (int, error) {
	enable, err := delegableControllers(root)
	if err != nil {
		return -1, err
	}
	for _, d := range []string{shimCgroup, workloadCgroup, mainCgroup} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return -1, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	fd, err := unix.Open(filepath.Join(root, mainCgroup), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", mainCgroup, err)
	}
	var enabled []string
	moved := false
	rollback := func() {
		for i := len(enabled) - 1; i >= 0; i-- {
			_ = writeCgroupControl(filepath.Join(enabled[i], "cgroup.subtree_control"), []byte(strings.ReplaceAll(subtreeControl(enable), "+", "-")), 0o644)
		}
		if moved {
			_ = moveCgroupProcs(filepath.Join(root, shimCgroup), root)
		}
		for _, d := range []string{mainCgroup, workloadCgroup, shimCgroup} {
			_ = removeCgroup(filepath.Join(root, d))
		}
	}
	fail := func(err error) (int, error) {
		unix.Close(fd)
		rollback()
		return -1, err
	}
	if err := probe(fd); err != nil {
		return fail(fmt.Errorf("starting a process into %s: %w", mainCgroup, err))
	}
	moved = true // A failed move can still have moved some processes.
	if err := moveCgroupProcs(root, filepath.Join(root, shimCgroup)); err != nil {
		return fail(err)
	}
	ctl := subtreeControl(enable)
	for _, d := range []string{root, filepath.Join(root, workloadCgroup)} {
		if err := writeCgroupControl(filepath.Join(d, "cgroup.subtree_control"), []byte(ctl), 0o644); err != nil {
			return fail(fmt.Errorf("enabling %q: %w", ctl, err))
		}
		enabled = append(enabled, d)
	}
	max := strconv.Itoa(maxWorkloadDescendants)
	if err := os.WriteFile(filepath.Join(root, workloadCgroup, "cgroup.max.descendants"), []byte(max), 0o644); err != nil {
		return fail(fmt.Errorf("%s/cgroup.max.descendants: %w", workloadCgroup, err))
	}
	for _, name := range []string{"cpu.max", "memory.max", "memory.swap.max"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(root, mainCgroup, name), b, 0o644); err != nil {
			return fail(err)
		}
	}
	if uid != 0 || gid != 0 {
		for _, d := range []string{workloadCgroup, mainCgroup} {
			if err := giveCgroup(filepath.Join(root, d), uid, gid); err != nil {
				return fail(err)
			}
		}
	}
	return fd, nil
}

// moveProcs moves every process of the cgroup at from into the cgroup at
// to. A process forked meanwhile lands in from too, so it reads again
// until from is empty, for a bounded time.
func moveProcs(from, to string) error {
	for range 50 {
		pids, err := cgroupPids(filepath.Join(from, "cgroup.procs"))
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			// ESRCH: it exited meanwhile.
			if err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("moving %d to %s: %w", pid, filepath.Base(to), err)
			}
		}
		remaining, err := cgroupPids(filepath.Join(from, "cgroup.procs"))
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("processes keep appearing in %s", from)
}

// chownDelegated gives the cgroup directory dir and its delegationFiles to
// uid:gid, through dir's descriptor, never following a link.
func chownDelegated(dir string, uid, gid int) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	files := slices.Clone(delegationFiles)
	if filepath.Base(dir) == workloadCgroup {
		files = append(files, "cgroup.subtree_control")
	}
	for _, f := range files {
		if err := unix.Fchownat(fd, f, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("chown %s/%s: %w", dir, f, err)
		}
	}
	return nil
}

// probeCloneInto starts `lux-shim capabilities`, cloned into the cgroup
// fd, and waits for it. Called before the reaper runs, so Wait reaps it.
func probeCloneInto(fd int) error {
	cmd := exec.Command(proto.ShimBinary, proto.ShimCapabilitiesCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	return cmd.Run()
}

// delegateAt keeps the workload/main descriptor for tenant starts, or
// warns once and falls back to the shim's cgroup on failure.
func (s *Shim) delegateAt(root string, probe func(int) error) {
	if !s.cfg.DelegateCgroup {
		return
	}
	u, err := lookupUser(s.cfg.User)
	if err == nil {
		var fd int
		if fd, err = delegateCgroup(root, u.uid, u.gid, probe); err == nil {
			s.workCgroup = os.NewFile(uintptr(fd), filepath.Join(root, mainCgroup))
		}
	}
	if err != nil {
		msg := "nested containers: the Run's cgroup is not delegated, so limits given to nested containers may not be enforced: " + err.Error()
		fmt.Fprintln(os.Stderr, "lux-shim:", msg)
		s.out.Event(proto.EvWarning, map[string]any{"message": msg})
	}
}

func (s *Shim) placementMemoryOOM(root string) *int64 {
	n := memoryOOM(filepath.Join(root, "memory.events.local"))
	if s.workCgroup != nil {
		leaf := memoryOOM(filepath.Join(root, mainCgroup, "memory.events.local"))
		if n == nil || leaf == nil {
			return nil
		}
		*n += *leaf
	}
	return n
}

// memoryOOM reads the cumulative local oom count, or nil if unavailable.
func memoryOOM(path string) *int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok || k != "oom" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	return nil
}
