package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

func TestPrintDiff(t *testing.T) {
	d := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{
		{Repo: "app", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40),
			Files: 2, Insertions: 3, Deletions: 1, Truncated: true,
			FileStats: []proto.DiffFile{{Path: "x.go", Insertions: 3, Deletions: 1}, {Path: "img.png", Binary: true}},
			Patch:     "diff --git a/x.go b/x.go\n+new\n"},
		{Repo: "same", Files: 0},
	}}
	var out, errOut bytes.Buffer
	a := &app{stdout: &out, stderr: &errOut}
	if err := a.printDiff(d, false, false); err != nil {
		t.Fatal(err)
	}
	want := "# repo app: aaaaaaaaaaaa..bbbbbbbbbbbb, TRUNCATED\n" +
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

	// What the patch cannot carry is said on stderr.
	errOut.Reset()
	lim := server.RunDiff{Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "p\n", FiltersIgnored: true, FilteredPaths: []string{"big.psd"},
		NormalizedPaths: []string{"crlf.txt"}, DirtySubmodules: []string{"vendor/lib"}}}}
	if err := a.printDiff(lim, false, false); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"compared raw (filters are not run): big.psd", "line endings or encoding; the patch has its stored form: crlf.txt", "not in the patch: vendor/lib"} {
		if !strings.Contains(errOut.String(), w) {
			t.Errorf("stderr lacks %q: %q", w, errOut.String())
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

// mainCLI runs lux as Main does against h: stdout, stderr and the exit code.
func mainCLI(t *testing.T, h http.Handler, args ...string) (string, string, int) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	code := a.main(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
	return out.String(), errOut.String(), code
}

// lux diff's exit codes: 4 for a Run that is not running (with luxd's
// message), 3 for no_diff, 1 when a repository's diff failed (with -o json
// too, after printing it), 0 otherwise. Its flags become the query.
func TestDiffCommand(t *testing.T) {
	ok := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "diff --git a/x b/x\n"}}}
	failed := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "p\n"}, {Repo: "lib", Error: "gone", ErrorCode: "base_unreachable"}}}
	stopped := "the Run is stopped: its diff is available only while the Run is running; resume it, or save a patch at stop with workload.beforeStop (git diff > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts"
	f := &fakeLuxd{
		bodies: map[string]any{"/v1/runs/r_ok/diff": ok, "/v1/runs/r_bad/diff": failed},
		status: map[string]int{"/v1/runs/r_none/diff": http.StatusNotFound, "/v1/runs/r_stopped/diff": http.StatusConflict},
		errors: map[string][2]string{"/v1/runs/r_none/diff": {"no_diff", "the Run has no repositories"},
			"/v1/runs/r_stopped/diff": {"run_not_running", stopped}},
	}
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"diff", "r_ok"}, 0},
		{[]string{"diff", "r_ok", "-o", "json"}, 0},
		{[]string{"diff", "r_none"}, 3},
		{[]string{"diff", "r_stopped"}, 4},
		{[]string{"diff", "r_stopped", "-o", "json"}, 4},
		{[]string{"diff", "r_bad"}, 1},
		{[]string{"diff", "r_bad", "-o", "json"}, 1},
	} {
		out, errOut, code := mainCLI(t, f, c.args...)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
		if c.args[1] == "r_stopped" && errOut != "lux: "+stopped+"\n" {
			t.Errorf("%v: stderr %q", c.args, errOut)
		}
		if c.args[1] == "r_bad" && len(c.args) > 2 {
			var d server.RunDiff
			if err := json.Unmarshal([]byte(out), &d); err != nil || len(d.Repos) != 2 || d.Repos[1].ErrorCode != "base_unreachable" {
				t.Errorf("json not printed: %v %q", err, out)
			}
		}
	}
	f.seen = nil
	mainCLI(t, f, "diff", "r_ok", "--repo", "app", "--base", "head", "--stat")
	if len(f.seen) != 1 || f.seen[0] != "/v1/runs/r_ok/diff?base=head&repo=app&stat=true" {
		t.Errorf("query: %v", f.seen)
	}
	if _, _, code := mainCLI(t, f, "diff", "r_ok", "--base", "snapshot"); code != 1 {
		t.Errorf("--base snapshot: exit %d", code)
	}
}
