package server

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// packFixture is a shaped static pool "shaped" (testShape, pack by
// default) with a connected host advertising the shape for each of hosts.
func packFixture(t *testing.T, hosts ...string) *Server {
	t.Helper()
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	shapedPool(t, s, "shaped", &testShape)
	for _, h := range hosts {
		shapedHost(t, s, h, "shaped")
	}
	return s
}

// shapedHost adds a ready host in pool advertising testShape with
// disk-scale, connected to this luxd.
func shapedHost(t *testing.T, s *Server, id, pool string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id, name, pool_id, state, capacity, capabilities, last_heartbeat)
		VALUES ($1, $1, $2, 'ready', $3, ARRAY['diff', 'sync-modes', 'disk-scale'], now())`, id, pool, shapeCapacity)
	s.hub.polled(id)
}

// occupy puts a running Run of res on host: a live placement the
// scheduler counts as reserved there.
func occupy(t *testing.T, s *Server, host string, res spec.Resources) {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("busy-%s-%d", host, queryOne[int](t, s, `SELECT count(*) FROM runs`))
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ($1, 't1', '{}', 'running', 1)`, id)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		VALUES ('p-'||$1, 't1', $1, $2, 1, 'running', $3)`, id, host, res)
}

// submitSizes submits one Run per size to pool, in order, and returns their ids.
func submitSizes(t *testing.T, s *Server, pool string, sizes ...string) []string {
	t.Helper()
	var ids []string
	for _, sz := range sizes {
		out, err := submitSized(s, "", pool, spec.Resources{Size: sz})
		if err != nil {
			t.Fatalf("submit %s: %v", sz, err)
		}
		ids = append(ids, out.Body.ID)
	}
	return ids
}

// runHosts is each Run's host ("" while it has none).
func runHosts(t *testing.T, s *Server, ids []string) []string {
	t.Helper()
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i], _, _ = placedOn(t, s, id)
	}
	return out
}

// reservedOn is the cpus, memory and disk a host's live placements reserve.
func reservedOn(t *testing.T, s *Server, host string) (cpus float64, memory, disk int64) {
	t.Helper()
	systemScan(t, s, `SELECT coalesce(sum((resources->>'cpus')::float8), 0), coalesce(sum((resources->>'memory')::bigint), 0),
			coalesce(sum((resources->>'disk')::bigint), 0)
		FROM placements WHERE host_id = $1 AND state IN `+livePlacementStates, []any{host}, &cpus, &memory, &disk)
	return cpus, memory, disk
}

// Runs submitted together to a pack pool fill one host before the next:
// four quarters take all of ha, the fifth goes to hb. Both hosts start
// empty, so the first goes to the lower id.
func TestPackCompetingSubmissions(t *testing.T) {
	s := packFixture(t, "hb", "ha")
	ids := submitSizes(t, s, "shaped", "m-2-arm64", "m-2-arm64", "m-2-arm64", "m-2-arm64", "m-2-arm64")
	schedule(t, s)
	if got, want := runHosts(t, s, ids), []string{"ha", "ha", "ha", "ha", "hb"}; !slices.Equal(got, want) {
		t.Fatalf("hosts %v, want %v", got, want)
	}
	if cpus, mem, disk := reservedOn(t, s, "ha"); cpus != 8 || mem != shapeCapacity.Memory || disk != shapeCapacity.Disk {
		t.Errorf("ha reserves %g cpus, %d memory, %d disk; want the whole host", cpus, mem, disk)
	}
}

// Fractions of a host sum to it exactly: 1/2 + 1/4 + 1/4, and 1/2 + 1/4 +
// 1/8 + 1/16 + 1/16, each fill ha to its capacity in every dimension, and
// the next Run, however small, goes to hb.
func TestPackFillsAHostExactly(t *testing.T) {
	for _, sizes := range [][]string{
		{"m-4-arm64", "m-2-arm64", "m-2-arm64"},
		{"m-4-arm64", "m-2-arm64", "m-1-arm64", "m-05-arm64", "m-05-arm64"},
	} {
		t.Run(fmt.Sprint(len(sizes), " sizes"), func(t *testing.T) {
			s := packFixture(t, "ha", "hb")
			ids := submitSizes(t, s, "shaped", append(sizes, "m-05-arm64")...)
			schedule(t, s)
			got := runHosts(t, s, ids)
			want := append(slices.Repeat([]string{"ha"}, len(sizes)), "hb")
			if !slices.Equal(got, want) {
				t.Fatalf("hosts %v, want %v", got, want)
			}
			if cpus, mem, disk := reservedOn(t, s, "ha"); cpus != shapeCapacity.CPUs || mem != shapeCapacity.Memory || disk != shapeCapacity.Disk {
				t.Errorf("ha reserves %g cpus, %d memory, %d disk; want exactly %+v", cpus, mem, disk, shapeCapacity)
			}
		})
	}
}

// Two hosts with a quarter free each cannot take a half: in a static pool
// the half waits saying every resource is short on both; in a provisioned
// pool it asks for a host. A quarter still fits, on the lower id.
func TestPackScatteredHoles(t *testing.T) {
	for _, provider := range []string{"static", "ec2"} {
		t.Run(provider, func(t *testing.T) {
			s := packFixture(t, "ha", "hb")
			execSQL(t, s, context.Background(), `UPDATE pools SET provider = $1 WHERE id = 'shaped'`, provider)
			for _, h := range []string{"ha", "hb"} {
				occupy(t, s, h, sized(4))
				occupy(t, s, h, sized(2))
			}
			ids := submitSizes(t, s, "shaped", "m-4-arm64", "m-2-arm64")
			schedule(t, s)
			if got := runHosts(t, s, ids); !slices.Equal(got, []string{"", "ha"}) {
				t.Fatalf("hosts %v, want the half unplaced and the quarter on ha", got)
			}
			_, state, reason := placedOn(t, s, ids[0])
			wantState := map[string]string{"static": StateSubmitted, "ec2": StateProvisioning}[provider]
			const want = "waiting for capacity: 2 hosts in its pool lack cpus (requested 4), 2 lack memory (requested 16.0 GiB), 2 lack disk (requested 80.0 GiB)"
			if state != wantState || reason != want {
				t.Errorf("half: %s %q, want %s %q", state, reason, wantState, want)
			}
		})
	}
}

// pack measures how allocated a host is by its reserved share, never its
// Run count: hb with one half beats ha with two sixteenths.
func TestPackByFractionNotRunCount(t *testing.T) {
	s := packFixture(t, "ha", "hb")
	occupy(t, s, "ha", sized(0.5))
	occupy(t, s, "ha", sized(0.5))
	occupy(t, s, "hb", sized(4))
	ids := submitSizes(t, s, "shaped", "m-1-arm64")
	schedule(t, s)
	if got := runHosts(t, s, ids); got[0] != "hb" {
		t.Fatalf("placed on %q, want hb (half allocated) over ha (two Runs, an eighth)", got[0])
	}
}

// spotOnHost puts a running spot Run of res on host as placement id.
func spotOnHost(t *testing.T, s *Server, id, host string, res spec.Resources) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ($1, 't1', '{"spot": true}', 'running', 1)`, "run-"+id)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		VALUES ($1, 't1', $2, $3, 1, 'running', $4)`, id, "run-"+id, host, res)
}

// reclaimFor is the normal Run a placement is claimed for ("": none).
func reclaimFor(t *testing.T, s *Server, placement string) string {
	t.Helper()
	return queryOne[string](t, s, `SELECT coalesce(reclaim_for, '') FROM placements WHERE id = $1`, placement)
}

// pack ranks hosts by normal work only: a spot half on ha is spare
// capacity, so a normal quarter on hb makes hb the fuller host. The
// normal Run goes there, leaving ha spot-only (idle to scale-down); a
// whole-host Run, which then fits only ha, reclaims its spot work.
func TestPackIgnoresSpotAllocation(t *testing.T) {
	s := packFixture(t, "ha", "hb")
	spotOnHost(t, s, "p-spot", "ha", sized(4))
	occupy(t, s, "hb", sized(2))
	ids := submitSizes(t, s, "shaped", "m-1-arm64")
	schedule(t, s)
	if got := runHosts(t, s, ids); got[0] != "hb" {
		t.Fatalf("placed on %q, want hb: ha holds only spot work", got[0])
	}
	whole := submitSizes(t, s, "shaped", "m-8-arm64")
	schedule(t, s)
	if _, state, reason := placedOn(t, s, whole[0]); state != StateSubmitted || reason != reclaimWaitReason {
		t.Fatalf("whole-host Run %s %q, want it waiting for the reclaimed spot", state, reason)
	}
	if got := reclaimFor(t, s, "p-spot"); got != whole[0] {
		t.Fatalf("ha's spot claimed for %q, want the whole-host Run %s", got, whole[0])
	}
}

// Among hosts with the same normal allocation, pack shares the one already
// holding spot work: the quarter joins hb's spot half, so ha stays empty
// and a whole-host Run takes it with no reclaim.
func TestPackSharesSpotHostOverEmpty(t *testing.T) {
	s := packFixture(t, "ha", "hb")
	execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities = capabilities || $1::text[]`, []string{proto.CapSpotReclaim})
	spotOnHost(t, s, "p-spot", "hb", sized(4))
	quarter := submitSizes(t, s, "shaped", "m-2-arm64")
	schedule(t, s)
	if got := runHosts(t, s, quarter); got[0] != "hb" {
		t.Fatalf("quarter on %q, want hb beside the spot work", got[0])
	}
	whole := submitSizes(t, s, "shaped", "m-8-arm64")
	schedule(t, s)
	if got, state, reason := placedOn(t, s, whole[0]); got != "ha" {
		t.Fatalf("whole-host Run on %q (%s %q), want the empty ha", got, state, reason)
	}
	if got := reclaimFor(t, s, "p-spot"); got != "" {
		t.Fatalf("spot claimed for %q, want no reclaim", got)
	}
}

// allocated is the largest of reserved/capacity over cpus, memory and
// disk; a resource a host does not limit counts nothing.
func TestCandidateHostAllocated(t *testing.T) {
	for _, c := range []struct {
		name string
		h    candidateHost
		want float64
	}{
		{"empty", candidateHost{Capacity: shapeCapacity}, 0},
		{"memory tightest", candidateHost{Capacity: proto.Capacity{CPUs: 8, Memory: 8 << 30, Disk: 80 << 30},
			UsedCPUs: 2, UsedMem: 6 << 30, UsedDisk: 10 << 30}, 0.75},
		{"disk tightest", candidateHost{Capacity: proto.Capacity{CPUs: 8, Memory: 8 << 30, Disk: 80 << 30},
			UsedCPUs: 2, UsedMem: 2 << 30, UsedDisk: 40 << 30}, 0.5},
		{"no disk limit", candidateHost{Capacity: proto.Capacity{CPUs: 4, Memory: 8 << 30},
			UsedCPUs: 1, UsedMem: 1 << 30, UsedDisk: 1 << 40, UsedRuns: 30}, 0.25},
		{"spot work counts free", candidateHost{Capacity: proto.Capacity{CPUs: 8, Memory: 32 << 30, Disk: 160 << 30},
			UsedCPUs: 4, UsedMem: 16 << 30, UsedDisk: 80 << 30, UsedRuns: 2,
			Spots: []spotPlacement{{Res: spec.Resources{CPUs: 2, Memory: 8 << 30, Disk: 40 << 30}}}}, 0.25},
		{"claimed spot work counts", candidateHost{Capacity: proto.Capacity{CPUs: 8, Memory: 32 << 30, Disk: 160 << 30},
			UsedCPUs: 4, UsedMem: 16 << 30, UsedDisk: 80 << 30, UsedRuns: 2,
			Spots: []spotPlacement{{Res: spec.Resources{CPUs: 2, Memory: 8 << 30, Disk: 40 << 30}, ReclaimFor: "n"}}}, 0.5},
	} {
		if got := c.h.allocated(); got != c.want {
			t.Errorf("%s: allocated %g, want %g", c.name, got, c.want)
		}
	}
}

// pickHostWith runs pickHost for r over hosts in a transaction.
func pickHostWith(t *testing.T, s *Server, r pendingRun, hosts []*candidateHost) (*candidateHost, string) {
	t.Helper()
	var h *candidateHost
	var wait string
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		var err error
		h, wait, err = s.pickHost(context.Background(), tx, r, hosts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return h, wait
}

// packHosts are shaped candidate hosts with these ids, empty, in pool.
func packHosts(ids ...string) []*candidateHost {
	var out []*candidateHost
	for _, id := range ids {
		out = append(out, &candidateHost{ID: id, PoolID: "pool", Connected: true, Capacity: shapeCapacity,
			Caps: []string{proto.CapDiskScale}, Labels: map[string]string{}})
	}
	return out
}

func packRun() pendingRun {
	r, _ := fitFixture()
	r.Shape, r.Pack = &testShape, true
	r.Spec.Resources = sized(1)
	r.Spec.Image.Ref = "alpine"
	return r
}

// Among hosts allocated alike, pack prefers the cached image, mirror or
// preferred label, then the lowest id; a more allocated host wins over a
// cached one. The order the hosts come in never matters.
func TestPackTieBreaks(t *testing.T) {
	s := testServer(t)
	for _, c := range []struct {
		name string
		set  func(hs map[string]*candidateHost)
		want string
	}{
		{"all alike: lowest id", func(map[string]*candidateHost) {}, "ha"},
		{"alike, image cached on hc", func(hs map[string]*candidateHost) { hs["hc"].Images = []string{"alpine"} }, "hc"},
		{"alike, mirror cached on hb", func(hs map[string]*candidateHost) { hs["hb"].Mirrors = []string{"https://example.com/app.git"} }, "hb"},
		{"alike, preferred label on hc", func(hs map[string]*candidateHost) { hs["hc"].Labels["zone"] = "a" }, "hc"},
		{"image on hc, hb more allocated", func(hs map[string]*candidateHost) {
			hs["hc"].Images = []string{"alpine"}
			hs["hb"].UsedCPUs, hs["hb"].UsedMem, hs["hb"].UsedDisk = 0.5, 2<<30, 10<<30
		}, "hb"},
		{"image on ha and hc: lowest id of those", func(hs map[string]*candidateHost) {
			hs["ha"].Images, hs["hc"].Images = []string{"alpine"}, []string{"alpine"}
		}, "ha"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hosts := packHosts("ha", "hb", "hc")
			byID := map[string]*candidateHost{}
			for _, h := range hosts {
				byID[h.ID] = h
			}
			c.set(byID)
			r := packRun()
			r.Spec.Git = &spec.Git{Repositories: []spec.Repository{{Name: "app", URL: "https://example.com/app.git"}}}
			r.Spec.Placement.Prefers = map[string]string{"zone": "a"}
			for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {2, 0, 1}} {
				in := []*candidateHost{hosts[order[0]], hosts[order[1]], hosts[order[2]]}
				if h, wait := pickHostWith(t, s, r, in); h == nil || h.ID != c.want {
					t.Errorf("order %v: picked %v (%q), want %s", order, h, wait, c.want)
				}
			}
		})
	}
}

// A host the Run must avoid comes last however packed it is, and a
// snapshot only a less packed host holds (not uploaded) sends the Run
// there; once uploaded, pack chooses by allocation again, the snapshot's
// host winning only among hosts allocated alike.
func TestPackAvoidAndLocalSnapshot(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('hb', 'hb', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'resuming')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p', 't1', 'r', 'hb', 1, 'exited')`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, host_id, manifest, available, uploaded, host_copy)
		VALUES ('snap', 't1', 'r', 'p', 1, 'hb', '{}', true, false, true)`)
	hosts := packHosts("ha", "hb")
	hosts[0].UsedCPUs, hosts[0].UsedMem, hosts[0].UsedDisk = 4, 16<<30, 80<<30
	r := packRun()
	r.ID = "r"
	r.AvoidHost = "ha"
	if h, _ := pickHostWith(t, s, r, hosts); h == nil || h.ID != "hb" {
		t.Fatalf("avoiding the packed ha: picked %v", h)
	}
	r.AvoidHost = ""
	r.SnapshotID = new("snap")
	if h, _ := pickHostWith(t, s, r, hosts); h == nil || h.ID != "hb" {
		t.Fatalf("snapshot only on the emptier hb: picked %v", h)
	}
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snap'`)
	if h, _ := pickHostWith(t, s, r, hosts); h == nil || h.ID != "ha" {
		t.Fatalf("snapshot uploaded: picked %v, want the packed ha", h)
	}
	hosts[0].UsedCPUs, hosts[0].UsedMem, hosts[0].UsedDisk = 0, 0, 0
	for _, in := range [][]*candidateHost{hosts, {hosts[1], hosts[0]}} {
		if h, _ := pickHostWith(t, s, r, in); h == nil || h.ID != "hb" {
			t.Fatalf("uploaded snapshot copy on hb, hosts alike: picked %v, want hb over the lower id", h)
		}
	}
}

// An unshaped pack pool counts a resource a host does not advertise as
// empty: a host advertising no limits ranks emptiest, so a half-used
// limited host is chosen over it whatever the order.
func TestPackUnlimitedHostRanksEmptiest(t *testing.T) {
	s := testServer(t)
	limited := &candidateHost{ID: "hb", PoolID: "pool", Connected: true, Labels: map[string]string{},
		Capacity: proto.Capacity{CPUs: 8, Memory: 32 << 30, Disk: 160 << 30}, UsedCPUs: 4, UsedMem: 16 << 30, UsedDisk: 80 << 30, UsedRuns: 1}
	unlimited := &candidateHost{ID: "ha", PoolID: "pool", Connected: true, Labels: map[string]string{}}
	r := packRun()
	r.Shape = nil
	r.Spec.Resources = spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(gib)}
	for _, in := range [][]*candidateHost{{unlimited, limited}, {limited, unlimited}} {
		if h, wait := pickHostWith(t, s, r, in); h == nil || h.ID != "hb" {
			t.Fatalf("picked %v (%q), want the half-used hb over the unlimited ha", h, wait)
		}
	}
}

// Float shares that differ only by rounding error tie under pack:
// 0.1+0.2 cpus against 0.3 on capacity 1, and affinity (the image cached
// on hb) decides, not the lower id or the last bit.
func TestPackFloatSharesTie(t *testing.T) {
	s := testServer(t)
	tenth, fifth := 0.1, 0.2
	ha := &candidateHost{ID: "ha", PoolID: "pool", Connected: true, Labels: map[string]string{},
		Capacity: proto.Capacity{CPUs: 1}, UsedCPUs: tenth + fifth}
	hb := &candidateHost{ID: "hb", PoolID: "pool", Connected: true, Labels: map[string]string{},
		Capacity: proto.Capacity{CPUs: 1}, UsedCPUs: 0.3, Images: []string{"alpine"}}
	if ha.UsedCPUs == hb.UsedCPUs {
		t.Fatal("0.1+0.2 equals 0.3 here: the case tests nothing")
	}
	r := packRun()
	r.Shape = nil
	r.Spec.Resources = spec.Resources{CPUs: 0.1}
	for _, in := range [][]*candidateHost{{ha, hb}, {hb, ha}} {
		if h, wait := pickHostWith(t, s, r, in); h == nil || h.ID != "hb" {
			t.Fatalf("picked %v (%q), want hb by its cached image", h, wait)
		}
	}
}

// pack considers only hosts the Run may use: a stale host (no heartbeat
// within the lease), a host not connected to this luxd, and a host in the
// pool not advertising the shape are passed over however packed they are.
func TestPackSkipsHostsItCannotUse(t *testing.T) {
	s := packFixture(t, "ha", "hb", "hc", "hd")
	ctx := context.Background()
	for _, h := range []string{"ha", "hb", "hc"} {
		occupy(t, s, h, sized(4))
	}
	occupy(t, s, "hd", sized(1))
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'ha'`)
	// hb's runner last polled this luxd long ago: another luxd may hold it.
	s.hub.mu.Lock()
	s.hub.polls["hb"] = time.Now().Add(-time.Hour)
	s.hub.mu.Unlock()
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY['diff', 'sync-modes'] WHERE id = 'hc'`)
	ids := submitSizes(t, s, "shaped", "m-1-arm64")
	schedule(t, s)
	if got := runHosts(t, s, ids); got[0] != "hd" {
		t.Fatalf("placed on %q, want hd, the one usable host", got[0])
	}
}

// A shaped Run in a pool mixing shaped and unshaped hosts packs onto the
// shaped hosts only; the busier host without the shape is ignored.
func TestPackMixedHosts(t *testing.T) {
	s := packFixture(t, "hb", "hc")
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, capabilities, last_heartbeat)
		VALUES ('ha', 'ha', 'shaped', 'ready', $1, ARRAY['diff', 'sync-modes'], now())`,
		proto.Capacity{CPUs: 16, Memory: 64 << 30, Disk: 200 << 30, Runs: 16})
	s.hub.polled("ha")
	occupy(t, s, "ha", sized(8))
	occupy(t, s, "hc", sized(2))
	ids := submitSizes(t, s, "shaped", "m-2-arm64", "m-2-arm64")
	schedule(t, s)
	if got := runHosts(t, s, ids); !slices.Equal(got, []string{"hc", "hc"}) {
		t.Fatalf("hosts %v, want both on hc", got)
	}
}

// Placement defaults: an unshaped pool spreads, a shaped pool packs; each
// follows its stored placement when set (an unshaped pool may pack, a
// shaped one spread).
func TestPlacementDefaultsAndOverrides(t *testing.T) {
	for _, c := range []struct {
		name      string
		shape     *PoolShape
		placement *string
		want      []string
	}{
		{"unshaped default spreads", nil, nil, []string{"ha", "hb"}},
		{"unshaped pack packs", nil, new("pack"), []string{"ha", "ha"}},
		{"shaped default packs", &testShape, nil, []string{"ha", "ha"}},
		{"shaped spread spreads", &testShape, new("spread"), []string{"ha", "hb"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			s.cfg.LeaseDuration = time.Minute
			shapedPool(t, s, "pool", c.shape)
			execSQL(t, s, context.Background(), `UPDATE pools SET placement = $1 WHERE id = 'pool'`, c.placement)
			shapedHost(t, s, "ha", "pool")
			shapedHost(t, s, "hb", "pool")
			res := spec.Resources{Size: "m-2-arm64"}
			if c.shape == nil {
				res = spec.Resources{CPUs: 2, Memory: spec.Bytes(8 * gib), Disk: spec.Bytes(40 * gib)}
			}
			var ids []string
			for range 2 {
				out, err := submitSized(s, "", "pool", res)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, out.Body.ID)
			}
			schedule(t, s)
			if got := runHosts(t, s, ids); !slices.Equal(got, c.want) {
				t.Fatalf("hosts %v, want %v", got, c.want)
			}
		})
	}
}

// spread orders by: snapshot host (+1000),
// cached image (+100 each), cached mirrors (+50), preferred labels (+10),
// minus the Run count, an avoided host last; among equal scores the host
// listed first. Allocation plays no part.
func TestSpreadUnchanged(t *testing.T) {
	s := testServer(t)
	spreadRun := func() pendingRun {
		r := packRun()
		r.Shape, r.Pack = nil, false
		r.Spec.Resources = spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(gib)}
		r.Spec.Git = &spec.Git{Repositories: []spec.Repository{{Name: "app", URL: "https://example.com/app.git"}}}
		r.Spec.Placement.Prefers = map[string]string{"zone": "a"}
		return r
	}
	for _, c := range []struct {
		name  string
		set   func(r *pendingRun, hs []*candidateHost)
		order []int // pickHost's input order, as indexes of ha, hb, hc
		want  string
	}{
		{"equal: the first listed", func(*pendingRun, []*candidateHost) {}, []int{1, 0, 2}, "hb"},
		{"equal, other order", func(*pendingRun, []*candidateHost) {}, []int{2, 1, 0}, "hc"},
		{"fewest Runs, not allocation", func(_ *pendingRun, hs []*candidateHost) {
			hs[0].UsedRuns, hs[0].UsedCPUs = 2, 2
			hs[1].UsedRuns, hs[1].UsedCPUs = 1, 6
			hs[2].UsedRuns, hs[2].UsedCPUs = 3, 0.5
		}, []int{0, 1, 2}, "hb"},
		{"image beats 99 Runs", func(_ *pendingRun, hs []*candidateHost) {
			hs[2].Images, hs[2].UsedRuns = []string{"alpine"}, 99
		}, []int{0, 1, 2}, "hc"},
		{"100 Runs undo the image", func(_ *pendingRun, hs []*candidateHost) {
			hs[2].Images, hs[2].UsedRuns = []string{"alpine"}, 100
		}, []int{2, 0, 1}, "hc"},
		{"100 Runs undo the image, listed later", func(_ *pendingRun, hs []*candidateHost) {
			hs[2].Images, hs[2].UsedRuns = []string{"alpine"}, 100
		}, []int{0, 2, 1}, "ha"},
		{"mirror beats 49 Runs", func(_ *pendingRun, hs []*candidateHost) {
			hs[1].Mirrors, hs[1].UsedRuns = []string{"https://example.com/app.git"}, 49
		}, []int{0, 1, 2}, "hb"},
		{"label beats 9 Runs", func(_ *pendingRun, hs []*candidateHost) {
			hs[1].Labels["zone"], hs[1].UsedRuns = "a", 9
		}, []int{0, 1, 2}, "hb"},
		{"avoided host last", func(r *pendingRun, hs []*candidateHost) {
			r.AvoidHost = "ha"
			hs[0].Images = []string{"alpine"}
			hs[1].UsedRuns, hs[2].UsedRuns = 50, 60
		}, []int{0, 1, 2}, "hb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hosts := packHosts("ha", "hb", "hc")
			for _, h := range hosts {
				h.Capacity, h.Caps = proto.Capacity{CPUs: 8, Memory: 32 << 30, Disk: 160 << 30}, nil
			}
			r := spreadRun()
			c.set(&r, hosts)
			in := []*candidateHost{hosts[c.order[0]], hosts[c.order[1]], hosts[c.order[2]]}
			if h, wait := pickHostWith(t, s, r, in); h == nil || h.ID != c.want {
				t.Fatalf("picked %v (%q), want %s", h, wait, c.want)
			}
		})
	}
}
