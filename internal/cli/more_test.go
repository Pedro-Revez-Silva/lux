package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
)

func TestDownloadPathStaysInside(t *testing.T) {
	for _, bad := range []string{"/../../etc/passwd", "../x", "/workspace/../../x", "/", ""} {
		if p, err := downloadPath("/tmp/d", 1, bad); err == nil {
			t.Errorf("%q → %q: want refused", bad, p)
		}
	}
	p, err := downloadPath("/tmp/d", 2, "/workspace/out/a.txt")
	if err != nil || p != filepath.FromSlash("/tmp/d/2/workspace/out/a.txt") {
		t.Errorf("got %q, %v", p, err)
	}
}

func TestParseAddRepo(t *testing.T) {
	f := false
	for in, want := range map[string]spec.Repository{
		"two=http://10.0.0.1:8080/two.git":                  {Name: "two", URL: "http://10.0.0.1:8080/two.git"},
		"two=https://h/o/two.git@main,credential=GIT_TOKEN": {Name: "two", URL: "https://h/o/two.git", Ref: "main", Credential: "GIT_TOKEN"},
		"g=git@github.com:o/r.git":                          {Name: "g", URL: "git@github.com:o/r.git"},
		"g=git@github.com:r.git":                            {Name: "g", URL: "git@github.com:r.git"},
		"g=git@github.com:o/r.git@v1":                       {Name: "g", URL: "git@github.com:o/r.git", Ref: "v1"},
		"s=ssh://git@h/o/r.git":                             {Name: "s", URL: "ssh://git@h/o/r.git"},
		"b=https://h/r.git,ref=feature/x,path=/workspace/b": {Name: "b", URL: "https://h/r.git", Ref: "feature/x", Path: "/workspace/b"},
		"c=https://h/r.git,push=false,credential=T":         {Name: "c", URL: "https://h/r.git", Push: &f, Credential: "T"},
		"q=https://h/r.git?a=1,b=2":                         {Name: "q", URL: "https://h/r.git?a=1,b=2"},
	} {
		got, err := parseAddRepo(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "two", "two=", "=u", "t=,credential=X", "t=https://h/r.git@a,ref=b", "t=https://h/r.git,push=maybe"} {
		if r, err := parseAddRepo(bad); err == nil {
			t.Errorf("%q: want an error, got %+v", bad, r)
		}
	}
}

// tenants ls shows each tenant's expiry: days, or never for 0.
func TestTenantsLsExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenants":[{"id":"t_a","name":"a","retentionDays":30,"expireAfterDays":90},
			{"id":"t_b","name":"b","retentionDays":5,"expireAfterDays":0}]}`))
	}))
	defer srv.Close()
	var out strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
	root := a.root()
	root.SetArgs([]string{"--url", srv.URL, "--api-key", "k", "tenants", "ls"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	header, rowA, rowB := strings.Fields(lines[0]), strings.Fields(lines[1]), strings.Fields(lines[2])
	if header[len(header)-1] != "EXPIRY" || rowA[len(rowA)-1] != "90d" || rowB[len(rowB)-1] != "never" || rowB[len(rowB)-2] != "5d" {
		t.Fatalf("table:\n%s", out.String())
	}
}

// artifacts --delete sends DELETE /v1/runs/<run>/artifacts and says how
// many went.
func TestArtifactsDelete(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":3}`))
	}))
	defer srv.Close()
	var out strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
	root := a.root()
	root.SetArgs([]string{"--url", srv.URL, "--api-key", "k", "artifacts", "run_1", "--delete"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"DELETE /v1/runs/run_1/artifacts"}) || out.String() != "deleted 3 artifacts\n" {
		t.Fatalf("sent %v, printed %q", seen, out.String())
	}
}

// artifacts --download with --delete is refused before any request.
func TestArtifactsDownloadDeleteRefused(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":3,"artifacts":[]}`))
	}))
	defer srv.Close()
	var out strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
	root := a.root()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--url", srv.URL, "--api-key", "k", "artifacts", "run_1", "--download", t.TempDir(), "--delete"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("--download --delete: %v, want refused as mutually exclusive", err)
	}
	if len(seen) != 0 || out.String() != "" {
		t.Fatalf("sent %v, printed %q, want nothing", seen, out.String())
	}
}

// poolsLuxd is a fake luxd for pools set: whoami says operator or tenant
// t1, GET /v1/pools lists pools, and each POST /v1/pools body is recorded.
func poolsLuxd(t *testing.T, operator bool, pools ...string) (string, *[]map[string]any) {
	t.Helper()
	posted := &[]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"operator": operator, "tenant": map[bool]string{false: "t1"}[operator]})
		case "GET /v1/pools":
			_, _ = w.Write([]byte(`{"pools":[` + strings.Join(pools, ",") + `]}`))
		case "POST /v1/pools":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("body: %v", err)
			}
			*posted = append(*posted, body)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, posted
}

// lux runs the CLI against url, returning its exit code and what it printed.
func lux(url string, args ...string) (code int, stdout, stderr string) {
	var out, errs strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errs}
	code = a.main(append([]string{"--url", url, "--api-key", "k"}, args...))
	return code, out.String(), errs.String()
}

// pools set --default alone sends a marker-only body, exactly name and
// isDefault, which luxd refuses to read as anything else; with a setting,
// the whole pool.
func TestPoolsSetDefaultSendsOnlyTheMark(t *testing.T) {
	url, got := poolsLuxd(t, false, `{"name":"a","tenant":"t1","provider":"static","isDefault":false}`)
	for _, args := range [][]string{{"pools", "set", "a", "--default"}, {"pools", "set", "a", "--default=false"}, {"pools", "set", "a", "--provider", "static", "--default"}} {
		if code, _, stderr := lux(url, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	if want := (map[string]any{"name": "a", "isDefault": true}); !reflect.DeepEqual((*got)[0], want) {
		t.Errorf("--default sent %v, want %v", (*got)[0], want)
	}
	if want := (map[string]any{"name": "a", "isDefault": false}); !reflect.DeepEqual((*got)[1], want) {
		t.Errorf("--default=false sent %v, want %v", (*got)[1], want)
	}
	if (*got)[2]["provider"] != "static" || (*got)[2]["isDefault"] != true {
		t.Errorf("a full set sent %v", (*got)[2])
	}
}

// The production pool of 2026-10-07: nested containers and --max 4. A set
// pasted without nestedContainers and with --max 10 is refused, naming the
// removal; --replace writes it; --dry-run prints both changes and writes
// nothing; a value-only change is written, and printed.
func TestPoolsSetRefusesRemovals(t *testing.T) {
	const existing = `{"name":"default","tenant":"t1","provider":"ec2","maxHosts":4,"isDefault":true,
		"template":{"launchTemplate":"lux-runner","nestedContainers":true}}`
	set := []string{"pools", "set", "default", "--provider", "ec2", "--max", "10", "--template", `{"launchTemplate":"lux-runner"}`}

	url, posted := poolsLuxd(t, false, existing)
	code, stdout, stderr := lux(url, set...)
	if code != 4 || len(*posted) != 0 {
		t.Fatalf("exit %d, posted %v; want 4 and nothing", code, *posted)
	}
	if !strings.Contains(stderr, "would remove template.nestedContainers from pool default") || !strings.Contains(stderr, "--replace") {
		t.Errorf("stderr %q", stderr)
	}
	if want := "maxHosts 4→10\ntemplate.nestedContainers true→-\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}

	code, stdout, _ = lux(url, append(set, "--dry-run")...)
	if code != 0 || len(*posted) != 0 || stdout != "maxHosts 4→10\ntemplate.nestedContainers true→-\n" {
		t.Errorf("--dry-run: exit %d, posted %v, stdout %q", code, *posted, stdout)
	}

	if code, _, stderr = lux(url, append(set, "--replace")...); code != 0 || len(*posted) != 1 || (*posted)[0]["maxHosts"] != 10.0 {
		t.Errorf("--replace: exit %d (%s), posted %v", code, stderr, *posted)
	}

	code, stdout, stderr = lux(url, "pools", "set", "default", "--provider", "ec2", "--max", "10",
		"--template", `{"launchTemplate":"lux-runner","nestedContainers":true}`)
	if code != 0 || len(*posted) != 2 || stdout != "maxHosts 4→10\n" {
		t.Errorf("value-only change: exit %d (%s), posted %d, stdout %q", code, stderr, len(*posted), stdout)
	}
}

// Optional top-level settings the pool has and the set leaves out are
// removals too; giving them again is not.
func TestPoolsSetRefusesRemovedOptionalSettings(t *testing.T) {
	url, posted := poolsLuxd(t, false, `{"name":"metal","tenant":"t1","provider":"static","scaleDownAfter":"10m0s",
		"warmWhileActive":true,"hourlyPrice":"0.4","currency":"USD"}`)
	code, _, stderr := lux(url, "pools", "set", "metal", "--provider", "static")
	if code != 4 || len(*posted) != 0 {
		t.Fatalf("exit %d, posted %v", code, *posted)
	}
	if !strings.Contains(stderr, "would remove currency, hourlyPrice, scaleDownAfterSeconds, warmWhileActive from pool metal") {
		t.Errorf("stderr %q", stderr)
	}
	code, stdout, stderr := lux(url, "pools", "set", "metal", "--provider", "static", "--scale-down-after", "10m",
		"--warm-while-active", "--hourly-price", "0.40", "--currency", "USD")
	if code != 0 || len(*posted) != 1 || stdout != "no changes\n" {
		t.Errorf("the same settings: exit %d (%s), posted %d, stdout %q", code, stderr, len(*posted), stdout)
	}
}

// A set naming no pool of the target owner says it creates one, and does;
// a platform pool of that name is not the tenant's.
func TestPoolsSetSaysItCreates(t *testing.T) {
	url, posted := poolsLuxd(t, false, `{"name":"arm64","provider":"ec2","platform":true}`)
	code, stdout, _ := lux(url, "pools", "set", "arm64", "--provider", "static", "--dry-run")
	if code != 0 || len(*posted) != 0 || stdout != "creating pool arm64 (no pool of that name for tenant t1)\n" {
		t.Errorf("--dry-run: exit %d, posted %v, stdout %q", code, *posted, stdout)
	}
	code, stdout, _ = lux(url, "pools", "set", "arm64", "--provider", "static")
	if code != 0 || len(*posted) != 1 || !strings.HasPrefix(stdout, "creating pool arm64") {
		t.Errorf("exit %d, posted %v, stdout %q", code, *posted, stdout)
	}
	url, _ = poolsLuxd(t, true)
	if code, stdout, _ = lux(url, "--tenant", "acme", "pools", "set", "arm64", "--provider", "static", "--dry-run"); stdout != "creating pool arm64 (no pool of that name for tenant acme)\n" {
		t.Errorf("operator --tenant acme: exit %d, stdout %q", code, stdout)
	}
}

// pools ls shows an OWNER column to an unscoped operator, and in any
// listing where the same name belongs to more than one owner.
func TestPoolsLsOwner(t *testing.T) {
	operator := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"operator": operator})
		case "/v1/pools":
			_, _ = w.Write([]byte(`{"pools":[{"name":"burst","provider":"ec2","platform":true,"isDefault":true},
				{"name":"burst","tenant":"acme","provider":"static","isDefault":false}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	ls := func(extra ...string) []string {
		t.Helper()
		var out strings.Builder
		a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
		root := a.root()
		root.SetArgs(append(append([]string{"--url", srv.URL, "--api-key", "k"}, extra...), "pools", "ls"))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(out.String()), "\n")
	}
	lines := ls()
	if got := strings.Fields(lines[0]); !reflect.DeepEqual(got[:3], []string{"NAME", "OWNER", "DEFAULT"}) {
		t.Fatalf("operator header %v", got)
	}
	if a, b := strings.Fields(lines[1]), strings.Fields(lines[2]); a[1] != "platform" || a[2] != "*" || b[1] != "acme" || b[2] != "static" {
		t.Fatalf("operator rows %q", lines[1:])
	}
	if got := strings.Fields(ls("--tenant", "acme")[0]); got[1] != "OWNER" {
		t.Fatalf("narrowed listing with duplicate names: header %v", got)
	}
	operator = false
	if got := strings.Fields(ls()[0]); got[1] != "OWNER" {
		t.Fatalf("tenant with duplicate names: header %v", got)
	}
}

func TestEventLine(t *testing.T) {
	last := time.Date(2026, 9, 29, 10, 0, 5, 0, time.Local)
	for _, c := range []struct {
		e    server.LifecycleEvent
		want string
	}{
		{server.LifecycleEvent{Type: "pool.launch_failed", Count: 3, LastTime: &last, Data: map[string]any{"error": "duplicate tag"}},
			"launch failed: duplicate tag (×3, last 2026-09-29 10:00:05)"},
		{server.LifecycleEvent{Type: "pool.host_released", Count: 1, Data: map[string]any{"name": "burst-1", "reason": "idle", "idleSeconds": 600.0}},
			"burst-1 released: idle for 600s"},
		{server.LifecycleEvent{Type: "pool.config_changed", Count: 1, Data: map[string]any{"created": false, "changes": map[string]any{
			"maxHosts": map[string]any{"old": 2.0, "new": 4.0}, "template.region": map[string]any{"old": nil, "new": "eu-west-1"}}}},
			"maxHosts 2→4, template.region -→eu-west-1"},
		{server.LifecycleEvent{Type: "pool.retired", Count: 1, Data: map[string]any{"created": false, "changes": map[string]any{
			"retired": map[string]any{"old": false, "new": true}}}},
			"retired false→true"},
		{server.LifecycleEvent{Type: "host.placement_ended", Count: 1, Data: map[string]any{"run": "run_1", "epoch": 2.0, "outcome": "lost", "reason": "host lost"}},
			"run_1 epoch 2: lost (host lost)"},
	} {
		if got := eventLine(c.e); got != c.want {
			t.Errorf("%s: %q, want %q", c.e.Type, got, c.want)
		}
	}
}

// pools rename posts the new name to the pool's rename path; --platform
// names the platform's pool.
func TestPoolsRename(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		paths, bodies = append(paths, r.Method+" "+r.URL.RequestURI()), append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"new"}`))
	}))
	defer srv.Close()
	for _, args := range [][]string{{"pools", "rename", "old", "new"}, {"pools", "rename", "old", "new", "--platform"}} {
		var out strings.Builder
		a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
		root := a.root()
		root.SetArgs(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if out.String() != "renamed old to new\n" {
			t.Errorf("%v printed %q", args, out.String())
		}
	}
	if want := []string{"POST /v1/pools/old/rename", "POST /v1/pools/old/rename?owner=platform"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("requests %v, want %v", paths, want)
	}
	for _, b := range bodies {
		if want := (map[string]any{"name": "new"}); !reflect.DeepEqual(b, want) {
			t.Errorf("body %v, want %v", b, want)
		}
	}
}

// A terminated host whose launch the provider refused reads "launch
// failed"; one launched and later terminated reads terminated.
func TestHostState(t *testing.T) {
	for _, c := range []struct {
		h    server.Host
		want string
	}{
		{server.Host{State: "terminated", Launch: &server.HostLaunch{Outcome: "failed"}}, "launch failed"},
		{server.Host{State: "terminated", Launch: &server.HostLaunch{Outcome: "launched"}}, "terminated"},
		{server.Host{State: "terminated"}, "terminated"},
		{server.Host{State: "provisioning", Launch: &server.HostLaunch{Outcome: "requested"}}, "provisioning"},
		{server.Host{State: "ready", Draining: true}, "ready (draining)"},
		{server.Host{State: "draining", Draining: true}, "draining"},
	} {
		if got := hostState(c.h); got != c.want {
			t.Errorf("%+v: %q, want %q", c.h, got, c.want)
		}
	}
}

// lux sync --wait names an operation in progress on a repository's line.
func TestSyncWaitNamesTheOperation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"requestId":"rq"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[
			{"id":1,"type":"git.sync","data":{"requestId":"rq","repo":"app","status":"kept","from":"aaaa","to":"bbbb","ahead":0,"behind":2,"operation":"rebase"}},
			{"id":2,"type":"git.sync","data":{"requestId":"rq","repo":"lib","status":"up-to-date","from":"cccc","to":"cccc","ahead":0,"behind":0}},
			{"id":3,"type":"sync.done","data":{"requestId":"rq","changed":false}}]}`))
	}))
	defer srv.Close()
	var out strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: io.Discard}
	root := a.root()
	root.SetArgs([]string{"--url", srv.URL, "--api-key", "k", "sync", "run_x", "app=main", "lib=main", "--mode", "fast-forward", "--wait"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "app kept aaaa → bbbb (0 ahead, 2 behind) [rebase in progress]\nlib up-to-date cccc → cccc (0 ahead, 0 behind)\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
}
