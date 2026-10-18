package proto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointBundleContents(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "implementation")
	git("commit", "-q", "--allow-empty", "-m", "unpublished")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/lux/pre-sync", base)
	git("update-ref", "refs/remotes/origin/remote-only", base)
	git("tag", "excluded", base)
	if err := os.WriteFile(filepath.Join(dir, "uncommitted"), []byte("not bundled"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, detached := range []bool{false, true} {
		if detached {
			git("checkout", "-q", "--detach", head)
		}
		bundle := filepath.Join(t.TempDir(), "checkpoint.bundle")
		cmd := exec.Command("sh", "-c", BundleScript, "sh", "checkpoint", bundle, "")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("bundle: %v: %s", err, out)
		}
		want := head + " refs/heads/implementation\n" + base + " refs/heads/main\n" + head + " HEAD"
		if got := git("bundle", "list-heads", bundle); got != want {
			t.Fatalf("heads: %q, want %q", got, want)
		}
		unchanged := filepath.Join(t.TempDir(), "unchanged.bundle")
		cmd = exec.Command("sh", "-c", BundleScript, "sh", "checkpoint", unchanged, want)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil || string(out) != "lux-unchanged\n" {
			t.Fatalf("unchanged: %v: %s", err, out)
		}
		if _, err := os.Stat(unchanged); !os.IsNotExist(err) {
			t.Fatalf("unchanged refs produced a bundle: %v", err)
		}
		clone := filepath.Join(t.TempDir(), "clone")
		git("clone", "-q", bundle, clone)
		git("-C", clone, "cat-file", "-e", head+"^{commit}")
		if _, err := os.Stat(filepath.Join(clone, "uncommitted")); !os.IsNotExist(err) {
			t.Fatalf("uncommitted file in clone: %v", err)
		}
	}
}
