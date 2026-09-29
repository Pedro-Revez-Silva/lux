package cli

import (
	"bytes"
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
