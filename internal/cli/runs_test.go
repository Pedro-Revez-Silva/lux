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
	"time"
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

func TestRunDryRunUsesValidateOnly(t *testing.T) {
	var query string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true,"wouldStartNow":false,"effectivePool":"p","effectiveResources":{"cpus":1,"memory":2,"disk":3},"blockers":[{"code":"space_not_settled","hosts":1,"remainingSeconds":5}]}`))
	})
	out, err := runCLI(t, h, "run", "--spot", "--dry-run", "--image", "alpine", "--", "true")
	want := "Valid spec; effective pool p, CPUs 1, memory 2 bytes, disk 3 bytes\n" +
		"Would not start now (advisory; no host launches or reclamation are promised):\n" +
		"  space_not_settled (wait at least 5s if nothing changes)\n"
	if err != nil || query != "validateOnly=true" || out != want {
		t.Fatalf("query %q err %v output:\n%q\nwant:\n%q", query, err, out, want)
	}
}

// --dry-run sends each secret by name with its reference as written: the
// ${NAME} is not resolved from the environment, nor any .env file read
// (with --secrets-from naming a missing file, and without it).
func TestRunDryRunDoesNotResolveSecretValues(t *testing.T) {
	file := filepath.Join(t.TempDir(), "spot.yaml")
	if err := os.WriteFile(file, []byte("spot: true\nimage: {ref: alpine}\nworkload: {command: [\"true\"]}\nsecrets: [{name: TOKEN, value: '${LUX_TEST_ABSENT_SECRET}'}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUX_TEST_ABSENT_SECRET", "resolved-value-must-not-be-sent")
	for _, extra := range [][]string{nil, {"--secrets-from", filepath.Join(t.TempDir(), "absent.env")}} {
		var body map[string]any
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("POST body %s: %v", b, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"valid":true,"effectivePool":"p","effectiveResources":{"cpus":1,"memory":2,"disk":3}}`))
		})
		if _, err := runCLI(t, h, append([]string{"run", "-f", file, "--dry-run"}, extra...)...); err != nil {
			t.Fatal(err)
		}
		secrets, _ := body["secrets"].([]any)
		if len(secrets) != 1 {
			t.Fatalf("secrets sent %v, want TOKEN", body["secrets"])
		}
		sec, _ := secrets[0].(map[string]any)
		if sec["name"] != "TOKEN" || (sec["value"] != nil && sec["value"] != "${LUX_TEST_ABSENT_SECRET}") {
			t.Fatalf("secret sent %v: want TOKEN by name, its value unresolved", sec)
		}
		if b, _ := json.Marshal(body); strings.Contains(string(b), "resolved-value-must-not-be-sent") {
			t.Fatalf("the resolved value was sent: %s", b)
		}
	}
}

func TestRunIdleTimeoutFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(file, []byte("image: {ref: alpine}\nworkload: {adapter: acp, idleTimeout: 5m}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "5m0s"}, {[]string{"--idle-timeout", "2m"}, "2m0s"}, {[]string{"--idle-timeout", "0"}, "0s"},
	} {
		body := submitted(t, append([]string{"run", "-f", file}, c.args...)...)
		if got := body["workload"].(map[string]any)["idleTimeout"]; got != c.want {
			t.Fatalf("got %v want %s", got, c.want)
		}
	}
}

func TestRunSpotFlag(t *testing.T) {
	body := submitted(t, "run", "--spot", "--image", "alpine", "--", "true")
	if body["spot"] != true {
		t.Fatalf("spot = %v", body["spot"])
	}
	file := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(file, []byte("spot: true\nresumePolicy: auto\nimage: {ref: alpine}\nworkload: {command: [\"true\"]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = submitted(t, "run", "-f", file)
	if body["spot"] != true || body["resumePolicy"] != "auto" {
		t.Fatalf("spec must preserve explicit policy for server validation: %v", body)
	}
	body = submitted(t, "run", "-f", file, "--spot=false")
	if _, exists := body["spot"]; exists || body["resumePolicy"] != "auto" {
		t.Fatalf("flag override: %v", body)
	}
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

// resume sends --disk-grace as resources.diskGrace, "0s" included; without
// the flag it sends none.
func TestResumeDiskGraceFlag(t *testing.T) {
	sent := func(args ...string) map[string]any {
		t.Helper()
		var body map[string]any
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == "GET" && r.URL.Path == "/v1/runs/run_1":
				_, _ = w.Write([]byte(`{"id":"run_1","state":"stopped"}`))
			case r.Method == "POST" && r.URL.Path == "/v1/runs/run_1/resume":
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming"}`))
			default:
				http.NotFound(w, r)
			}
		})
		if _, err := runCLI(t, h, append([]string{"resume", "run_1"}, args...)...); err != nil {
			t.Fatal(err)
		}
		res, _ := body["resources"].(map[string]any)
		return res
	}
	if res := sent("--disk-grace", "5m"); !reflect.DeepEqual(res, map[string]any{"diskGrace": "5m"}) {
		t.Errorf("--disk-grace 5m: resources %v", res)
	}
	if res := sent("--disk-grace", "0s"); !reflect.DeepEqual(res, map[string]any{"diskGrace": "0s"}) {
		t.Errorf("--disk-grace 0s: resources %v", res)
	}
	if res := sent(); res != nil {
		t.Errorf("no flag: resources %v, want none", res)
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

// lux get prints the Run's stage, its reason while stopping, and since when.
func TestGetPrintsTheStage(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run_1","state":"stopping","stage":"stopping","stageReason":"migrate",
			"stageSince":"2026-10-09T12:00:05Z","createdAt":"2026-10-09T12:00:00Z","spec":{}}`))
	})
	out, err := runCLI(t, h, "get", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	since, _ := time.Parse(time.RFC3339, "2026-10-09T12:00:05Z")
	if want := "stage:     stopping (migrate) since " + since.Local().Format(time.RFC3339) + "\n"; !strings.Contains(out, want) {
		t.Errorf("lux get printed:\n%s\nwant a line %q", out, want)
	}
	if strings.Contains(out, "idle") {
		t.Errorf("lux get printed an idle line without idleTimeout:\n%s", out)
	}
}

// lux get shows a set idle timeout in the label column.
func TestGetPrintsTheIdleTimeout(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run_1","state":"running","createdAt":"2026-10-09T12:00:00Z",
			"spec":{"workload":{"adapter":"codex","idleTimeout":"15m"}}}`))
	})
	out, err := runCLI(t, h, "get", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "adapter:   codex\nidle stop: 15m0s\n"; !strings.Contains(out, want) {
		t.Errorf("lux get printed:\n%s\nwant %q", out, want)
	}
}
