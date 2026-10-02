package adapter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// eventSink is inputSink keeping each event as JSON too.
type eventSink struct {
	inputSink
	evMu   sync.Mutex
	events []string
}

func (s *eventSink) Event(typ string, v any) {
	b, _ := json.Marshal(v)
	s.evMu.Lock()
	s.events = append(s.events, typ+" "+string(b))
	s.evMu.Unlock()
	s.inputSink.Event(typ, v)
}

func (s *eventSink) all() string {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return strings.Join(s.events, "\n")
}

// fakeWait bounds how long runFake waits for its event.
const fakeWait = 10 * time.Second

// runFake runs lux-fake (ACP) under the ACP adapter with cfg, waits for
// the sink to have want, and stops it.
func runFake(t *testing.T, bin, home string, cfg proto.ShimConfig, want string) (*eventSink, string) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a, sink := NewACP(), &eventSink{}
	var session string
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(t.Context(), &Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, cfg, sink)
	}()
	for end := time.Now().Add(fakeWait); !strings.Contains(sink.all(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("no %q in events:\n%s", want, sink.all())
		}
	}
	a.mu.Lock()
	session = a.session
	a.mu.Unlock()
	_ = a.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not return")
	}
	_ = cmd.Wait()
	return sink, session
}

// An agent that replays a user turn's image on session/load (as OpenCode
// may): the replayed update is recorded with the image's size and sha256,
// never its bytes.
func TestACPLoadReplayDropsImageData(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lux-fake")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/marcioapm/lux/cmd/lux-fake").CombinedOutput(); err != nil {
		t.Fatalf("build lux-fake: %v\n%s", err, out)
	}
	home := t.TempDir()
	im := spec.Attachment{Name: "a.png", ContentType: "image/png", Data: imgData}
	_, session := runFake(t, bin, home, proto.ShimConfig{Prompt: "look", PromptAttachments: []spec.Attachment{im}}, "acp.turn_end")
	if session == "" {
		t.Fatal("no session")
	}
	sink, _ := runFake(t, bin, home, proto.ShimConfig{Resume: true, SessionID: session}, `"sessionUpdate":"user_message_chunk"`)
	events := sink.all()
	if !strings.Contains(events, `"type":"image"`) {
		t.Fatalf("lux-fake replayed no image:\n%s", events)
	}
	if strings.Contains(events, imgData) {
		t.Fatalf("a replayed image's bytes are in the events:\n%s", events)
	}
	if !strings.Contains(events, `"sha256":"`) || !strings.Contains(events, `"size":11`) {
		t.Fatalf("replayed image without its metadata:\n%s", events)
	}
}
