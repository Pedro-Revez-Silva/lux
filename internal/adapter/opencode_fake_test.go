package adapter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// runFakeOpenCode runs `lux-fake acp` under the OpenCode adapter (with
// OpenCode's server, --port), its first prompt prompt, until the sink has
// want; then stops it.
func runFakeOpenCode(t *testing.T, prompt, want string) *childSink {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "lux-fake")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/marcioapm/lux/cmd/lux-fake").CombinedOutput(); err != nil {
		t.Fatalf("build lux-fake: %v\n%s", err, out)
	}
	a := NewOpenCode()
	cfg := proto.ShimConfig{Command: []string{bin, "acp"}, Workdir: t.TempDir(), Prompt: prompt}
	argv, err := a.Command(cfg)
	if err != nil || a.bus == nil {
		t.Fatalf("command %q: %v", argv, err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sink := &childSink{inputSink: &inputSink{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(t.Context(), &Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, cfg, sink)
	}()
	for end := time.Now().Add(fakeWait); !strings.Contains(strings.Join(sink.records(), "\n"), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("no %q in records:\n%s", want, strings.Join(sink.records(), "\n"))
		}
	}
	_ = a.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not return")
	}
	_ = cmd.Wait()
	return sink
}

// fakeTaskDone ends lux-fake's task call's opencode.task.
const fakeTaskDone = `"providerID":"lux-fake"},"status":"completed"}`

// kinds is each record's type and status (or answered), in order.
func kinds(recs []string) []string {
	var out []string
	for _, r := range recs {
		typ, d, _ := strings.Cut(r, " ")
		var m map[string]any
		_ = json.Unmarshal([]byte(d), &m)
		k := typ
		for _, f := range []string{"tool", "status", "answered"} {
			if s, _ := m[f].(string); s != "" {
				k += " " + s
			}
		}
		out = append(out, k)
	}
	return out
}

// lux-fake's subagent (cmd/lux-fake/subagent.go), as dude's tests drive
// it: a task call running a child that reads a file and runs bash.
func TestLuxFakeOpenCodeSubagent(t *testing.T) {
	sink := runFakeOpenCode(t, "subagent", fakeTaskDone)
	wantRecords(t, kinds(sink.records()), []string{
		"opencode.session",
		"opencode.task running",
		"opencode.part read running",
		"opencode.part read completed",
		"opencode.part bash running",
		"opencode.part bash completed",
		"opencode.message",
		"opencode.task completed",
	})
}

// With `subagent ask`, the child's bash call asks a permission that only
// lux answers: allowed over HTTP, the call completes, and the warning
// says so. OpenCode goes on as it takes the answer, so the record and its
// warning, together, come after the ask (bash running) and possibly after
// what the child did next.
func TestLuxFakeOpenCodeSubagentAsks(t *testing.T) {
	sink := runFakeOpenCode(t, "subagent ask", fakeTaskDone)
	recs := sink.records()
	got := kinds(recs)
	at := slices.Index(got, "opencode.permission allow")
	if at < 0 || at+1 >= len(got) || got[at+1] != "lux.warning" || at < slices.Index(got, "opencode.part bash running") {
		t.Fatalf("records:\n%s", strings.Join(recs, "\n"))
	}
	if !strings.Contains(recs[at+1], "asked for bash on cat /etc/hostname; lux allowed it") {
		t.Fatalf("warning: %s", recs[at+1])
	}
	wantRecords(t, slices.Delete(got, at, at+2), []string{
		"opencode.session",
		"opencode.task running",
		"opencode.part read running",
		"opencode.part read completed",
		"opencode.part bash running",
		"opencode.part bash completed",
		"opencode.message",
		"opencode.task completed",
	})
}
