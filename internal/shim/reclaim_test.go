package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// A reclamation signals SIGTERM to every group at once and schedules
// SIGKILL exactly at the deadline, not at the workload's grace.
func TestReclaimSignalsTermNowKillAtDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	var sent []string
	var delay time.Duration
	var fire func()
	kill := func(pid int, sig syscall.Signal) { sent = append(sent, sig.String()) }
	after := func(d time.Duration, f func()) *time.Timer { delay, fire = d, f; return nil }
	reclaimSignals([]int{10, 20}, now.Add(15*time.Second), now, kill, after)
	if len(sent) != 2 || sent[0] != syscall.SIGTERM.String() || delay != 15*time.Second {
		t.Fatalf("sent %v, kill in %v", sent, delay)
	}
	fire()
	if len(sent) != 4 || sent[3] != syscall.SIGKILL.String() {
		t.Fatalf("after deadline %v", sent)
	}
	reclaimSignals(nil, now, now.Add(time.Minute), kill, after)
	if delay != 0 {
		t.Fatalf("past deadline delayed %v", delay)
	}
}

// Real processes: a workload ignoring SIGTERM, and its nested child, are
// alive after the SIGTERM and both gone once the deadline's SIGKILL comes;
// the deadline is short here, not 15s.
func TestReclaimKillsNestedProcessGroup(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", "-c", `trap '' TERM; sh -c 'trap "" TERM; touch "$0"; exec sleep 60' "$1" & wait`, "sh", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pg := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pg, syscall.SIGKILL) })
	waitFor(t, func() bool { _, err := os.Stat(ready); return err == nil }, "the nested process never installed its trap")
	deadline := time.Now().Add(300 * time.Millisecond)
	reclaimSignals([]int{pg}, deadline, time.Now(), func(pid int, sig syscall.Signal) { _ = syscall.Kill(-pid, sig) }, time.AfterFunc)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	time.Sleep(150 * time.Millisecond)
	if err := syscall.Kill(-pg, 0); err != nil {
		t.Fatalf("the group died on SIGTERM (%v): the deadline's kill was not exercised", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workload survived the deadline")
	}
	if time.Now().Before(deadline) {
		t.Fatal("the workload died before the deadline")
	}
	waitFor(t, func() bool { return errors.Is(syscall.Kill(-pg, 0), syscall.ESRCH) }, "a process of the group survived the deadline")
}

// reclaimShim is a Shim serving its runner socket at a temporary path,
// with a beforeStop hook that marks that it ran and then sleeps 10s, and a
// 60s workload grace: neither may delay a reclamation.
func reclaimShim(t *testing.T) (s *Shim, conn net.Conn, dir string) {
	t.Helper()
	// Not t.TempDir: it embeds the test name, and shim.sock must fit a
	// unix socket path (108 bytes on Linux, 104 on macOS).
	dir, err := os.MkdirTemp("", "lux-sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	o, err := OpenOutput(filepath.Join(dir, "out.jsonl"), NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	s = &Shim{out: o, red: NewRedactor(nil), user: &userInfo{home: dir}, env: []string{"PATH=" + os.Getenv("PATH")},
		startCh: make(chan proto.ShimMsg, 1), streams: map[int]chan syscall.WaitStatus{}, groups: map[int]bool{},
		exitCh: make(chan syscall.WaitStatus, 1), initCh: make(chan syscall.WaitStatus, 1),
		srv: servers{procs: map[string]*serverProc{}, started: map[string]int64{}, stopping: map[string]*serverProc{}}}
	s.cfg.Workdir = dir
	s.cfg.GraceSec = 60
	s.cfg.BeforeStopTimeoutSec = 30
	s.cfg.BeforeStop = []string{"sh", "-c", "touch " + filepath.Join(dir, "hook-ran") + "; sleep 10"}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go s.reap(done)
	ln, err := net.Listen("unix", filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.serve(ln)
	conn, err = net.Dial("unix", filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
		}
	}()
	return s, conn, dir
}

func sendShim(t *testing.T, c net.Conn, m proto.ShimMsg) {
	t.Helper()
	if err := json.NewEncoder(c).Encode(m); err != nil {
		t.Fatal(err)
	}
}

// startTrapping starts, as the shim's workload, a shell that records each
// SIGTERM in $dir/term and survives it, with a nested child ignoring it.
func startTrapping(t *testing.T, s *Shim, dir string) int {
	t.Helper()
	script := `trap 'echo TERM >> "$0/term"' TERM; sh -c 'trap "" TERM; touch "$0/nested"; exec sleep 60' "$0" & touch "$0/ready"; while :; do sleep 0.05; done`
	proc, err := s.startWorkload([]string{"sh", "-c", script, dir}, s.env)
	if err != nil {
		t.Fatal(err)
	}
	pg := proc.Cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pg, syscall.SIGKILL) })
	waitFor(t, func() bool {
		_, a := os.Stat(filepath.Join(dir, "ready"))
		_, b := os.Stat(filepath.Join(dir, "nested"))
		return a == nil && b == nil
	}, "the workload never got ready")
	return pg
}

// A ShimStop carrying KillAtUnixMs, over the runner's socket: the workload
// gets SIGTERM at once, the beforeStop hook never runs, and its process
// group (the nested child too) is gone at the deadline, not after the
// hook or the 60s grace. A repeated notice with a later deadline does not
// move the kill.
func TestShimReclaimSkipsHookAndGrace(t *testing.T) {
	s, conn, dir := reclaimShim(t)
	pg := startTrapping(t, s, dir)
	notice := time.Now()
	// The deadline as sent: whole milliseconds.
	killAt := time.UnixMilli(notice.Add(300 * time.Millisecond).UnixMilli())
	sendShim(t, conn, proto.ShimMsg{Type: proto.ShimStop, Reason: "spot-reclaim", KillAtUnixMs: killAt.UnixMilli()})
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "term"))
		return strings.Contains(string(b), "TERM")
	}, "no SIGTERM")
	if took := time.Since(notice); took > 250*time.Millisecond {
		t.Fatalf("SIGTERM %v after the notice, want at once", took)
	}
	sendShim(t, conn, proto.ShimMsg{Type: proto.ShimStop, Reason: "spot-reclaim", KillAtUnixMs: time.Now().Add(5 * time.Second).UnixMilli()})
	if err := syscall.Kill(-pg, 0); err != nil {
		t.Fatalf("the workload died on SIGTERM (%v): the deadline's kill was not exercised", err)
	}
	select {
	case <-s.exitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the workload outlived the first deadline")
	}
	if dead := time.Now(); dead.Before(killAt) || dead.After(killAt.Add(time.Second)) {
		t.Fatalf("workload died at %v, want at the first deadline %v", dead.Sub(notice), killAt.Sub(notice))
	}
	waitFor(t, func() bool { return errors.Is(syscall.Kill(-pg, 0), syscall.ESRCH) }, "the nested child outlived the deadline")
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "hook-ran")); err == nil {
		t.Fatal("the beforeStop hook ran on a reclamation")
	}
	if !s.isStopping() || s.stopWhy != "spot-reclaim" {
		t.Fatalf("stopping %v, why %q", s.isStopping(), s.stopWhy)
	}
}

// A reclamation before the workload has started: the wait for the start
// is released, and no workload is started after it, with or without a
// terminal.
func TestShimReclaimBeforeWorkloadStart(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			s, conn, dir := reclaimShim(t)
			s.cfg.TTY = tty
			sendShim(t, conn, proto.ShimMsg{Type: proto.ShimStop, Reason: "spot-reclaim", KillAtUnixMs: time.Now().Add(300 * time.Millisecond).UnixMilli()})
			select {
			case <-s.startCh:
			case <-time.After(3 * time.Second):
				t.Fatal("the wait for the start was not released")
			}
			marker := filepath.Join(dir, "started")
			proc, err := s.startWorkload([]string{"sh", "-c", "touch " + marker + "; sleep 60"}, s.env)
			if proc != nil {
				t.Cleanup(func() { _ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL) })
			}
			if !errors.Is(err, errReclaimed) || proc != nil {
				t.Fatalf("workload start after a reclamation: %v, %v", proc, err)
			}
			time.Sleep(200 * time.Millisecond)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the workload started after a reclamation")
			}
		})
	}
}

// A reclamation while an ordinary stop's beforeStop hook runs: the hook's
// process group, which ignores SIGTERM and would sleep 10s within a 30s
// timeout, is killed at the reclamation's deadline.
func TestShimReclaimKillsRunningBeforeStopHook(t *testing.T) {
	s, conn, dir := reclaimShim(t)
	s.cfg.BeforeStop = []string{"sh", "-c", "trap '' TERM; touch " + filepath.Join(dir, "hook-ran") + "; sleep 10"}
	startTrapping(t, s, dir)
	sendShim(t, conn, proto.ShimMsg{Type: proto.ShimStop, Reason: "stop", GraceSec: 60})
	quick := func(cond func() bool, what string) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	quick(func() bool { _, err := os.Stat(filepath.Join(dir, "hook-ran")); return err == nil }, "the hook never started")
	var hook int
	quick(func() bool { s.mu.Lock(); hook = s.hookPgid; s.mu.Unlock(); return hook > 0 }, "no hook group")
	t.Cleanup(func() { _ = syscall.Kill(-hook, syscall.SIGKILL) })
	killAt := time.UnixMilli(time.Now().Add(300 * time.Millisecond).UnixMilli())
	sendShim(t, conn, proto.ShimMsg{Type: proto.ShimStop, Reason: "spot-reclaim", KillAtUnixMs: killAt.UnixMilli()})
	quick(func() bool { return errors.Is(syscall.Kill(-hook, 0), syscall.ESRCH) }, "the hook's group outlived the reclamation's deadline")
	if now := time.Now(); now.After(killAt.Add(2 * time.Second)) {
		t.Fatalf("the hook's group died %v after the deadline", now.Sub(killAt))
	}
}

// A reclamation skips the stop-time git checkpoint: staging may take far
// longer than the reclamation's deadline. It is reported as skipped.
func TestShimReclaimSkipsCheckpointStaging(t *testing.T) {
	s, _, dir := reclaimShim(t)
	stage := filepath.Join(dir, "checkpoints-1")
	s.cfg.Checkpoint = &proto.CheckpointArgs{Dir: stage, Repos: []proto.CheckpointRepo{{Name: "api", Path: dir}}}
	s.reclaim(time.Now().Add(300 * time.Millisecond))
	s.stageCheckpoints()
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("staged on a reclamation: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out.jsonl"))
	if !strings.Contains(string(b), "git checkpoint: skipped on a spot reclamation") {
		t.Fatalf("output %s", b)
	}
}

// A reclamation that lands after an ordinary stop registered its hook (its
// 30s deadline) but before the hook started: the hook never starts.
func TestShimReclaimBeforeHookStarts(t *testing.T) {
	s, _, dir := reclaimShim(t)
	s.mu.Lock()
	s.stopping, s.stopWhy = true, "stop"
	s.hookBy, s.hookMoved = time.Now().Add(30*time.Second), make(chan struct{}, 1)
	s.mu.Unlock()
	s.reclaim(time.Now().Add(300 * time.Millisecond))
	done := make(chan struct{})
	go func() { defer close(done); s.beforeStop(s.env) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the hook ran after a reclamation")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "hook-ran")); err == nil {
		t.Fatal("the hook started after a reclamation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hookPgid != 0 {
		t.Fatalf("hook group %d published after a reclamation", s.hookPgid)
	}
}
