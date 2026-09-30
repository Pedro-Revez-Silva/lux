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

// pools set --default alone sends a marker-only body, exactly name and
// isDefault, which luxd refuses to read as anything else; with a setting,
// the whole pool.
func TestPoolsSetDefaultSendsOnlyTheMark(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		got = append(got, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, args := range [][]string{{"pools", "set", "a", "--default"}, {"pools", "set", "a", "--default=false"}, {"pools", "set", "a", "--provider", "static", "--default"}} {
		a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
		root := a.root()
		root.SetArgs(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if want := (map[string]any{"name": "a", "isDefault": true}); !reflect.DeepEqual(got[0], want) {
		t.Errorf("--default sent %v, want %v", got[0], want)
	}
	if want := (map[string]any{"name": "a", "isDefault": false}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("--default=false sent %v, want %v", got[1], want)
	}
	if got[2]["provider"] != "static" || got[2]["isDefault"] != true {
		t.Errorf("a full set sent %v", got[2])
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
