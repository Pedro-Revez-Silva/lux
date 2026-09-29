package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

func TestPrintDiff(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	d := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{
		{Repo: "app", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Source: "snapshot", SnapshotID: "snap_1", At: at,
			Files: 2, Insertions: 3, Deletions: 1, Truncated: true,
			FileStats: []proto.DiffFile{{Path: "x.go", Insertions: 3, Deletions: 1}, {Path: "img.png", Binary: true}},
			Patch:     "diff --git a/x.go b/x.go\n+new\n"},
		{Repo: "same", Source: "snapshot", Files: 0},
	}}
	var out, errOut bytes.Buffer
	a := &app{stdout: &out, stderr: &errOut}
	if err := a.printDiff(d, false, false); err != nil {
		t.Fatal(err)
	}
	want := "# repo app: aaaaaaaaaaaa..bbbbbbbbbbbb (snapshot, snapshot snap_1 at 2026-09-29T10:00:00Z), TRUNCATED\n" +
		"diff --git a/x.go b/x.go\n+new\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
	if !strings.Contains(errOut.String(), "cut at the size limit") {
		t.Errorf("stderr: %q", errOut.String())
	}

	out.Reset()
	if err := a.printDiff(d, true, false); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{" x.go    | 4 +++-\n", " img.png | Bin\n", " 2 files changed, 3 insertions(+), 1 deletion(-)\n"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("stat lacks %q:\n%s", w, out.String())
		}
	}

	// Nothing changed: nothing printed. A failed repository: an error.
	out.Reset()
	if err := a.printDiff(server.RunDiff{Repos: []server.RepoDiff{{Repo: "same"}}}, false, false); err != nil || out.Len() != 0 {
		t.Errorf("no change: %v %q", err, out.String())
	}
	if err := a.printDiff(server.RunDiff{Repos: []server.RepoDiff{{Repo: "bad", Error: "boom"}}}, false, false); err == nil {
		t.Error("a failed repository did not fail the command")
	}
}

func TestColorPatch(t *testing.T) {
	got := string(colorPatch([]byte("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n c\n")))
	for _, w := range []string{"\x1b[1m--- a/f\x1b[m\n", "\x1b[36m@@ -1 +1 @@\x1b[m\n", "\x1b[31m-a\x1b[m\n", "\x1b[32m+b\x1b[m\n", " c\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q in %q", w, got)
		}
	}
}

// lux diff's exit codes: 3 for no_diff, 1 when a repository's diff failed
// (with -o json too, after printing it), 0 otherwise.
func TestDiffCommandExitCodes(t *testing.T) {
	ok := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "diff --git a/x b/x\n"}}}
	failed := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "p\n"}, {Repo: "lib", Error: "gone", ErrorCode: "base_unreachable"}}}
	f := &fakeLuxd{
		bodies: map[string]any{"/v1/runs/r_ok/diff": ok, "/v1/runs/r_bad/diff": failed},
		status: map[string]int{"/v1/runs/r_none/diff": http.StatusNotFound},
		errors: map[string][2]string{"/v1/runs/r_none/diff": {"no_diff", "no snapshot of this Run has a diff"}},
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"diff", "r_ok"}, 0},
		{[]string{"diff", "r_ok", "-o", "json"}, 0},
		{[]string{"diff", "r_none"}, 3},
		{[]string{"diff", "r_none", "-o", "json"}, 3},
		{[]string{"diff", "r_bad"}, 1},
		{[]string{"diff", "r_bad", "-o", "json"}, 1},
	} {
		out, code := mainCLI(t, f, c.args...)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
		if c.args[1] == "r_bad" && len(c.args) > 2 {
			var d server.RunDiff
			if err := json.Unmarshal([]byte(out), &d); err != nil || len(d.Repos) != 2 || d.Repos[1].ErrorCode != "base_unreachable" {
				t.Errorf("json not printed: %v %q", err, out)
			}
		}
	}
}

// mainCLI runs lux as Main does against f: stdout and the exit code.
func mainCLI(t *testing.T, f *fakeLuxd, args ...string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	code := a.main(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
	return out.String(), code
}
