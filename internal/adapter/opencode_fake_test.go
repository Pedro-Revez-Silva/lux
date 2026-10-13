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
// OpenCode's server, --port), its first prompt prompt, until done holds
// for the sink's records; then stops it.
func runFakeOpenCode(t *testing.T, prompt string, done func(recs []string) bool) *childSink {
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
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = a.Run(t.Context(), &Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, cfg, sink)
	}()
	for end := time.Now().Add(fakeWait); !done(sink.records()); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("records:\n%s", strings.Join(sink.records(), "\n"))
		}
	}
	_ = a.Stop()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not return")
	}
	_ = cmd.Wait()
	return sink
}

// fakeRecords are lux-fake's subagent records with what varies between
// Runs replaced: the parent session's id by $P, the child message's by $M
// (each taken from where it first appears, so every other record must
// carry the same), and every time, once checked set and in order, by 0.
func fakeRecords(t *testing.T, recs []string) []string {
	t.Helper()
	var parent, msg string
	var out []string
	var last int64
	ordered := func(ts ...int64) {
		for _, v := range ts {
			if v < last || v == 0 {
				t.Fatalf("time %d after %d in:\n%s", v, last, strings.Join(recs, "\n"))
			}
			last = v
		}
	}
	for _, r := range recs {
		typ, d, _ := strings.Cut(r, " ")
		var v any
		switch typ {
		case proto.EvOpenCodeSession:
			var x proto.OpenCodeSession
			_ = json.Unmarshal([]byte(d), &x)
			parent = x.ParentID
			last = x.CreatedAt
			ordered(x.CreatedAt)
			x.CreatedAt = 0
			v = x
		case proto.EvOpenCodePart:
			var x proto.OpenCodePart
			_ = json.Unmarshal([]byte(d), &x)
			if msg == "" {
				msg = x.MessageID
			}
			ordered(x.Time.Start)
			if x.Time.End != 0 {
				ordered(x.Time.End)
			}
			x.Time = proto.OpenCodeTime{}
			v = x
		case proto.EvOpenCodeMessage:
			var x proto.OpenCodeMessage
			_ = json.Unmarshal([]byte(d), &x)
			ordered(x.Completed)
			x.Completed = 0
			v = x
		default:
			out = append(out, r)
			continue
		}
		out = append(out, rec(typ, v))
	}
	if parent == "" || msg == "" {
		t.Fatalf("no session or part in:\n%s", strings.Join(recs, "\n"))
	}
	for i := range out {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(out[i], parent, "$P"), msg, "$M")
	}
	return out
}

// fakeSubagent is lux-fake's subagent as relayed, every field, its ids
// as cmd/lux-fake/subagent.go makes them for a Run's first subagent.
func fakeSubagent(bash string) []string {
	const child = "ses_$P_child1"
	model := json.RawMessage(`{"modelID":"lux-fake","providerID":"lux-fake"}`)
	readIn, bashIn := `{"filePath":"a.txt"}`, `{"command":"cat /etc/hostname","description":"Read the hostname"}`
	recs := []string{
		rec(proto.EvOpenCodeSession, proto.OpenCodeSession{ID: child, ParentID: "$P", Title: "fake subagent (@general subagent)"}),
		rec(proto.EvOpenCodeTask, proto.OpenCodeTask{SessionID: "$P", CallID: "toolu_task1", ChildSessionID: child, Model: model, Status: "running"}),
		rec(proto.EvOpenCodePart, proto.OpenCodePart{SessionID: child, MessageID: "$M", CallID: "toolu_read1", Tool: "read", Status: "running", Input: readIn}),
		rec(proto.EvOpenCodePart, proto.OpenCodePart{SessionID: child, MessageID: "$M", CallID: "toolu_read1", Tool: "read", Status: "completed", Title: "a.txt",
			Input: readIn, Output: "<path>a.txt</path>\n<content>\n1: hello\n</content>"}),
		rec(proto.EvOpenCodePart, proto.OpenCodePart{SessionID: child, MessageID: "$M", CallID: "toolu_bash1", Tool: "bash", Status: "running", Input: bashIn}),
		bash,
		rec(proto.EvOpenCodeMessage, proto.OpenCodeMessage{SessionID: child, MessageID: "$M", Role: "assistant", Text: "the child read a.txt: hello", Cost: 0.001,
			Tokens: proto.OpenCodeTokens{Input: 10, Output: 5, Reasoning: 0, CacheRead: 2, CacheWrite: 1}}),
		rec(proto.EvOpenCodeTask, proto.OpenCodeTask{SessionID: "$P", CallID: "toolu_task1", ChildSessionID: child, Model: model, Status: "completed"}),
	}
	return recs
}

var fakeBashDone = rec(proto.EvOpenCodePart, proto.OpenCodePart{SessionID: "ses_$P_child1", MessageID: "$M", CallID: "toolu_bash1", Tool: "bash", Status: "completed",
	Title: "Read the hostname", Input: `{"command":"cat /etc/hostname","description":"Read the hostname"}`, Output: "fakehost\n"})

// taskCompleted reports whether recs have the task call's completion.
func taskCompleted(recs []string) bool {
	return slices.ContainsFunc(recs, func(r string) bool {
		return strings.HasPrefix(r, proto.EvOpenCodeTask+" ") && strings.Contains(r, `"status":"completed"`)
	})
}

// lux-fake's subagent (cmd/lux-fake/subagent.go), as dude's tests drive
// it: a task call running a child that reads a file and runs bash.
func TestLuxFakeOpenCodeSubagent(t *testing.T) {
	sink := runFakeOpenCode(t, "subagent", taskCompleted)
	wantRecords(t, fakeRecords(t, sink.records()), fakeSubagent(fakeBashDone))
}

// With `subagent ask`, the child's bash call asks a permission that only
// lux answers: allowed over HTTP, the call completes, and the warning
// says so. OpenCode goes on as it takes the answer, so the record and its
// warning, together, come after the ask (bash running) and possibly after
// what the child did next.
func TestLuxFakeOpenCodeSubagentAsks(t *testing.T) {
	sink := runFakeOpenCode(t, "subagent ask", func(recs []string) bool {
		return taskCompleted(recs) && slices.ContainsFunc(recs, func(r string) bool { return strings.HasPrefix(r, proto.EvWarning+" ") })
	})
	got := fakeRecords(t, sink.records())
	pair := []string{
		rec(proto.EvOpenCodePermission, proto.OpenCodePermission{SessionID: "ses_$P_child1", PermissionID: "per_fake1", Permission: "bash",
			Patterns: []string{"cat /etc/hostname"}, CallID: "toolu_bash1", Answered: proto.PermissionAllowed}),
		`lux.warning {"message":"opencode: a subagent session ses_$P_child1 asked for bash on cat /etc/hostname; lux allowed it"}`,
	}
	want := fakeSubagent(fakeBashDone)
	at := slices.Index(got, pair[0])
	if at < 0 || at+1 >= len(got) || got[at+1] != pair[1] || at < slices.Index(got, want[4]) {
		t.Fatalf("no permission and warning together after bash running:\n%s", strings.Join(got, "\n"))
	}
	wantRecords(t, slices.Delete(slices.Clone(got), at, at+2), want)
}
