package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Hello offers spot reclamation only when the --shim says it handles it:
// this tree's shim does; a shim that knows sync modes but not the
// capabilities subcommand (the shim before reclamation: it starts as PID 1
// without a config and exits 125) does not, whatever its sync modes.
func TestShimKnowsReclaim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "lux-shim")
	build := exec.Command("go", "build", "-o", shim, "github.com/marcioapm/lux/cmd/lux-shim")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if !shimKnowsReclaim(shim) || !shimKnowsSyncModes(shim) {
		t.Fatal("this tree's lux-shim: no reclaim or no sync modes")
	}
	old := filepath.Join(dir, "old-shim")
	script := "#!/bin/sh\nif [ \"$1\" = sync ]; then echo '[{\"repo\":\"probe\",\"ref\":\"\",\"mode\":\"" + proto.SyncModeProbe + "\",\"status\":\"failed\"}]'; exit 0; fi\n" +
		"echo 'lux-shim: no config' >&2; exit 125\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if !shimKnowsSyncModes(old) {
		t.Fatal("the old shim's sync probe should pass")
	}
	if shimKnowsReclaim(old) || shimKnowsReclaim(filepath.Join(dir, "missing")) {
		t.Fatal("an old or missing lux-shim: reclaim")
	}
	r := &Runner{shimSyncModes: true, shimReclaim: shimKnowsReclaim(old)}
	if caps := r.capabilities(); slices.Contains(caps, proto.CapSpotReclaim) || !slices.Contains(caps, proto.CapSyncModes) {
		t.Fatalf("Hello with a sync-mode shim lacking reclaim: %v", caps)
	}
	r = &Runner{shimSyncModes: false, shimReclaim: shimKnowsReclaim(shim)}
	if caps := r.capabilities(); !slices.Contains(caps, proto.CapSpotReclaim) || slices.Contains(caps, proto.CapSyncModes) {
		t.Fatalf("Hello with a reclaim shim, sync gate kept apart: %v", caps)
	}
	// Each capability follows its own gate, disk scaling included.
	for _, c := range []struct {
		modes, reclaim, disk bool
		want                 []string
	}{
		{false, false, false, []string{proto.CapDiff, proto.CapGitCredentials, proto.CapProvisionalNet, proto.CapNonPreemptible}},
		{false, false, true, []string{proto.CapDiff, proto.CapGitCredentials, proto.CapProvisionalNet, proto.CapNonPreemptible, proto.CapDiskScale}},
		{false, true, true, []string{proto.CapDiff, proto.CapGitCredentials, proto.CapProvisionalNet, proto.CapNonPreemptible, proto.CapSpotReclaim, proto.CapDiskScale}},
		{true, true, true, []string{proto.CapDiff, proto.CapGitCredentials, proto.CapProvisionalNet, proto.CapNonPreemptible, proto.CapSyncModes, proto.CapSpotReclaim, proto.CapDiskScale}},
		{true, false, false, []string{proto.CapDiff, proto.CapGitCredentials, proto.CapProvisionalNet, proto.CapNonPreemptible, proto.CapSyncModes}},
	} {
		r := &Runner{shimSyncModes: c.modes, shimReclaim: c.reclaim, disk: diskScale{on: c.disk}}
		if got := r.capabilities(); !slices.Equal(got, c.want) {
			t.Fatalf("modes %v, reclaim %v, disk scaling %v: %v, want %v", c.modes, c.reclaim, c.disk, got, c.want)
		}
		// Every runner of this build exempts non-preemptible placements
		// from its disk guard, whatever its shim.
		if got := r.capabilities(); !slices.Contains(got, proto.CapNonPreemptible) {
			t.Fatalf("modes %v, reclaim %v, disk scaling %v: %v lacks %s", c.modes, c.reclaim, c.disk, got, proto.CapNonPreemptible)
		}
	}
}

// The kill instant is fixed at the first notice's receipt: a retried notice,
// a reconnect or a runner restart (re-reading the stored value) never moves
// it, and it is exactly 15s after that receipt.
func TestReclaimKillAtIsFixedAtFirstReceipt(t *testing.T) {
	first := time.UnixMilli(1_800_000_000_000)
	at := reclaimKillAt(0, first)
	if want := first.Add(15 * time.Second).UnixMilli(); at != want {
		t.Fatalf("first deadline %d, want %d", at, want)
	}
	for _, later := range []time.Duration{time.Second, 14 * time.Second, time.Hour} {
		if got := reclaimKillAt(at, first.Add(later)); got != at {
			t.Fatalf("notice %v later moved the deadline to %d", later, got)
		}
	}
}
