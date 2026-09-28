package shim

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// A server's output goes to the output file as ch=server records naming
// it and its stream, apart from the workload's; its start and exit are
// lux.server records there too, the exit with its code and last stderr
// line.
func TestServerOutputRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	o, err := OpenOutput(path, NewRedactor(map[string]string{"K": "supersecretvalue"}))
	if err != nil {
		t.Fatal(err)
	}
	o.Write("stdout", []byte("workload line\n"))
	o.WriteServer("web", "stdout", []byte("listening on "))
	o.WriteServer("web", "stderr", []byte("warn: supersecretvalue\n"))
	o.WriteServer("web", "stdout", []byte(":3000\n"))
	o.ServerEvent("web", map[string]any{"phase": "exit", "gen": 1, "exitCode": 2})
	o.Close()
	recs := readRecords(t, path)
	var got []string
	for _, r := range recs {
		got = append(got, r.Ch+"/"+r.Server+"/"+r.Stream+":"+strings.TrimSpace(r.Data)+string(r.Event))
	}
	want := []string{
		"stdout//:workload line",
		"server/web/stderr:warn: [REDACTED:K]",
		"server/web/stdout:listening on :3000",
		`server/web/:{"data":{"exitCode":2,"gen":1,"phase":"exit"},"type":"lux.server"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLastLine(t *testing.T) {
	var l lastLine
	l.Write([]byte("first\nsecond li"))
	l.Write([]byte("ne\n\n  \n"))
	if l.String() != "second line" {
		t.Fatalf("got %q", l.String())
	}
	l.Write([]byte("partial"))
	if l.String() != "partial" {
		t.Fatalf("got %q", l.String())
	}
}

// Servers run as the runner's set says: started once per gen, stopped when
// they leave the set (or change gen), and an exit is recorded with its
// code and last stderr line, and is not restarted for the same gen.
func TestServerProcesses(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutput(filepath.Join(dir, "out.jsonl"), NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := &Shim{out: o, red: NewRedactor(nil), user: &userInfo{home: dir}, env: []string{"PATH=" + os.Getenv("PATH")},
		streams: map[int]chan syscall.WaitStatus{}, groups: map[int]bool{}, srv: servers{procs: map[string]*serverProc{}, started: map[string]int64{}, stopping: map[string]*serverProc{}}}
	s.cfg.Workdir = dir
	go s.reap()

	marker := filepath.Join(dir, "ran")
	s.setServers([]proto.ServerSpec{
		{Name: "loop", Gen: 1, Command: []string{"sh", "-c", "echo up; echo $X > " + marker + "; exec sleep 60"}, Env: map[string]string{"X": "from-env"}},
		{Name: "fails", Gen: 1, Command: []string{"sh", "-c", "echo bad thing >&2; exit 3"}},
		{Name: "portonly", Gen: 1},
	})
	s.serversReady()
	waitFor(t, func() bool { b, _ := os.ReadFile(marker); return strings.TrimSpace(string(b)) == "from-env" }, "loop never ran with its env")
	waitFor(t, func() bool { return hasRecord(t, o, "fails", `"exitCode":3`) && hasRecord(t, o, "fails", `bad thing`) }, "no exit record for fails")
	s.srv.mu.Lock()
	loop := s.srv.procs["loop"]
	_, portOnly := s.srv.procs["portonly"]
	s.srv.mu.Unlock()
	if loop == nil || portOnly {
		t.Fatalf("procs: loop %v, portonly %v", loop, portOnly)
	}
	// The same set again: nothing restarts (fails stays exited).
	s.setServers([]proto.ServerSpec{
		{Name: "loop", Gen: 1, Command: []string{"sh", "-c", "exec sleep 60"}},
		{Name: "fails", Gen: 1, Command: []string{"sh", "-c", "exit 3"}},
	})
	time.Sleep(300 * time.Millisecond)
	s.srv.mu.Lock()
	same, failsAgain := s.srv.procs["loop"] == loop, s.srv.procs["fails"] != nil
	s.srv.mu.Unlock()
	if !same || failsAgain || strings.Count(readFile(t, filepath.Join(dir, "out.jsonl")), `"phase":"start"`) != 2 {
		t.Fatalf("restarted on the same set: same %v, fails %v", same, failsAgain)
	}
	// A new gen: the old process goes first, then the new one starts.
	s.setServers([]proto.ServerSpec{{Name: "loop", Gen: 2, Command: []string{"sh", "-c", "exec sleep 60"}}})
	select {
	case <-loop.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the old gen was not stopped")
	}
	waitFor(t, func() bool {
		s.srv.mu.Lock()
		defer s.srv.mu.Unlock()
		return s.srv.procs["loop"] != nil && s.srv.procs["loop"].gen == 2
	}, "the new gen never started")
	s.srv.mu.Lock()
	loop = s.srv.procs["loop"]
	s.srv.mu.Unlock()
	// Out of the set: stopped.
	s.setServers(nil)
	select {
	case <-loop.done:
	case <-time.After(10 * time.Second):
		t.Fatal("loop was not stopped")
	}

	// A server's group goes with its leader, killed by the reaper before
	// the leader is reaped; after, the shim no longer signals that pgid (it
	// may be another's by then), even when a stop's KILL comes due. (In
	// this test, not its own: one process has one reaper.)
	pidFile := filepath.Join(dir, "child")
	s.setServers([]proto.ServerSpec{{Name: "web", Gen: 1, Command: []string{"sh", "-c", "sleep 60 & echo $! > " + pidFile + "; sleep 0.3"}}})
	var web *serverProc
	waitFor(t, func() bool { s.srv.mu.Lock(); defer s.srv.mu.Unlock(); web = s.srv.procs["web"]; return web != nil }, "web never started")
	select {
	case <-web.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader's exit was not seen")
	}
	child, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return syscall.Kill(child, 0) != nil }, "the group outlived its leader")
	s.mu.Lock()
	tracked := s.groups[web.pgid]
	s.mu.Unlock()
	if tracked {
		t.Fatal("a reaped leader's group is still signalled")
	}
	o.Close()
}

func waitFor(t *testing.T, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasRecord(t *testing.T, o *Output, server, text string) bool {
	o.mu.Lock()
	o.w.Flush()
	name := o.f.Name()
	o.mu.Unlock()
	for _, line := range strings.Split(readFile(t, name), "\n") {
		if strings.Contains(line, `"server":"`+server+`"`) && strings.Contains(line, text) {
			return true
		}
	}
	return false
}
