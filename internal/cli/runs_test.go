package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// submitted runs lux with args against a fake luxd and returns the JSON body
// it POSTed to /v1/runs.
func submitted(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/runs" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("POST body %s: %v", b, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run_1"}`))
	})
	if _, err := runCLI(t, h, args...); err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("nothing was POSTed to /v1/runs")
	}
	return body
}

func TestRunPoolFlag(t *testing.T) {
	placement := func(body map[string]any) map[string]any {
		p, _ := body["placement"].(map[string]any)
		return p
	}

	body := submitted(t, "run", "--image", "alpine", "--pool", "arm64", "--", "echo", "hi")
	if got := placement(body)["pool"]; got != "arm64" {
		t.Errorf("quick form: placement.pool = %v, want arm64 (body %v)", got, body)
	}

	file := filepath.Join(t.TempDir(), "spec.yaml")
	err := os.WriteFile(file, []byte("image: {ref: alpine}\nworkload: {adapter: generic, command: [true]}\nplacement: {pool: gpu, requires: {zone: a}}\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	body = submitted(t, "run", "-f", file, "--pool", "arm64")
	want := map[string]any{"pool": "arm64", "requires": map[string]any{"zone": "a"}}
	if p := placement(body); !reflect.DeepEqual(p, want) {
		t.Errorf("-f with --pool: placement = %v, want %v (the spec's requires kept)", p, want)
	}
	body = submitted(t, "run", "-f", file)
	if got := placement(body)["pool"]; got != "gpu" {
		t.Errorf("-f without --pool: placement.pool = %v, want the spec's gpu", got)
	}

	body = submitted(t, "run", "--image", "alpine", "--", "echo", "hi")
	if _, ok := placement(body)["pool"]; ok {
		t.Errorf("no --pool: placement.pool was sent, want it left to the server (body %v)", body)
	}
}
