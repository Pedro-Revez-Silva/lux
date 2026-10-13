package server

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

const noDiskScale = "host's lux-runner does not scale disk (no --disk-headroom)"

// shapedSpotHost adds host h to shaped pool "shaped" advertising the
// shape's nominal capacity and caps, with that capacity free for 2 minutes.
func shapedSpotHost(t *testing.T, s *Server, caps []string) {
	t.Helper()
	shapedPool(t, s, "shaped", &testShape)
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id, name, pool_id, state, capacity, capabilities, last_heartbeat)
		VALUES ('h', 'h', 'shaped', 'ready', $1, $2, now())`, shapeCapacity, caps)
	s.hub.polled("h")
	seedAged(t, s, "h", freeOf(&candidateHost{Capacity: shapeCapacity}), 2*time.Minute)
}

func shapedSpotSpec() spec.RunSpec {
	sp := baseSpec("shaped", spec.Resources{Size: "m-2-arm64"})
	sp.Spot = true
	return sp
}

// A shaped pool's spot Run is refused by a host whose runner reclaims spot
// work but does not scale disk: the dry run names it as a constraint
// blocker, and the scheduler leaves the Run waiting. Once the runner
// advertises disk-scale, both start it there.
func TestShapedSpotRunNeedsDiskScale(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	shapedSpotHost(t, s, []string{proto.CapDiff, proto.CapSpotReclaim})

	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: shapedSpotSpec()})
	if err != nil {
		t.Fatal(err)
	}
	want := []AdmissionBlocker{{Code: blockConstraint, Hosts: 1, Reason: noDiskScale}}
	if out.Body.WouldStartNow || !reflect.DeepEqual(out.Body.Blockers, want) {
		t.Fatalf("dry run %+v, want blockers %+v", out.Body, want)
	}

	run, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: shapedSpotSpec()})
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = $1`, []any{run.Body.ID}, &state, &reason)
	if state == StateScheduled || reason != "waiting for spot capacity: 1 host fails its constraints" {
		t.Fatalf("shaped spot Run %s/%q, want waiting on the host's constraints", state, reason)
	}

	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapDiff, proto.CapSpotReclaim, proto.CapDiskScale})
	out, err = s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: shapedSpotSpec()})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.WouldStartNow || out.Body.EligibleHost != "h" || len(out.Body.Blockers) != 0 {
		t.Fatalf("dry run with disk-scale %+v, want wouldStartNow on h", out.Body)
	}
	execSQL(t, s, ctx, `UPDATE runs SET updated_at = now() WHERE id = $1`, run.Body.ID)
	schedule(t, s)
	if got := queryOne[string](t, s, `SELECT host_id FROM placements WHERE run_id = $1`, run.Body.ID); got != "h" {
		t.Fatalf("shaped spot Run placed on %q, want h", got)
	}
}

// A queued shaped normal Run chosen onto a host that does not scale disk
// cannot use that host, so it does not hold the host's spare capacity
// back from a spot Run of an unshaped pool there.
func TestSpotDemandHonoursShape(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	shapedPool(t, s, "shaped", &testShape)
	shapedPool(t, s, "plain", nil)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, capabilities, last_heartbeat)
		VALUES ('h', 'h', 'plain', 'ready', $1, $2, now())`, shapeCapacity, []string{proto.CapDiff, proto.CapSpotReclaim})
	s.hub.polled("h")
	seedAged(t, s, "h", freeOf(&candidateHost{Capacity: shapeCapacity}), 2*time.Minute)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, place_on, spec, state) VALUES ('normal', 't1', 'shaped', 'h', $1, 'submitted')`,
		baseSpec("shaped", sized(2)))
	sp := baseSpec("plain", spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(gib)})
	sp.Spot = true
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.WouldStartNow || out.Body.EligibleHost != "h" {
		t.Fatalf("dry run %+v, want wouldStartNow on h: the shaped normal Run cannot use it", out.Body)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = $1 WHERE id = 'h'`, []string{proto.CapDiff, proto.CapSpotReclaim, proto.CapDiskScale})
	out, err = s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if want := []AdmissionBlocker{{Code: blockNormalPending, Hosts: 1}}; out.Body.WouldStartNow || !reflect.DeepEqual(out.Body.Blockers, want) {
		t.Fatalf("dry run %+v, want %+v once the shaped Run could use h", out.Body, want)
	}
}

// Reclaiming for a shaped normal Run happens only on a host it could
// use (disk-scale, the shape advertised); the victims chosen there leave
// room for it under the shape gate.
func TestReclaimPlanShaped(t *testing.T) {
	sh := &PoolShape{Family: "m", Arch: "arm64", CPUs: 4, Memory: 40, Disk: 40, MinCPUs: 0.5, DefaultCPUs: 1}
	r := normalRun(2)
	r.Shape = sh
	host := func(caps ...string) *candidateHost {
		h := planHost("h", 2, spotOn("a", "t1", 1), spotOn("b", "t1", 1))
		h.Caps = caps
		return h
	}
	if h, v := reclaimPlan(r, []*candidateHost{host(proto.CapSpotReclaim)}); h != nil {
		t.Fatalf("reclaim on a host without disk-scale: %s %v", h.ID, victimIDs(v))
	}
	h, v := reclaimPlan(r, []*candidateHost{host(proto.CapSpotReclaim, proto.CapDiskScale)})
	if h == nil || !slices.Equal(victimIDs(v), []string{"b", "a"}) {
		t.Fatalf("victims %v, want [b a]", victimIDs(v))
	}
	after := h.clone()
	for _, sp := range v {
		after.remove(sp)
	}
	if b := hostFit(r, after); len(b) != 0 {
		t.Fatalf("shaped Run without its victims: %+v", b)
	}
}
