package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/proto"
)

// Every Welcome replaces the control-plane ports: a reconnect to a luxd
// that sends none, or an older luxd's Welcome without the field, leaves
// the control plane fully blocked.
func TestWelcomeReplacesControlPlanePorts(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	ctx := context.Background()
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{9000, 8090}})
	if got := r.egress.ControlPlanePorts(); !slices.Equal(got, []int{8090, 9000}) {
		t.Fatalf("after a Welcome with ports: %v", got)
	}
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090}})
	if got := r.egress.ControlPlanePorts(); !slices.Equal(got, []int{8090}) {
		t.Fatalf("after a changed list: %v", got)
	}
	r.onWelcome(ctx, proto.Welcome{})
	if got := r.egress.ControlPlanePorts(); len(got) != 0 {
		t.Fatalf("after a Welcome with none: %v", got)
	}
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090}})
	var older proto.Welcome
	if err := json.Unmarshal([]byte(`{"hostId":"h1","leaseSeconds":30,"live":[]}`), &older); err != nil {
		t.Fatal(err)
	}
	r.onWelcome(ctx, older)
	if got := r.egress.ControlPlanePorts(); len(got) != 0 {
		t.Fatalf("after an older luxd's Welcome: %v", got)
	}
	// A list the firewall refuses opens nothing.
	many := make([]int, proto.MaxControlPlanePorts+1)
	for i := range many {
		many[i] = 1000 + i
	}
	for _, refused := range [][]int{many, {0, 8090}, {8090, 70000}} {
		r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090}})
		r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: refused})
		if got := r.egress.ControlPlanePorts(); len(got) != 0 {
			t.Fatalf("after refused list %v: %v", refused, got)
		}
	}
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090, 8090}})
	if got := r.egress.ControlPlanePorts(); !slices.Equal(got, []int{8090}) {
		t.Fatalf("duplicates: %v", got)
	}
}

// The runner logs the list once per real change, never on a reconnect
// that brings the same list.
func TestWelcomeLogsControlPlanePortsChange(t *testing.T) {
	var buf bytes.Buffer
	r := &Runner{log: slog.New(slog.NewTextHandler(&buf, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	ctx := context.Background()
	applied := func() int { return strings.Count(buf.String(), `level=INFO msg="control-plane ports"`) }
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090}})
	r.onWelcome(ctx, proto.Welcome{ControlPlanePorts: []int{8090}})
	if n := applied(); n != 1 {
		t.Fatalf("%d change lines after one change:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "ports=[8090]") {
		t.Errorf("change line lacks the ports:\n%s", buf.String())
	}
	r.onWelcome(ctx, proto.Welcome{})
	r.onWelcome(ctx, proto.Welcome{})
	if n := applied(); n != 2 {
		t.Fatalf("%d change lines after clearing:\n%s", n, buf.String())
	}
}
