package cli

import (
	"path/filepath"
	"reflect"
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
		{server.LifecycleEvent{Type: "host.placement_ended", Count: 1, Data: map[string]any{"run": "run_1", "epoch": 2.0, "outcome": "lost", "reason": "host lost"}},
			"run_1 epoch 2: lost (host lost)"},
	} {
		if got := eventLine(c.e); got != c.want {
			t.Errorf("%s: %q, want %q", c.e.Type, got, c.want)
		}
	}
}
