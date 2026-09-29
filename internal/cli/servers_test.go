package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runShell runs shellCommand with PATH set to dir, as lux shell would, and
// returns what it printed for input.
func runShell(t *testing.T, dir, input string) []string {
	t.Helper()
	cmd := exec.Command(shellCommand[0], shellCommand[1:]...)
	cmd.Env = []string{"PATH=" + dir, "HOME=" + t.TempDir()}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell: %v\n%s", err, out)
	}
	return strings.Split(string(out), "\n")
}

func TestShellFallsBackToShWithoutBash(t *testing.T) {
	// Neither bash nor sh on PATH: a failed exec would end the shell (127).
	out := runShell(t, t.TempDir(), "echo shell-$((20+22)); exit\n")
	if !slices.Contains(out, "shell-42") {
		t.Fatalf("got %q", out)
	}
}

func TestShellPrefersBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash here")
	}
	dir := t.TempDir()
	if err := os.Symlink(bash, filepath.Join(dir, "bash")); err != nil {
		t.Fatal(err)
	}
	out := runShell(t, dir, "echo bash-${BASH_VERSION:+yes}; exit\n")
	if !slices.Contains(out, "bash-yes") {
		t.Fatalf("got %q", out)
	}
}
