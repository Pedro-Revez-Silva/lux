package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lux rename sends PATCH /v1/runs/<run> {"name": ...}, "" included, and
// says what it did; luxd's refusal is printed and exits 4.
func TestRename(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		var body struct{ Name string }
		_ = json.Unmarshal(b, &body)
		if len(body.Name) > 200 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_spec","message":"name: 201 bytes, at most 200"}}`))
			return
		}
		out, _ := json.Marshal(map[string]any{"id": "run_1", "name": body.Name, "state": "running"})
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	code, out, errOut := lux(srv.URL, "rename", "run_1", "window 2")
	if code != 0 || out != "renamed run_1 to window 2\n" {
		t.Fatalf("rename: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = lux(srv.URL, "rename", "run_1", "")
	if code != 0 || out != "renamed run_1 to \n" {
		t.Fatalf("clear: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = lux(srv.URL, "-o", "json", "rename", "run_1", "j")
	if code != 0 || !strings.Contains(out, `"name": "j"`) {
		t.Fatalf("json: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := []string{
		`PATCH /v1/runs/run_1 {"name":"window 2"}`,
		`PATCH /v1/runs/run_1 {"name":""}`,
		`PATCH /v1/runs/run_1 {"name":"j"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	code, out, errOut = lux(srv.URL, "rename", "run_1", strings.Repeat("a", 201))
	if code != 4 || out != "" || errOut != "lux: name: 201 bytes, at most 200\n" {
		t.Fatalf("refused: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := lux(srv.URL, "rename", "run_1"); code == 0 || !strings.Contains(errOut, "accepts 2 arg(s)") {
		t.Fatalf("one argument: exit %d, stderr %q", code, errOut)
	}
}
