package cli

import (
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// --platform asks for the platform's pool of that name.
func TestPoolEventsPlatform(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/pools/burst/events": map[string]any{"events": []server.LifecycleEvent{}}}}
	for _, c := range []struct {
		args  []string
		owner bool
	}{
		{[]string{"pools", "events", "burst"}, false},
		{[]string{"pools", "events", "burst", "--platform"}, true},
	} {
		f.seen = nil
		if _, err := runCLI(t, f, c.args...); err != nil {
			t.Fatal(err)
		}
		if len(f.seen) != 1 || strings.Contains(f.seen[0], "owner=platform") != c.owner {
			t.Errorf("%v requested %v", c.args, f.seen)
		}
	}
}
