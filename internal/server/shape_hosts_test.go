package server

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// shapeCapacity is testShape's nominal host as a runner advertises it.
var shapeCapacity = proto.Capacity{CPUs: 8, Memory: 32 << 30, Disk: 160 << 30, Runs: 16, DiskHeadroom: 10 << 30, FSCapacity: 155 << 30}

// The scheduler places a shaped pool's Run only on a host advertising
// disk-scale and the shape's nominal capacity: an old agent, or one
// advertising its hardware, is skipped with the reason saying so; once a
// host advertises the shape the Run is placed. An unshaped pool's Run goes
// to the same old agent as before.
func TestSchedulerShapedRunsNeedTheShape(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	shapedPool(t, s, "shaped", &testShape)
	shapedPool(t, s, "plain", nil)
	hardware := proto.Capacity{CPUs: 16, Memory: 64 << 30, Disk: 200 << 30, Runs: 16}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, capabilities, last_heartbeat) VALUES
		('old', 'old', 'shaped', 'ready', $1, ARRAY['diff', 'sync-modes'], now()),
		('big', 'big', 'shaped', 'ready', $1, ARRAY['diff', 'disk-scale'], now()),
		('plainold', 'plainold', 'plain', 'ready', $1, ARRAY['diff'], now())`, hardware)
	for _, h := range []string{"old", "big", "plainold"} {
		s.hub.polled(h)
	}
	shaped, err := submitSized(s, "", "shaped", spec.Resources{Size: "m-2-arm64"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := submitSized(s, "", "plain", spec.Resources{CPUs: 1, Memory: spec.Bytes(gib), Disk: spec.Bytes(gib)})
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	var state, reason string
	systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = $1`, []any{shaped.Body.ID}, &state, &reason)
	want := "waiting for capacity: 2 hosts in its pool do not advertise its pool's shape (cpus 8, memory 32.0 GiB, disk 160.0 GiB) with disk scaling"
	if state == StateScheduled || reason != want {
		t.Fatalf("shaped Run %s/%q, want waiting with %q", state, reason, want)
	}
	if got := storedState(t, s, plain.Body.ID); got != StateScheduled {
		t.Fatalf("unshaped Run on an old agent: %s, want scheduled", got)
	}
	// The larger host restarts advertising the nominal shape.
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = $1 WHERE id = 'big'`, shapeCapacity)
	execSQL(t, s, ctx, `UPDATE runs SET updated_at = now() WHERE id = $1`, shaped.Body.ID)
	schedule(t, s)
	if got := queryOne[string](t, s, `SELECT host_id FROM placements WHERE run_id = $1`, shaped.Body.ID); got != "big" {
		t.Fatalf("shaped Run placed on %q, want big", got)
	}
}

// The planner reserves a shaped pool's waiting Run on no ready host that
// lacks the shape, and plans its new host as launched for the shape (the
// nominal capacity with disk-scale), not as older registrations
// advertised their hardware.
func TestCapacityPlanShapedPool(t *testing.T) {
	s, pl, p := planningFixture(t, 0)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE pools SET shape = $1 WHERE id = 'pool1'`, testShape)
	pl.Shape = &testShape
	res := sized(2)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES ('r0', 't1', 'pool1', $1, 'provisioning')`,
		map[string]any{"resources": res, "placement": map[string]any{"pool": "burst"}})
	// A ready old agent, and history of hosts that advertised hardware too
	// small for a nominal reservation.
	observePlanningHost(t, s, "old", "ready", proto.Capacity{CPUs: 64, Memory: 256 << 30, Disk: 1 << 40, Runs: 16}, map[string]string{})
	observePlanningHost(t, s, "small", "terminated", proto.Capacity{CPUs: 1, Memory: 1 << 30, Runs: 16}, map[string]string{})
	planningTick(t, s, pl, p, false)
	if p.calls != 1 {
		t.Fatalf("launched %d hosts, want 1: the old agent cannot take a shaped Run, and the new host is the shape", p.calls)
	}
}

// A host luxd launches for a shaped pool starts its runner with the
// shape's nominal cpus, memory and disk and the pool's disk headroom (10
// GiB unless set); an unshaped pool's launch env is exactly as before.
func TestLaunchEnvForShapedPools(t *testing.T) {
	base := func(env map[string]string) map[string]string {
		out := map[string]string{}
		for _, k := range []string{"LUX_URL", "LUX_HOST_TOKEN", "LUX_HOST_NAME"} {
			out[k] = env[k]
		}
		return out
	}
	headroom := spec.Bytes(20 << 30)
	withHeadroom := testShape
	withHeadroom.DiskHeadroom = &headroom
	for _, c := range []struct {
		name  string
		shape *PoolShape
		extra map[string]string
	}{
		{"unshaped", nil, nil},
		{"shaped, default headroom", &testShape, map[string]string{"LUX_RUNNER_CPUS": "8", "LUX_RUNNER_MEMORY": "34359738368",
			"LUX_RUNNER_DISK": "171798691840", "LUX_RUNNER_DISK_HEADROOM": "10737418240"}},
		{"shaped, its own headroom", &withHeadroom, map[string]string{"LUX_RUNNER_CPUS": "8", "LUX_RUNNER_MEMORY": "34359738368",
			"LUX_RUNNER_DISK": "171798691840", "LUX_RUNNER_DISK_HEADROOM": "21474836480"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			s.cfg.RunnerURL = "http://10.0.1.10:7070"
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, shape) VALUES ('pool1', 'burst', 'ec2', $1)`, c.shape)
			prov := &fakeLaunchProvider{}
			if err := s.launch(ctx, prov, poolRow{ID: "pool1", Name: "burst", Provider: "ec2", Shape: c.shape}, nil); err != nil {
				t.Fatal(err)
			}
			want := base(prov.env)
			maps.Copy(want, c.extra)
			if !maps.Equal(prov.env, want) {
				t.Errorf("launch env %v, want %v", prov.env, want)
			}
			if want["LUX_URL"] != "http://10.0.1.10:7070" || want["LUX_HOST_TOKEN"] == "" || want["LUX_HOST_NAME"] == "" {
				t.Errorf("launch env lacks the runner's connection: %v", prov.env)
			}
		})
	}
}

// The provisioner reads each pool's shape with the rest of its row: a
// shaped pool's warm host is launched with the shape's env, an unshaped
// pool's without it.
func TestProvisionerLaunchesShapedPoolsForTheShape(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template, warm_hosts, max_hosts, shape) VALUES
		('pool1', 'shaped', 'ec2', '{"version":1}', 1, 1, $1), ('pool2', 'plain', 'ec2', '{"version":1}', 1, 1, NULL)`, testShape)
	p := &envRecorder{}
	s.cfg.Providers = map[string]Provider{"ec2": p}
	if err := s.provision(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.envs) != 2 {
		t.Fatalf("%d launches, want one per pool", len(p.envs))
	}
	for pool, env := range p.envs {
		_, shaped := env["LUX_RUNNER_DISK_HEADROOM"]
		if shaped != (pool == "pool1") || shaped && env["LUX_RUNNER_DISK"] != "171798691840" {
			t.Errorf("pool %s: launch env %v", pool, env)
		}
	}
}

// envRecorder is a provider recording each launch's env by its pool tag.
type envRecorder struct {
	planningProvider
	envs map[string]map[string]string
}

func (p *envRecorder) Launch(ctx context.Context, tmpl json.RawMessage, tags, env map[string]string) (Launched, error) {
	if p.envs == nil {
		p.envs = map[string]map[string]string{}
	}
	p.envs[tags[tagPoolID]] = env
	return p.planningProvider.Launch(ctx, tmpl, tags, env)
}
