package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// stageCheckpointDir writes the stop-time checkpoint stage of run1's repo
// api: results.json naming 0.bundle, which the caller creates.
func stageCheckpointDir(t *testing.T, f *finishFixture) string {
	t.Helper()
	f.p.assign.Spec.Git = &spec.Git{Repositories: []spec.Repository{{Name: "api", Path: "/work/api"}}}
	dir := filepath.Join(f.r.cfg.DataDir, "rt", "checkpoints-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	results := []proto.StagedCheckpoint{{Repo: "api", Head: head, Refs: map[string]string{"HEAD": head}, Size: 4, File: "0.bundle"}}
	b, _ := json.Marshal(results)
	if err := os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *finishFixture) warnings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.reports {
		var ev proto.RunEvent
		if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == proto.EvWarning {
			msg, _ := ev.Data["message"].(string)
			out = append(out, msg)
		}
	}
	return out
}

// A destination downloads a checkpoint up to the assignment's checkpoint
// limit, not the default: one at a limit below the default is accepted,
// one byte over it refused and removed. The request names the destination
// placement.
func TestDownloadCheckpointUsesAssignedLimit(t *testing.T) {
	f := newFinishFixture(t)
	const limit = 1 << 10
	content := bytes.Repeat([]byte("b"), limit)
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		zw, _ := zstd.NewWriter(w)
		_, _ = zw.Write(content)
		_ = zw.Close()
	}))
	defer srv.Close()
	f.r.api = newAPI(srv.URL, "tok", "host")
	for _, c := range []struct {
		limit int64
		ok    bool
	}{{limit, true}, {limit - 1, false}} {
		f.p.assign.CheckpointBytes = c.limit
		path, err := f.p.downloadCheckpoint(context.Background(), "blob-1")
		if (err == nil) != c.ok {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		if c.ok {
			if b, _ := os.ReadFile(path); !bytes.Equal(b, content) {
				t.Fatalf("downloaded %d bytes", len(b))
			}
			os.Remove(path)
		}
	}
	if query != "host=host&run=run1&epoch=1" {
		t.Fatalf("query %q", query)
	}
	left, _ := filepath.Glob(filepath.Join(f.r.cfg.DataDir, "checkpoint-*.bundle"))
	if len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// An on-demand checkpoint of a running Run collects what the shim staged
// and then removes its checkpoint-live-* stage from the runtime volume.
func TestCheckpointRunningRemovesItsStage(t *testing.T) {
	f := newFinishFixture(t)
	f.p.assign.Spec.Git = &spec.Git{Repositories: []spec.Repository{{Name: "api", Path: "/work/api"}}}
	f.p.user = passwd.User{UID: os.Getuid(), GID: os.Getgid()}
	f.p.assign.CheckpointRefs = map[string]string{"api": "previous-refs"}
	rt := filepath.Join(f.r.cfg.DataDir, "rt")
	head := strings.Repeat("a", 40)
	argsFile := filepath.Join(t.TempDir(), "args.json")
	// `podman exec ... lux-shim checkpoint ARGS`: record ARGS, stage
	// 0.bundle in the one live stage dir and print its result, as the shim
	// does.
	script := `#!/bin/sh
[ "$1" = exec ] || exit 1
eval "printf '%s' \"\${$#}\"" > ` + argsFile + `
d=$(ls -d ` + rt + `/checkpoint-live-*)
printf data > "$d/0.bundle"
printf '[{"repo":"api","head":"` + head + `","refs":{"HEAD":"` + head + `"},"size":4,"file":"0.bundle"}]'
`
	bin := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.r.pm = &podman.Podman{Bin: bin}
	f.p.checkpointRunning(context.Background())
	f.mu.Lock()
	reported := 0
	for _, fr := range f.reports {
		if fr.Type == proto.MsgCheckpointReady {
			reported++
		}
	}
	f.mu.Unlock()
	if reported != 1 {
		t.Fatalf("checkpoint.ready reports: %d; warnings %q", reported, f.warnings())
	}
	if left, _ := filepath.Glob(filepath.Join(rt, "checkpoint-live-*")); len(left) != 0 {
		t.Fatalf("stage left on the runtime volume: %v", left)
	}
	// The shim compares against the refs of the latest checkpoint luxd
	// acknowledged, so an unchanged repository is not bundled again.
	var args proto.CheckpointArgs
	if b, err := os.ReadFile(argsFile); err != nil || json.Unmarshal(b, &args) != nil {
		t.Fatalf("shim args: %s %v", b, err)
	}
	if len(args.Repos) != 1 || args.Repos[0].Name != "api" || args.Repos[0].Previous != "previous-refs" {
		t.Fatalf("shim repos %+v", args.Repos)
	}
}

// A repository the shim found unchanged since its latest checkpoint is
// reported as a git.checkpoint event with unchanged, and nothing uploaded.
func TestCollectCheckpointReportsUnchanged(t *testing.T) {
	f := newFinishFixture(t)
	dir := stageCheckpointDir(t, f)
	b, _ := json.Marshal([]proto.StagedCheckpoint{{Repo: "api", Unchanged: true}})
	if err := os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.p.collectCheckpoints(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		var found, ready bool
		for _, fr := range f.reports {
			var ev proto.RunEvent
			if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == "git.checkpoint" {
				found = ev.Data["repo"] == "api" && ev.Data["unchanged"] == true
			}
			ready = ready || fr.Type == proto.MsgCheckpointReady
		}
		f.mu.Unlock()
		if ready {
			t.Fatal("an unchanged repository was reported as a checkpoint")
		}
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no unchanged git.checkpoint event")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A FIFO the workload planted in place of a staged bundle is refused at
// once with a warning; collection neither blocks on it nor reports it.
func TestCollectCheckpointRefusesFIFO(t *testing.T) {
	f := newFinishFixture(t)
	dir := stageCheckpointDir(t, f)
	if err := syscall.Mkfifo(filepath.Join(dir, "0.bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.collectCheckpoints(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Unblock the open so the goroutine can end before the test does.
		if w, err := os.OpenFile(filepath.Join(dir, "0.bundle"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("collection blocked on a FIFO")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(strings.Join(f.warnings(), "\n"), "api: checkpoints-1/0.bundle is not a regular file") {
		if time.Now().After(deadline) {
			t.Fatalf("warnings %q", f.warnings())
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.reports {
		if fr.Type == proto.MsgCheckpointReady {
			t.Fatalf("reported a FIFO: %s", fr.Data)
		}
	}
}
