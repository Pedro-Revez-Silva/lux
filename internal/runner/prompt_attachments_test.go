package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

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
