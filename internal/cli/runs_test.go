package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if err := os.WriteFile(file, []byte("image: {ref: alpine}\nworkload: {adapter: generic, command: [true]}\nplacement: {pool: gpu, requires: {zone: a}}\n"), 0o600); err != nil {
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

// resume --wait -o json prints the Run it waited for with the resume's
// answer's resize, which GET does not carry.
func TestResumeWaitKeepsResize(t *testing.T) {
	const resize = `{"requested":{"disk":104857600},"applied":{"cpus":1,"memory":1073741824,"disk":21474836480},` +
		`"disk":{"requested":104857600,"kept":21474836480,"reason":"its saved state used up to 1 GiB"}}`
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/runs/run_1/resume":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming","resize":` + resize + `}`))
		case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
			_, _ = w.Write([]byte(`{"id":"run_1","state":"running"}`))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := runCLI(t, h, "-o", "json", "resume", "run_1", "--disk", "100Mi", "--wait")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		State  string          `json:"state"`
		Resize json.RawMessage `json:"resize"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	var want, have any
	_ = json.Unmarshal([]byte(resize), &want)
	_ = json.Unmarshal(got.Resize, &have)
	if got.State != "running" || !reflect.DeepEqual(have, want) {
		t.Fatalf("state %q resize %s, want running with %s", got.State, got.Resize, resize)
	}
}

// resume sends --secret for a name the Run lacks (a declaration), drops a
// --remove-secret name from the secrets it looks up, and sends it in
// removeSecrets.
func TestResumeSecretFlags(t *testing.T) {
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
			_, _ = w.Write([]byte(`{"id":"run_1","state":"stopped","secrets":[{"name":"TOKEN"},{"name":"OLD"}]}`))
		case r.Method == "POST" && r.URL.Path == "/v1/runs/run_1/resume":
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("POST body %s: %v", b, err)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming"}`))
		default:
			http.NotFound(w, r)
		}
	})
	t.Setenv("OLD", "from-the-environment")
	if _, err := runCLI(t, h, "resume", "run_1", "--secret", "TOKEN=t-1", "--secret", "EXTRA=e-1", "--remove-secret", "OLD"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"secrets":       []any{map[string]any{"name": "TOKEN", "value": "t-1"}, map[string]any{"name": "EXTRA", "value": "e-1"}},
		"removeSecrets": []any{"OLD"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("POST %v, want %v", body, want)
	}
}

// A name given both as --secret and --remove-secret is refused before any
// resume is POSTed.
func TestResumeSecretSuppliedAndRemoved(t *testing.T) {
	posted := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
			_, _ = w.Write([]byte(`{"id":"run_1","state":"stopped","secrets":[{"name":"TOKEN"}]}`))
		case r.Method == "POST":
			posted = true
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming"}`))
		default:
			http.NotFound(w, r)
		}
	})
	_, err := runCLI(t, h, "resume", "run_1", "--secret", "TOKEN=t-1", "--remove-secret", "TOKEN")
	if err == nil || !strings.Contains(err.Error(), `secret "TOKEN" is both supplied and removed`) {
		t.Fatalf("err = %v, want TOKEN both supplied and removed", err)
	}
	if posted {
		t.Fatal("a request was POSTed despite the conflict")
	}
}
