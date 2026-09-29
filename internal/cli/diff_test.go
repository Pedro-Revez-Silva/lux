package cli

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

func diffCLI(t *testing.T, d server.RunDiff, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	isolateConfig(t, "")
	srv := httptest.NewServer(&fakeLuxd{bodies: map[string]any{"/v1/runs/run_1/diff": d}})
	defer srv.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	code = a.main(append([]string{"--url", srv.URL, "--api-key", "k", "diff", "run_1"}, args...))
	return code, out.String(), errOut.String()
}

const cliPatch = "diff --git a/a.txt b/a.txt\nindex 1..2 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n-one\n+two\n+three\n" +
	"diff --git a/sp ace.bin b/sp ace.bin\nnew file mode 100644\nBinary files /dev/null and b/sp ace.bin differ\n"

func TestDiffText(t *testing.T) {
	d := server.RunDiff{Base: "clone", Repos: []server.RepoDiff{
		{Repo: "app", Base: "0123456789abcdef", Head: "fedcba9876543210", Patch: cliPatch, Files: 2, Insertions: 2, Deletions: 1, Truncated: true},
		{Repo: "same", Base: "0123456789abcdef", Head: "0123456789abcdef"},
	}}
	code, out, errOut := diffCLI(t, d)
	if code != 0 || out != "# repo app: 0123456789ab..fedcba987654\n"+cliPatch ||
		errOut != "lux: repo app: untracked files were cut or are binary; the patch will not apply cleanly\n" {
		t.Fatalf("exit %d\n%s\nstderr: %s", code, out, errOut)
	}
	_, out, _ = diffCLI(t, d, "--stat")
	want := "# repo app: 0123456789ab..fedcba987654\n a.txt      | 3 ++-\n sp ace.bin | Bin\n 2 files changed, 2 insertions(+), 1 deletion(-)\n"
	if out != want {
		t.Fatalf("stat:\n%q\nwant:\n%q", out, want)
	}
	// A repository that failed: exit 1, in JSON too.
	d.Repos[1].Error = "the checkout: gone"
	if code, _, errOut := diffCLI(t, d); code != 1 || !strings.Contains(errOut, "lux: repo same: the checkout: gone") {
		t.Fatalf("exit %d, %s", code, errOut)
	}
	if code, out, _ := diffCLI(t, d, "-o", "json"); code != 1 || !strings.Contains(out, `"error": "the checkout: gone"`) {
		t.Fatalf("exit %d, %s", code, out)
	}
}
