package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/adapter"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// A second placement restores the snapshot through prepareVolumes and passes
// the runner's shim config to each adapter through lux-fake, which must consume
// requestId=prompt even for an image-only workload. The shim's config gate is
// covered separately by TestAdapterConfigSnapshotWithoutSession.
func TestSnapshotOnlySecondPlacementStartsAgentPrompt(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lux-fake")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/marcioapm/lux/cmd/lux-fake").CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v\n%s", err, out)
	}
	for _, name := range []string{"acp", "opencode", "claude-code", "codex"} {
		for _, images := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/images=%v", name, images), func(t *testing.T) {
				f := newResumeFixture(t, 2, "")
				f.sp.Workload = spec.Workload{Adapter: name, Command: []string{bin}, Prompt: "echo INITIAL-PROMPT"}
				if images {
					f.sp.Workload.Prompt = ""
					f.a.PromptAttachments = []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: "iVBORw0KGgo="}}
				}
				if err := f.start(t.Context()); err != nil {
					t.Fatal(err)
				}
				f.waitEvent(t, "volumes.restored")
				if b, err := os.ReadFile(f.bin + ".vol." + volumeName("run1", "data")); err != nil || string(b) != "state" {
					t.Fatalf("restored data %q: %v", b, err)
				}
				if err := f.p.writeShimConfig(t.Context(), f.sp, f.a.PromptAttachments); err != nil {
					t.Fatal(err)
				}
				rt, _ := f.r.mountpoint(t.Context(), runtimeVolume("run1"))
				b, err := os.ReadFile(filepath.Join(rt, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				var cfg proto.ShimConfig
				if err := json.Unmarshal(b, &cfg); err != nil {
					t.Fatal(err)
				}
				if !cfg.Resume || cfg.SessionID != "" || cfg.Epoch != 2 {
					t.Fatalf("second placement config: %+v", cfg)
				}
				ad, _ := adapter.New(name)
				argv, err := ad.Command(cfg)
				if err != nil {
					t.Fatal(err)
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
				sink := &promptSink{consumed: make(chan string, 8)}
				done := make(chan struct{})
				go func() {
					defer close(done)
					_ = ad.Run(t.Context(), &adapter.Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, cfg, sink)
				}()
				t.Cleanup(func() {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					_ = cmd.Wait()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("adapter did not stop")
					}
				})
				select {
				case id := <-sink.consumed:
					if id != "prompt" {
						t.Fatalf("consumed %q, want prompt", id)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("second placement idled without consuming its prompt: %s", sink.output())
				}
			})
		}
	}
}

type promptSink struct {
	adapter.Sink
	mu       sync.Mutex
	log      strings.Builder
	consumed chan string
}

func (s *promptSink) output() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.log.String() }
func (s *promptSink) Stdout(b []byte) { s.mu.Lock(); defer s.mu.Unlock(); s.log.Write(b) }
func (s *promptSink) Stderr(b []byte) { s.Stdout(b) }
func (s *promptSink) EndMessage()     {}
func (s *promptSink) Event(typ string, v any) {
	b, _ := json.Marshal(v)
	s.Stdout(append([]byte(typ+" "), b...))
}
func (s *promptSink) Stream(typ, key, text string, wrap func(string) any) { s.Stdout([]byte(text)) }
func (s *promptSink) Session(string)                                      {}
func (s *promptSink) Activity(bool)                                       {}
func (s *promptSink) InputAccepted(in proto.Input, d adapter.Delivery) {
	if !d.Receipt {
		s.consumed <- in.RequestID
	}
}
func (s *promptSink) InputConsumed(id string)                  { s.consumed <- id }
func (s *promptSink) InputFailed(in proto.Input, err error)    { s.Stdout([]byte(err.Error())) }
func (s *promptSink) Compacted(proto.Compaction)               {}
func (s *promptSink) Clip(text string, max int) (string, bool) { return text, false }

// The prompt's image bytes reach the shim config from the Assign's own
// field on a first placement, and never on a resume; the spec stays
// metadata only.
func TestShimConfigPromptAttachments(t *testing.T) {
	sp := spec.RunSpec{Workload: spec.Workload{Adapter: "claude-code", Prompt: "look",
		Attachments: []spec.Attachment{{Name: "a.png", ContentType: "image/png"}}}}
	prompt := []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: "iVBORw0KGgo="}}
	for _, c := range []struct {
		name   string
		resume *proto.ResumeInfo
		want   []spec.Attachment
	}{
		{"first", nil, prompt},
		{"snapshot-only", &proto.ResumeInfo{Snapshot: &proto.Manifest{SnapshotID: "snap1"}}, prompt},
		{"resume", &proto.ResumeInfo{SessionID: "s"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := t.TempDir()
			r := &Runner{}
			r.mounts.Store(runtimeVolume("run_x"), rt)
			p := &placement{r: r, runID: "run_x", epoch: 1, assign: &proto.Assign{Resume: c.resume}, state: &runState{}}
			if err := p.writeShimConfig(context.Background(), sp, prompt); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(rt, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg proto.ShimConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.PromptAttachments) != len(c.want) || (len(c.want) == 1 && cfg.PromptAttachments[0] != c.want[0]) {
				t.Fatalf("promptAttachments %+v, want %+v", cfg.PromptAttachments, c.want)
			}
		})
	}
}
