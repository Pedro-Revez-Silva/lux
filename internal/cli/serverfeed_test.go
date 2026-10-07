package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestFeedLine(t *testing.T) {
	run := "run_aaaa"
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	for _, c := range []struct {
		e    server.FeedEvent
		want []string
	}{
		{server.FeedEvent{Event: server.Event{Type: "server.wake_requested", ServerID: "srv_x", Time: at,
			Data: map[string]any{"hostname": "web.pr9.lux.test", "by": "ada@example.com", "path": "/goals"}}, Tenant: "t"},
			[]string{"server.wake_requested", "srv_x", "web.pr9.lux.test", "by ada@example.com at /goals", "  -  "}},
		{server.FeedEvent{Event: server.Event{Type: "server.idle", ServerID: "srv_x", Time: at,
			Data: map[string]any{"name": "web", "idleAfter": "10m0s"}}, RunID: &run, Tenant: "t"},
			[]string{"run_aaaa", "web", "no request for 10m0s"}},
		{server.FeedEvent{Event: server.Event{Type: "server.state", ServerID: "srv_x", Time: at,
			Data: map[string]any{"name": "web", "state": "exited", "exitCode": float64(3)}}, RunID: &run, Tenant: "t"},
			[]string{"exited exit 3"}},
		{server.FeedEvent{Event: server.Event{Type: "state", Time: at, Data: map[string]any{"state": "running"}}, RunID: &run, Tenant: "t"},
			[]string{"run_aaaa", `{"state":"running"}`}},
	} {
		got := feedLine(c.e)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.e.Type, got, w)
			}
		}
	}
}

func TestParseSyncs(t *testing.T) {
	refs, err := parseSyncs([]string{"app=main", "lib=feat/x=y"}, "")
	if err != nil || len(refs) != 2 || refs[0].Repo != "app" || refs[1].Ref != "feat/x=y" || refs[0].Mode != "" {
		t.Fatalf("%+v %v", refs, err)
	}
	// The call's mode goes with every repository.
	refs, err = parseSyncs([]string{"app=main", "lib=v1"}, "fast-forward")
	if err != nil || len(refs) != 2 || refs[0].Mode != "fast-forward" || refs[1].Mode != "fast-forward" {
		t.Fatalf("%+v %v", refs, err)
	}
	for _, bad := range []string{"app", "=main", "app="} {
		if _, err := parseSyncs([]string{bad}, ""); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
