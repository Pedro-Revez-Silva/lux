package shim

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/adapter"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// recordingAdapter keeps what Deliver was given, and checks each image's
// file exists at the moment it is handed over.
type recordingAdapter struct {
	mu     sync.Mutex
	got    []proto.Input
	onDisk []bool
}

func (r *recordingAdapter) Command(proto.ShimConfig) ([]string, error) {
	return []string{"true"}, nil
}
func (r *recordingAdapter) Run(context.Context, *adapter.Process, proto.ShimConfig, adapter.Sink) error {
	return nil
}
func (r *recordingAdapter) Deliver(in proto.Input) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, in)
	for _, a := range in.Attachments {
		_, err := os.Stat(a.Path)
		r.onDisk = append(r.onDisk, a.Path != "" && err == nil)
	}
}
func (r *recordingAdapter) Interrupt() error { return nil }
func (r *recordingAdapter) Stop() error      { return nil }

func deliverShim(t *testing.T) (*Shim, *recordingAdapter, string, string) {
	t.Helper()
	s, home, out := inputsShim(t)
	ad := &recordingAdapter{}
	s.adapter, s.proc, s.delivered = ad, &adapter.Process{}, map[string]bool{}
	return s, ad, home, out
}

// deliver writes an input's images before the adapter has it, and hands
// the adapter their paths with the bytes; LUX_INPUTS names the directory.
func TestDeliverWritesInputs(t *testing.T) {
	s, ad, home, _ := deliverShim(t)
	s.deliver(proto.Input{RequestID: "r", Text: "see", Attachments: []spec.Attachment{pngAttachment("a.png")}})
	if len(ad.got) != 1 || len(ad.got[0].Attachments) != 1 {
		t.Fatalf("delivered %+v", ad.got)
	}
	a := ad.got[0].Attachments[0]
	if want := filepath.Join(home, ".lux-inputs", "r", "1-a.png"); a.Path != want || !ad.onDisk[0] {
		t.Fatalf("path %q (on disk before Deliver: %v), want %q", a.Path, ad.onDisk, want)
	}
	if b, _ := os.ReadFile(a.Path); !bytes.Equal(b, testPNG) || a.Data != pngAttachment("a.png").Data {
		t.Fatalf("file %q, data %q", b, a.Data)
	}
	if env := s.environment(nil); !slices.Contains(env, "LUX_INPUTS="+filepath.Join(home, ".lux-inputs")) {
		t.Fatalf("environment without LUX_INPUTS: %v", env)
	}
}

// An image that cannot be written still reaches the agent, inline, and a
// lux.warning names the input.
func TestDeliverInputsWriteFails(t *testing.T) {
	s, ad, home, out := deliverShim(t)
	// A file where the request's directory goes.
	if err := os.MkdirAll(filepath.Join(home, ".lux-inputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".lux-inputs", "r"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.deliver(proto.Input{RequestID: "r", Attachments: []spec.Attachment{pngAttachment("a.png")}})
	s.out.Close()
	if len(ad.got) != 1 || len(ad.got[0].Attachments) != 1 {
		t.Fatalf("delivered %+v", ad.got)
	}
	if a := ad.got[0].Attachments[0]; a.Path != "" || a.Data != pngAttachment("a.png").Data {
		t.Fatalf("attachment %+v, want inline data, no path", a)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), `"type":"lux.warning"`) || !strings.Contains(string(raw), "input r: writing its images to $LUX_INPUTS") {
		t.Fatalf("no warning: %s", raw)
	}
}

// The prompt's images go to the adapter's config with their paths; the
// shim keeps no copy of their bytes. A resume gives the adapter none.
func TestAdapterConfigReleasesPromptImages(t *testing.T) {
	s, _, home, _ := deliverShim(t)
	s.cfg.PromptAttachments = []spec.Attachment{pngAttachment("p.png")}
	cfg := s.adapterConfig()
	if s.cfg.PromptAttachments != nil {
		t.Fatalf("shim config still holds the prompt images: %+v", s.cfg.PromptAttachments)
	}
	if len(cfg.PromptAttachments) != 1 || cfg.PromptAttachments[0].Path != filepath.Join(home, ".lux-inputs", "prompt", "1-p.png") || cfg.PromptAttachments[0].Data == "" {
		t.Fatalf("adapter config %+v", cfg.PromptAttachments)
	}
	s.cfg.Resume, s.cfg.PromptAttachments = true, []spec.Attachment{pngAttachment("p.png")}
	if cfg := s.adapterConfig(); cfg.PromptAttachments != nil || s.cfg.PromptAttachments != nil {
		t.Fatalf("resume: adapter %+v, shim %+v", cfg.PromptAttachments, s.cfg.PromptAttachments)
	}
}

// envAdapter records the environment it is given and whether that came
// before Command.
type envAdapter struct {
	recordingAdapter
	env           []string
	envBeforeArgv bool
}

func (e *envAdapter) WorkloadEnv(env []string) { e.env = env }
func (e *envAdapter) Command(proto.ShimConfig) ([]string, error) {
	e.envBeforeArgv = e.env != nil
	return []string{"true"}, nil
}

// An adapter that reads the workload's environment is given it, with the
// Run's env secrets, before it builds the command; the workload starts
// with that same environment.
func TestWorkloadCommandGivesAdapterTheEnvironment(t *testing.T) {
	s, _, _ := inputsShim(t)
	s.cfg.Secrets = []spec.Secret{{Name: "OPENCODE_SERVER_PASSWORD", As: "env"}, {Name: "KEYFILE", As: "file", Path: "/k"}}
	env := s.environment(map[string]string{"OPENCODE_SERVER_PASSWORD": "pw-1", "KEYFILE": "k"})
	ad := &envAdapter{}
	argv, got, err := workloadCommand(ad, s.cfg, env)
	if err != nil || !slices.Equal(argv, []string{"true"}) || !ad.envBeforeArgv {
		t.Fatalf("argv %q, err %v, environment before Command %v", argv, err, ad.envBeforeArgv)
	}
	if !slices.Contains(ad.env, "OPENCODE_SERVER_PASSWORD=pw-1") || slices.Contains(ad.env, "KEYFILE=k") || !slices.Equal(ad.env, got) {
		t.Fatalf("adapter's environment %q, workload's %q", ad.env, got)
	}
}
