package runner

import (
	"context"
	"testing"
)

// A placement's stop reason is its first stop's, except that a terminate
// (or "cancel", from a luxd older than terminate) overrides an earlier one,
// so the shim and the exit report say the Run is ending for good.
func TestRequestStopTerminateOverrides(t *testing.T) {
	for _, c := range []struct{ first, then, want string }{
		{"stop", "terminate", "terminate"},
		{"terminate", "stop", "terminate"},
		{"migrate", "terminate", "terminate"},
		{"stop", "cancel", "cancel"},
		{"stop", "preempt", "stop"},
		{"", "stop", "stop"},
	} {
		t.Run(c.first+"/"+c.then, func(t *testing.T) {
			// Assigned, not started: no shim, no state on disk.
			p := &placement{phase: "assigned"}
			ctx := context.Background()
			if c.first != "" {
				p.requestStop(ctx, c.first)
			}
			p.requestStop(ctx, c.then)
			if got := p.pendingStop(); got != c.want {
				t.Fatalf("stop reason %q, want %q", got, c.want)
			}
		})
	}
}
