package shim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

func TestCheckpointStagesLocalBranches(t *testing.T) {
	m := newModeRepo(t)
	gitT(t, m.dir, "checkout", "-q", "-b", "implementation")
	head := m.commitLocal("implementation.txt", "unpublished\n")
	gitT(t, m.dir, "update-ref", "refs/lux/pre-sync", head)
	m.write("dirty.txt", "not committed")
	a := proto.CheckpointArgs{Dir: t.TempDir(), MaxBytes: proto.DefaultCheckpointBytes}
	repo := proto.CheckpointRepo{Name: "api", Path: m.dir}
	res := checkpointRepo(context.Background(), a, repo, 0)
	if res.Error != "" || res.Head != head || res.Branch != "implementation" || res.Size == 0 {
		t.Fatalf("%+v", res)
	}
	if len(res.Refs) != 3 {
		t.Fatalf("refs: %v", res.Refs)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, m.dir, "clone", "-q", filepath.Join(a.Dir, res.File), clone)
	if got := gitT(t, clone, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head %s", got)
	}
	if _, err := os.Stat(filepath.Join(clone, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty file: %v", err)
	}
	repo.Previous = gitT(t, m.dir, "bundle", "list-heads", filepath.Join(a.Dir, res.File))
	res = checkpointRepo(context.Background(), a, repo, 1)
	if !res.Unchanged || res.Error != "" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "1.bundle")); !os.IsNotExist(err) {
		t.Fatalf("unchanged bundle: %v", err)
	}
}

func TestCheckpointLimitAndFailure(t *testing.T) {
	m := newModeRepo(t)
	a := proto.CheckpointArgs{Dir: t.TempDir(), MaxBytes: 1}
	repo := proto.CheckpointRepo{Name: "api", Path: m.dir}
	res := checkpointRepo(context.Background(), a, repo, 0)
	if !strings.Contains(res.Error, "1-byte limit") {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "0.bundle")); !os.IsNotExist(err) {
		t.Fatalf("oversize bundle: %v", err)
	}
	repo.Path = t.TempDir()
	res = checkpointRepo(context.Background(), a, repo, 1)
	if res.Error == "" {
		t.Fatalf("%+v", res)
	}
}
