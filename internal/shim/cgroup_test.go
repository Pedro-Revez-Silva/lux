package shim

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"golang.org/x/sys/unix"
)

// fakeCgroup makes a directory shaped like a cgroup v2 root (its files as
// plain files) with controllers ctl and no processes.
func fakeCgroup(t *testing.T, ctl string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		"cgroup.controllers": ctl + "\n", "cgroup.subtree_control": "", "cgroup.procs": "",
		"memory.max": "268435456\n", "cpu.max": "200000 100000\n", "memory.swap.max": "0\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// withChildren adds the cgroups delegateCgroup makes, with the files the
// kernel gives a new cgroup (plain files here).
func withChildren(t *testing.T, root string) string {
	t.Helper()
	for _, d := range []string{shimCgroup, workloadCgroup, mainCgroup} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"cgroup.procs", "cgroup.threads", "cgroup.max.descendants", "cgroup.subtree_control", "cpu.max", "memory.max", "memory.swap.max", "pids.max"} {
			if err := os.WriteFile(filepath.Join(root, d, f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestDelegableControllers(t *testing.T) {
	got, err := delegableControllers(fakeCgroup(t, "cpuset cpu io memory hugetlb pids rdma misc"))
	if err != nil || strings.Join(got, " ") != "cpu memory pids io" {
		t.Fatalf("all controllers: %v, %v", got, err)
	}
	// io is enabled only when the Run's cgroup has it.
	got, err = delegableControllers(fakeCgroup(t, "cpu memory pids"))
	if err != nil || strings.Join(got, " ") != "cpu memory pids" {
		t.Fatalf("without io: %v, %v", got, err)
	}
	if _, err := delegableControllers(fakeCgroup(t, "cpu io pids")); !errors.Is(err, errNotDelegable) || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("without memory: %v", err)
	}
	if _, err := delegableControllers(t.TempDir()); !errors.Is(err, errNotDelegable) {
		t.Fatalf("not a cgroup: %v", err)
	}
	if os.Geteuid() != 0 {
		ro := fakeCgroup(t, "cpu memory pids io")
		if err := os.Chmod(filepath.Join(ro, "cgroup.subtree_control"), 0o444); err != nil {
			t.Fatal(err)
		}
		if _, err := delegableControllers(ro); !errors.Is(err, errNotDelegable) || !strings.Contains(err.Error(), "subtree_control") {
			t.Fatalf("read-only subtree_control: %v", err)
		}
	}
}

// The layout delegateCgroup leaves: the three cgroups, controllers enabled
// at the root and in workload/, the descendants bound on workload/, the Run's
// limits untouched, and the descriptor it returns is workload/main's, which
// the probe was given before any of it was written.
func TestDelegateCgroupLayout(t *testing.T) {
	root := withChildren(t, fakeCgroup(t, "cpuset cpu io memory pids"))
	var probed unix.Stat_t
	var subtreeAtProbe string
	fd, err := delegateCgroup(root, os.Getuid(), os.Getgid(), func(fd int) error {
		if err := unix.Fstat(fd, &probed); err != nil {
			t.Fatal(err)
		}
		subtreeAtProbe = readTrim(t, filepath.Join(root, "cgroup.subtree_control"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	for _, d := range []string{shimCgroup, workloadCgroup, mainCgroup} {
		if fi, err := os.Stat(filepath.Join(root, d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if got := readTrim(t, filepath.Join(root, "cgroup.subtree_control")); got != "+cpu +memory +pids +io" {
		t.Errorf("root subtree_control %q", got)
	}
	if got := readTrim(t, filepath.Join(root, workloadCgroup, "cgroup.subtree_control")); got != "+cpu +memory +pids +io" {
		t.Errorf("workload subtree_control %q", got)
	}
	if got := readTrim(t, filepath.Join(root, workloadCgroup, "cgroup.max.descendants")); got != "1024" {
		t.Errorf("workload cgroup.max.descendants %q", got)
	}
	if got := readTrim(t, filepath.Join(root, "memory.max")); got != "268435456" {
		t.Errorf("the Run's memory.max changed: %q", got)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	main, _ := os.Stat(filepath.Join(root, mainCgroup))
	if ms := main.Sys().(*syscall.Stat_t); ms.Dev != st.Dev || ms.Ino != st.Ino {
		t.Errorf("descriptor is not workload/main's")
	}
	if probed.Dev != st.Dev || probed.Ino != st.Ino || subtreeAtProbe != "" {
		t.Errorf("probe: got %v, subtree_control then %q", probed, subtreeAtProbe)
	}
}

// A kernel or seccomp profile that refuses CLONE_INTO_CGROUP: nothing is
// left changed, and the error says what failed.
func TestDelegateCgroupProbeFailsChangesNothing(t *testing.T) {
	root := fakeCgroup(t, "cpu io memory pids")
	fd, err := delegateCgroup(root, os.Getuid(), os.Getgid(), func(int) error { return syscall.ENOSYS })
	if err == nil || fd != -1 || !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("fd %d, err %v", fd, err)
	}
	for _, d := range []string{shimCgroup, workloadCgroup} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", d, err)
		}
	}
	if got := readTrim(t, filepath.Join(root, "cgroup.subtree_control")); got != "" {
		t.Errorf("subtree_control written: %q", got)
	}
}

func TestDelegateCgroupRefusesWithoutControllers(t *testing.T) {
	root := fakeCgroup(t, "cpu io")
	called := false
	if _, err := delegateCgroup(root, 1000, 1000, func(int) error { called = true; return nil }); !errors.Is(err, errNotDelegable) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, workloadCgroup)); !os.IsNotExist(err) || called {
		t.Fatalf("touched the cgroup: %v, probe %v", err, called)
	}
}

// Tenant processes start in workload/main once delegated; the shim's own
// sync stays where the shim is; without delegation nothing is set.
func TestCommandStartsInTheWorkloadCgroup(t *testing.T) {
	s := &Shim{user: &userInfo{home: "/"}}
	if cmd := s.command([]string{"/bin/true"}, nil); cmd.SysProcAttr.UseCgroupFD {
		t.Fatal("undelegated: cloned into a cgroup")
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	s.workCgroup = dir
	if cmd := s.syncCommand(proto.SyncArgs{}, nil); cmd.SysProcAttr.UseCgroupFD {
		t.Fatal("sync cloned into workload cgroup")
	}
	cmd := s.command([]string{"/bin/true"}, nil)
	if !cmd.SysProcAttr.UseCgroupFD || cmd.SysProcAttr.CgroupFD != int(dir.Fd()) {
		t.Fatalf("delegated: %+v", cmd.SysProcAttr)
	}
}

func TestDelegateFallbackStartsCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	out, err := OpenOutput(path, NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := &Shim{cfg: proto.ShimConfig{DelegateCgroup: true}, out: out, user: &userInfo{home: "/"}}
	s.delegateAt(fakeCgroup(t, "cpu pids"), func(int) error { t.Fatal("unexpected probe"); return nil })
	out.Close()
	if s.workCgroup != nil {
		t.Fatal("fallback retained cgroup fd")
	}
	cmd := s.command([]string{"/bin/true"}, nil)
	if cmd.SysProcAttr.UseCgroupFD {
		t.Fatal("fallback uses cgroup fd")
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	warnings := 0
	for _, line := range lines {
		var ev struct {
			Event struct {
				Type string `json:"type"`
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Event.Type == "lux.warning" {
			warnings++
		}
	}
	if warnings != 1 || !strings.Contains(string(b), "not delegated") {
		t.Fatalf("warnings %d: %s", warnings, b)
	}
}

func TestDelegateExactOwnershipAndLeafLimits(t *testing.T) {
	root := withChildren(t, fakeCgroup(t, "cpu memory pids"))
	old := giveCgroup
	t.Cleanup(func() { giveCgroup = old })
	var targets []string
	giveCgroup = func(dir string, uid, gid int) error {
		if uid != 1234 || gid != 1234 {
			t.Fatalf("owner %d:%d", uid, gid)
		}
		targets = append(targets, filepath.Base(dir))
		if os.Geteuid() == 0 {
			return old(dir, uid, gid)
		}
		return nil
	}
	fd, err := delegateCgroup(root, 1234, 1234, func(int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if strings.Join(targets, ",") != "workload,main" {
		t.Fatal(targets)
	}
	for _, name := range []string{"cpu.max", "memory.max", "memory.swap.max"} {
		if got, want := readTrim(t, filepath.Join(root, mainCgroup, name)), readTrim(t, filepath.Join(root, name)); got != want {
			t.Fatalf("%s: %q != %q", name, got, want)
		}
	}
	// Stat every control/resource file after the actual delegation chowns.
	if os.Geteuid() != 0 {
		t.Skip("ownership stat requires root; copy and delegation targets checked")
	}
	for _, d := range []string{workloadCgroup, mainCgroup} {
		entries, err := os.ReadDir(filepath.Join(root, d))
		if err != nil {
			t.Fatal(err)
		}
		names := []string{"."}
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		for _, name := range names {
			fi, err := os.Lstat(filepath.Join(root, d, name))
			if err != nil {
				t.Fatal(err)
			}
			want := uint32(0)
			if name == "." || name == "cgroup.procs" || name == "cgroup.threads" || d == workloadCgroup && name == "cgroup.subtree_control" {
				want = 1234
			}
			st := fi.Sys().(*syscall.Stat_t)
			if st.Uid != want || st.Gid != want {
				t.Errorf("%s/%s owner %d:%d want %d", d, name, st.Uid, st.Gid, want)
			}
		}
	}
}

func TestDelegateRollbackAfterMove(t *testing.T) {
	root := withChildren(t, fakeCgroup(t, "cpu memory pids"))
	oldMove, oldRemove, oldGive, oldWrite := moveCgroupProcs, removeCgroup, giveCgroup, writeCgroupControl
	t.Cleanup(func() {
		moveCgroupProcs, removeCgroup, giveCgroup, writeCgroupControl = oldMove, oldRemove, oldGive, oldWrite
	})
	type group struct {
		controllers map[string]bool
		pids        []int
	}
	groups := map[string]*group{}
	for _, d := range []string{"", shimCgroup, workloadCgroup, mainCgroup} {
		groups[filepath.Join(root, d)] = &group{controllers: map[string]bool{}}
	}
	groups[root].pids = []int{1, 2}
	var operations []string
	writeCgroupControl = func(path string, data []byte, mode os.FileMode) error {
		dir := filepath.Dir(path)
		g := groups[dir]
		for _, token := range strings.Fields(string(data)) {
			controller := token[1:]
			if token[0] == '+' {
				if len(g.pids) != 0 || dir != root && !groups[filepath.Dir(dir)].controllers[controller] {
					t.Errorf("cannot enable %s in populated group or without parent controller: %s", controller, dir)
					return syscall.EBUSY
				}
			} else {
				for child, state := range groups {
					if strings.HasPrefix(child, dir+"/") && state.controllers[controller] {
						t.Errorf("cannot disable %s while child %s has it enabled", controller, child)
						return syscall.EBUSY
					}
				}
			}
		}
		for _, token := range strings.Fields(string(data)) {
			if token[0] == '+' {
				g.controllers[token[1:]] = true
			} else {
				delete(g.controllers, token[1:])
			}
		}
		operations = append(operations, filepath.Base(dir)+":"+string(data[:1]))
		return nil
	}
	moveCgroupProcs = func(from, to string) error {
		if len(groups[to].controllers) != 0 {
			t.Errorf("cannot move processes into internal group %s", to)
			return syscall.EBUSY
		}
		groups[to].pids = append(groups[to].pids, groups[from].pids...)
		groups[from].pids = nil
		operations = append(operations, filepath.Base(from)+">"+filepath.Base(to))
		return nil
	}
	giveCgroup = func(string, int, int) error {
		for _, d := range []string{root, filepath.Join(root, workloadCgroup)} {
			if len(groups[d].controllers) != 3 {
				t.Errorf("late failure before controllers enabled: %s", d)
			}
		}
		if len(groups[filepath.Join(root, shimCgroup)].pids) != 2 {
			t.Error("late failure before processes moved")
		}
		return syscall.EPERM
	}
	removeCgroup = func(path string) error {
		if len(groups[path].pids) != 0 {
			t.Errorf("cannot remove populated group %s", path)
			return syscall.EBUSY
		}
		for child := range groups {
			if filepath.Dir(child) == path {
				t.Errorf("cannot remove group %s with child %s", path, child)
				return syscall.ENOTEMPTY
			}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		// Kernel control files disappear with rmdir; ordinary fixture files do not.
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		delete(groups, path)
		operations = append(operations, "remove:"+strings.TrimPrefix(path, root+"/"))
		return nil
	}
	fd, err := delegateCgroup(root, 1234, 1234, func(int) error { return nil })
	if fd != -1 || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("fd %d err %v", fd, err)
	}
	want := []string{filepath.Base(root) + ">lux.shim", filepath.Base(root) + ":+", "workload:+",
		"workload:-", filepath.Base(root) + ":-", "lux.shim>" + filepath.Base(root),
		"remove:workload/main", "remove:workload", "remove:lux.shim"}
	if !slices.Equal(operations, want) {
		t.Errorf("operations %v, want %v", operations, want)
	}
	if len(groups) != 1 || len(groups[root].controllers) != 0 || !slices.Equal(groups[root].pids, []int{1, 2}) {
		t.Fatalf("rollback state: %v, root %+v", groups, groups[root])
	}
	for _, d := range []string{workloadCgroup, shimCgroup} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Fatalf("left %s: %v", d, err)
		}
	}
}

func TestPlacementMemoryOOM(t *testing.T) {
	root := withChildren(t, fakeCgroup(t, "cpu memory pids"))
	for d, count := range map[string]string{"": "2", mainCgroup: "3"} {
		if err := os.WriteFile(filepath.Join(root, d, "memory.events.local"), []byte("oom "+count+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Shim{}
	if n := s.placementMemoryOOM(root); n == nil || *n != 2 {
		t.Fatal(n)
	}
	f, err := os.Open(filepath.Join(root, mainCgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s.workCgroup = f
	if n := s.placementMemoryOOM(root); n == nil || *n != 5 {
		t.Fatal(n)
	}
}

func TestSizeEnvironmentSecretOverride(t *testing.T) {
	s := &Shim{user: &userInfo{home: "/"}, cfg: proto.ShimConfig{
		Env:     map[string]string{"LUX_CPUS": "2"},
		Secrets: []spec.Secret{{Name: "LUX_CPUS", As: "env"}},
	}}
	var got string
	for _, kv := range s.environment(map[string]string{"LUX_CPUS": "8"}) {
		if v, ok := strings.CutPrefix(kv, "LUX_CPUS="); ok {
			got = v
		}
	}
	if got != "8" {
		t.Fatalf("secret override %q", got)
	}
}

func TestMemoryOOM(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "memory.events.local")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if n := memoryOOM(write("low 0\nhigh 0\nmax 6854\noom 1\noom_kill 0\noom_group_kill 0\n")); n == nil || *n != 1 {
		t.Fatalf("got %v", n)
	}
	if n := memoryOOM(write("low 0\nhigh 0\nmax 19\noom 0\noom_kill 0\n")); n == nil || *n != 0 {
		t.Fatalf("got %v", n)
	}
	// oom_kill is not oom.
	if n := memoryOOM(write("oom_kill 3\n")); n != nil {
		t.Fatalf("no oom key: got %d", *n)
	}
	if n := memoryOOM(filepath.Join(dir, "missing")); n != nil {
		t.Fatalf("missing file: got %d", *n)
	}
}
