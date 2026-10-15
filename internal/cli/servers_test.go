package cli

import (
	"encoding/json"
	"net/http"
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
	// $0 too: bash as /bin/sh (macOS) sets BASH_VERSION in the fallback.
	out := runShell(t, dir, "echo bash-${BASH_VERSION:+yes}; echo \"0=$0\"; exit\n")
	if !slices.Contains(out, "bash-yes") || !slices.Contains(out, "0=bash") {
		t.Fatalf("got %q", out)
	}
}

// The --inject flag reaches the request body of server create, add and
// update; update sends inject only when the flag is given, "" included.
func TestServerInjectFlagSent(t *testing.T) {
	for _, c := range []struct {
		name   string
		args   []string
		path   string
		inject any // nil: no inject key
	}{
		{"create", []string{"server", "create", "web", "3000", "--inject", "<meta x>"}, "POST /v1/servers", "<meta x>"},
		{"add", []string{"server", "add", "run_1", "web", "3000", "--inject", "<meta x>"}, "POST /v1/runs/run_1/servers", "<meta x>"},
		{"update", []string{"server", "update", "srv_a", "--inject", "<meta x>"}, "PATCH /v1/servers/srv_a", "<meta x>"},
		{"update without --inject", []string{"server", "update", "srv_a", "--port", "3001"}, "PATCH /v1/servers/srv_a", nil},
		{"update --inject \"\"", []string{"server", "update", "srv_a", "--inject", ""}, "PATCH /v1/servers/srv_a", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var seen string
			var body map[string]any
			f := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Method + " " + r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"srv_a","name":"web"}`))
			})
			if _, err := runCLI(t, f, c.args...); err != nil {
				t.Fatal(err)
			}
			got, ok := body["inject"]
			if seen != c.path || (c.inject == nil) == ok || (ok && got != c.inject) {
				t.Fatalf("sent %s %v, want %s with inject %v", seen, body, c.path, c.inject)
			}
		})
	}
}
