package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	// What the patch cannot carry is said on stderr.
	errOut.Reset()
	lim := server.RunDiff{Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "p\n", NormalizedPaths: []string{"crlf.txt"}, DirtySubmodules: []string{"vendor/lib"}}}}
	if err := a.printDiff(lim, false, false); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"line endings or encoding; the patch has its stored form: crlf.txt", "not in the patch: vendor/lib"} {
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

// pendingLuxd answers diff_pending (retry after 1s) pending times, then
// the diff.
type pendingLuxd struct {
	mu      sync.Mutex
	pending int
	asked   []string
}

func (f *pendingLuxd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	if f.pending > 0 {
		f.pending--
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"code":"diff_pending","message":"m","retryAfter":1,"snapshotId":"snap_9"}}`))
		return
	}
	json.NewEncoder(w).Encode(server.RunDiff{Base: "clone", Repos: []server.RepoDiff{{Repo: "app", Files: 1, Patch: "diff --git a/x b/x\n"}}})
}

func runDiffCLI(t *testing.T, h http.Handler, args ...string) (string, string, int) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	code := a.main(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
	return out.String(), errOut.String(), code
}

// diff_pending: exit 4 with when to try again; --wait polls until the
// diff is there, or --timeout; diff_unavailable: exit 3 with why.
// --snapshot asks for that snapshot.
func TestDiffPendingAndWait(t *testing.T) {
	f := &pendingLuxd{pending: 1}
	_, errOut, code := runDiffCLI(t, f, "diff", "r1")
	if code != 4 || !strings.Contains(errOut, "diff for snapshot snap_9 is still being computed; try again in 1s") {
		t.Errorf("pending: %d %q", code, errOut)
	}

	f = &pendingLuxd{pending: 2}
	start := time.Now()
	out, errOut, code := runDiffCLI(t, f, "diff", "r1", "--wait")
	if code != 0 || !strings.Contains(out, "diff --git a/x b/x") || len(f.asked) != 3 || time.Since(start) < 2*time.Second {
		t.Errorf("--wait: %d %q %q, %d requests in %s", code, out, errOut, len(f.asked), time.Since(start))
	}

	f = &pendingLuxd{pending: 100}
	start = time.Now()
	_, errOut, code = runDiffCLI(t, f, "diff", "r1", "--wait", "--timeout", "2500ms")
	if code != 4 || !strings.Contains(errOut, "still being computed after 2.5s") || time.Since(start) > 4*time.Second {
		t.Errorf("--wait --timeout: %d %q after %s", code, errOut, time.Since(start))
	}

	_, errOut, code = runDiffCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"diff_unavailable","message":"m","reason":"superseded: the Run resumed on this host","snapshotId":"snap_9"}}`))
	}), "diff", "r1")
	if code != 3 || !strings.Contains(errOut, "snapshot snap_9 has no diff: superseded") {
		t.Errorf("unavailable: %d %q", code, errOut)
	}

	f = &pendingLuxd{}
	runDiffCLI(t, f, "diff", "r1", "--snapshot", "snap_1")
	if len(f.asked) != 1 || !strings.Contains(f.asked[0], "snapshot=snap_1") {
		t.Errorf("--snapshot: %v", f.asked)
	}
}
