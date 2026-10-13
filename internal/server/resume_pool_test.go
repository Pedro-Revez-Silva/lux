package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// movedRun is stoppedRun in platform pool "default" (host ha: 4 cpus,
// 8 GiB), with a secret TOKEN, plus t1's static pool "big" with host hb
// (16 cpus, 64 GiB, amd64), and a larger host hc left in "default" so a
// placement there would be possible were the Run not moved. Returns the
// Run's id and big's id.
func movedRun(t *testing.T, s *Server) (string, string) {
	t.Helper()
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	id := stoppedRun(t, s, resizeBase, i64(gib))
	execSQL(t, s, ctx, `UPDATE runs SET secrets = '[{"name":"TOKEN","fingerprint":"old"}]',
		spec = jsonb_set(spec, '{secrets}', '[{"name":"TOKEN","as":"env"}]') WHERE id = $1`, id)
	s.hub.polled("ha")
	readyHost(t, s, "hb", "big", "t1", false)
	readyHost(t, s, "hc", "default", "", true)
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = '{"cpus":16,"memory":68719476736}', arch = 'amd64' WHERE id = 'hb'`)
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = '{"cpus":64,"memory":274877906944}', arch = 'amd64', registered_at = now() WHERE id = 'hc'`)
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() WHERE id IN ('ha', 'hb')`)
	return id, poolID(t, s, "big", "t1")
}

func resumeTo(s *Server, id, poolID string, res *resumeResources) (*resumeOutput, error) {
	req := &resumeRequest{Resources: res, Secrets: []spec.Secret{{Name: "TOKEN", Value: "v"}}}
	if poolID != "" {
		req.Placement = &resumeTarget{PoolID: poolID}
	}
	return s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: req})
}

// runBinding is what a move changes: pool_id, the spec's placement, state,
// secrets and resources.
type runBinding struct {
	PoolID, SpecPool, SpecPoolID, State, Secrets string
	Resources                                    spec.Resources
}

func binding(t *testing.T, s *Server, id string) runBinding {
	t.Helper()
	var b runBinding
	systemScan(t, s, `SELECT coalesce(pool_id, ''), coalesce(spec->'placement'->>'pool', ''), coalesce(spec->'placement'->>'poolId', ''), state, secrets::text
		FROM runs WHERE id = $1`, []any{id}, &b.PoolID, &b.SpecPool, &b.SpecPoolID, &b.State, &b.Secrets)
	b.Resources = storedResources(t, s, id)
	return b
}

// setSpec sets the Run's stored spec at path (a jsonb_set path, e.g.
// "{placement,requires}") to the JSON value.
func setSpec(t *testing.T, s *Server, id, path, value string) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE runs SET spec = jsonb_set(spec, $2::text::text[], $3::text::jsonb) WHERE id = $1`, id, path, value)
}

func resumeRequests(t *testing.T, s *Server, id string) int {
	t.Helper()
	var n int
	systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'resume.requested'`, []any{id}, &n)
	return n
}

// A resume naming another pool with larger resources rebinds the Run
// (pool_id, spec.placement.pool and poolId), says so in its answer, its
// resume.requested event and GET, and places it only on the new pool's
// host, recording the placement on that pool. A later resume without
// placement stays there.
func TestResumeIntoAnotherPool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id, big := movedRun(t, s)
	out, err := resumeTo(s, id, big, &resumeResources{CPUs: f64(8), Memory: bytesPtr(32 * gib)})
	if err != nil {
		t.Fatal(err)
	}
	b := binding(t, s, id)
	if b.PoolID != big || b.SpecPool != "big" || b.SpecPoolID != big || b.State != StateResuming || b.Resources.CPUs != 8 {
		t.Fatalf("after the move: %+v", b)
	}
	if pl := out.Body.Placement; pl == nil || pl.Pool != "big" || pl.PoolID != big || pl.From == nil || pl.From.ID != "default" || pl.From.Name != "default" || !pl.PlatformUnchecked {
		t.Fatalf("answer's placement %+v", out.Body.Placement)
	}
	if out.Body.Pool != "big" || out.Body.PoolID != big {
		t.Errorf("answer's pool %q %q", out.Body.Pool, out.Body.PoolID)
	}
	ev := resumeEvent(t, s, id)
	pe, _ := ev["pool"].(map[string]any)
	from, _ := pe["from"].(map[string]any)
	to, _ := pe["to"].(map[string]any)
	if from["name"] != "default" || from["id"] != "default" || to["name"] != "big" || to["id"] != big || pe["platformUnchecked"] != true {
		t.Errorf("event's pool %v", ev["pool"])
	}
	if eventRes(ev, "applied")["cpus"] != 8.0 {
		t.Errorf("event's resources %v", ev["resources"])
	}
	got, err := s.getRun(tenantCtx("t1"), &RunPath{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body.Pool != "big" || got.Body.PoolID != big || got.Body.Spec.Placement.PoolID != big {
		t.Errorf("GET: pool %q %q spec %+v", got.Body.Pool, got.Body.PoolID, got.Body.Spec.Placement)
	}

	schedule(t, s)
	if host, _, reason := placedOn(t, s, id); host != "hb" {
		t.Fatalf("placed on %q (%s), want hb", host, reason)
	}
	var onBig, onDefault int
	systemScan(t, s, `SELECT count(*) FILTER (WHERE pool_id = $1), count(*) FILTER (WHERE pool_id = 'default')
		FROM pool_events WHERE type = 'pool.placement' AND data->>'run' = $2`, []any{big, id}, &onBig, &onDefault)
	if onBig != 1 || onDefault != 0 {
		t.Errorf("pool.placement events: %d on big, %d on default; want 1, 0", onBig, onDefault)
	}

	// It stops; a plain resume stays in big.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = $1`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	out, err = resumeTo(s, id, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Placement != nil || binding(t, s, id).PoolID != big {
		t.Fatalf("plain resume: placement %+v, pool %q", out.Body.Placement, binding(t, s, id).PoolID)
	}
	if _, ok := resumeEvent(t, s, id)["pool"]; ok {
		t.Error("a resume that moved nothing has a pool in its event")
	}
	schedule(t, s)
	if host, _, reason := placedOn(t, s, id); host != "hb" {
		t.Fatalf("plain resume placed on %q (%s), want hb", host, reason)
	}
	// Naming the pool it is in moves nothing and says so.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = $1`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	out, err = resumeTo(s, id, big, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pl := out.Body.Placement; pl == nil || pl.PoolID != big || pl.From != nil {
		t.Errorf("resume naming its own pool: %+v", pl)
	}
}

// Every refusal is 422 with its code, and changes nothing: pool_id, the
// spec (placement, resources), state and secrets stay as they were, and no
// resume.requested is written.
func TestResumeIntoAnotherPoolRefusals(t *testing.T) {
	type fixture struct {
		s  *Server
		id string
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f fixture) string // the pool id to resume into
		res   *resumeResources
		code  string
		says  string // the whole message; <id> stands for the pool id setup returns
	}{
		{name: "unknown id", code: "unknown_pool", says: "no pool has id pool_nope",
			setup: func(t *testing.T, f fixture) string { return "pool_nope" }},
		{name: "another tenant's pool", code: "unknown_pool", says: "no pool has id <id>",
			setup: func(t *testing.T, f fixture) string {
				execSQL(t, f.s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
				return poolID(t, f.s, "theirs", "t2")
			}},
		{name: "retired pool", code: "unknown_pool", says: "no pool has id <id>",
			setup: func(t *testing.T, f fixture) string {
				id := poolID(t, f.s, "gone", "t1")
				execSQL(t, f.s, context.Background(), `UPDATE pools SET retired = true WHERE id = $1`, id)
				return id
			}},
		{name: "platform mismatch, from its hosts", code: "platform_mismatch",
			says: "pool big runs linux/arm64; this Run's image offers only linux/amd64",
			setup: func(t *testing.T, f fixture) string {
				execSQL(t, f.s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, f.id)
				execSQL(t, f.s, context.Background(), `UPDATE hosts SET arch = 'arm64' WHERE id = 'hb'`)
				return poolID(t, f.s, "big", "t1")
			}},
		{name: "platform mismatch, from the catalog", code: "platform_mismatch",
			says: "pool graviton runs linux/arm64; this Run's image offers only linux/amd64, linux/386",
			setup: func(t *testing.T, f fixture) string {
				execSQL(t, f.s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64,linux/386}' WHERE run_id = $1`, f.id)
				return ec2Pool(t, f.s, "graviton", `{"region":"us-east-1","instanceType":"m7g.4xlarge"}`)
			}},
		{name: "nested on static hosts without --nested", code: "pool_unfit",
			says: "pool big cannot take this Run: none of its hosts offers nested containers (lux-runner --nested)",
			setup: func(t *testing.T, f fixture) string {
				setSpec(t, f.s, f.id, "{sandbox}", `{"nestedContainers":true}`)
				return poolID(t, f.s, "big", "t1")
			}},
		{name: "nested on an ec2 template without nestedContainers", code: "pool_unfit",
			says: "pool burst cannot take this Run: it does not offer nested containers (its template has no nestedContainers, and none of its hosts runs lux-runner --nested)",
			setup: func(t *testing.T, f fixture) string {
				setSpec(t, f.s, f.id, "{sandbox}", `{"nestedContainers":true}`)
				return ec2Pool(t, f.s, "burst", `{"region":"us-east-1"}`)
			}},
		{name: "required labels no host has", code: "pool_unfit",
			says: "pool big cannot take this Run: none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, f fixture) string {
				setSpec(t, f.s, f.id, "{placement,requires}", `{"gpu":"a100"}`)
				return poolID(t, f.s, "big", "t1")
			}},
		{name: "cpus beyond its largest static host", code: "pool_unfit",
			says:  "pool big cannot take this Run: it needs 32 cpus; its largest host has 16",
			res:   &resumeResources{CPUs: f64(32)},
			setup: func(t *testing.T, f fixture) string { return poolID(t, f.s, "big", "t1") }},
		{name: "memory beyond its largest static host", code: "pool_unfit",
			says:  "pool big cannot take this Run: it needs 128.0 GiB of memory; its largest host has 64.0 GiB",
			res:   &resumeResources{Memory: bytesPtr(128 * gib)},
			setup: func(t *testing.T, f fixture) string { return poolID(t, f.s, "big", "t1") }},
		{name: "cpus beyond its template's instance type", code: "pool_unfit",
			says: "pool graviton cannot take this Run: it needs 32 cpus; its largest host has 16",
			res:  &resumeResources{CPUs: f64(32)},
			setup: func(t *testing.T, f fixture) string {
				return ec2Pool(t, f.s, "graviton", `{"region":"us-east-1","instanceType":"m7g.4xlarge"}`)
			}},
		{name: "disk beyond its largest static host", code: "pool_unfit",
			says: "pool big cannot take this Run: it needs 500.0 GiB of disk; its largest host has 100.0 GiB",
			res:  &resumeResources{Disk: spec.Bytes(500 * gib)},
			setup: func(t *testing.T, f fixture) string {
				execSQL(t, f.s, context.Background(), `UPDATE hosts SET capacity = capacity || '{"disk":107374182400}' WHERE id = 'hb'`)
				return poolID(t, f.s, "big", "t1")
			}},
		{name: "nested where only another tenant's host in a platform pool offers it", code: "pool_unfit",
			says: "pool shared cannot take this Run: none of its hosts offers nested containers (lux-runner --nested)",
			setup: func(t *testing.T, f fixture) string {
				ctx := context.Background()
				execSQL(t, f.s, ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
				readyHost(t, f.s, "h1", "shared", "", false)
				readyHost(t, f.s, "h2", "shared", "", false)
				execSQL(t, f.s, ctx, `UPDATE hosts SET tenant_id = 't2', labels = '{"nested":"true"}' WHERE id = 'h2'`)
				execSQL(t, f.s, ctx, `UPDATE hosts SET registered_at = now() WHERE id IN ('h1', 'h2')`)
				setSpec(t, f.s, f.id, "{sandbox}", `{"nestedContainers":true}`)
				return poolID(t, f.s, "shared", "")
			}},
		{name: "three hosts, each missing a different requirement: every reason", code: "pool_unfit",
			says: "pool trio cannot take this Run: it needs 24 cpus; its largest host has 32; none of its hosts has the labels placement.requires names; none of its hosts offers nested containers (lux-runner --nested)",
			res:  &resumeResources{CPUs: f64(24)},
			setup: func(t *testing.T, f fixture) string {
				ctx := context.Background()
				for _, h := range []struct{ id, capacity, labels string }{
					{"t-cpu", `{"runs":10,"cpus":16,"memory":68719476736}`, `{"gpu":"a100","nested":"true"}`},
					{"t-gpu", `{"runs":10,"cpus":32,"memory":68719476736}`, `{"nested":"true"}`},
					{"t-nest", `{"runs":10,"cpus":32,"memory":68719476736}`, `{"gpu":"a100"}`},
				} {
					readyHost(t, f.s, h.id, "trio", "t1", false)
					execSQL(t, f.s, ctx, `UPDATE hosts SET capacity = $2, labels = $3, arch = 'amd64', registered_at = now() WHERE id = $1`, h.id, h.capacity, h.labels)
				}
				setSpec(t, f.s, f.id, "{placement,requires}", `{"gpu":"a100"}`)
				setSpec(t, f.s, f.id, "{sandbox}", `{"nestedContainers":true}`)
				return poolID(t, f.s, "trio", "t1")
			}},
		{name: "required labels the ec2 template's registrations lack", code: "pool_unfit",
			says: "pool burst cannot take this Run: none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, f fixture) string {
				setSpec(t, f.s, f.id, "{placement,requires}", `{"gpu":"a100"}`)
				pool := ec2Pool(t, f.s, "burst", `{"region":"us-east-1"}`)
				ec2Host(t, f.s, "e1", pool, "terminated", time.Minute, `{"cpus":16}`, `{"zone":"a"}`)
				return pool
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, _ := movedRun(t, s)
			addCatalog(t, s)
			target := c.setup(t, fixture{s, id})
			before := binding(t, s, id)
			_, err := resumeTo(s, id, target, c.res)
			he := refused(t, err, http.StatusUnprocessableEntity, c.code, "resume")
			if want := strings.ReplaceAll(c.says, "<id>", target); he.Message != want {
				t.Errorf("message %q, want %q", he.Message, want)
			}
			if after := binding(t, s, id); after != before {
				t.Errorf("after the refusal: %+v, want %+v", after, before)
			}
			if before.State != StateStopped || before.PoolID != "default" {
				t.Errorf("fixture: %+v", before)
			}
			if events := resumeRequests(t, s, id); events != 0 {
				t.Errorf("%d resume.requested after a refusal", events)
			}
			if _, held := s.secrets.get(id); held {
				t.Error("a refused resume left its secrets held")
			}
		})
	}
}

// ec2Pool adds t1's ec2 pool name with template, no host ever launched.
func ec2Pool(t *testing.T, s *Server, name, template string) string {
	t.Helper()
	id := "pool_" + name
	execSQL(t, s, context.Background(), `INSERT INTO pools (id, tenant_id, name, provider, template, max_hosts) VALUES ($1, 't1', $2, 'ec2', $3, 4)`, id, name, template)
	return id
}

// addCatalog: m7g.4xlarge (16 vCPUs, 64 GiB, arm64) and m7i.4xlarge (16,
// 64 GiB, x86_64) in us-east-1, as the market sampler stores them.
func addCatalog(t *testing.T, s *Server) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO instance_types (provider, region, instance_type, vcpus, memory_bytes, architectures, fetched_at) VALUES
		('ec2', 'us-east-1', 'm7g.4xlarge', 16, $1, '{arm64}', now()), ('ec2', 'us-east-1', 'm7i.4xlarge', 16, $1, '{x86_64}', now())`, 64*gib)
}

// addType adds x86_64 instance type name with vcpus and 4 GiB per vCPU to
// the us-east-1 catalog.
func addType(t *testing.T, s *Server, name string, vcpus int) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO instance_types (provider, region, instance_type, vcpus, memory_bytes, architectures, fetched_at)
		VALUES ('ec2', 'us-east-1', $1, $2, $3, '{x86_64}', now())`, name, vcpus, int64(vcpus)*4*gib)
}

// probeArchPool: pool intel launches m7i.8xlarge (32 vCPUs, x86_64) and
// falls back to addCatalog's m7g.4xlarge (arm64); its only registration is
// a terminated 16-vCPU arm64 fallback.
func probeArchPool(t *testing.T, s *Server) {
	t.Helper()
	addType(t, s, "m7i.8xlarge", 32)
	pool := ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.8xlarge","fallbackInstanceTypes":["m7g.4xlarge"]}`)
	ec2Host(t, s, "e1", pool, "terminated", time.Minute, `{"cpus":16,"memory":68719476736}`, `{"arch":"arm64"}`)
}

// ec2Host records a host ec2 pool poolID launched from its current
// template, registered age ago, in state, with capacity and labels.
func ec2Host(t *testing.T, s *Server, id, poolID, state string, age time.Duration, capacity, labels string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id, name, tenant_id, pool_id, state, provider_id, provision_requested_at, registered_at, last_heartbeat, launch_template, capacity, labels, arch)
		SELECT $1, $1, tenant_id, id, $3, $1, now() - $4::interval, now() - $4::interval, now(), template, $5, $6, 'amd64' FROM pools WHERE id = $2`,
		id, poolID, state, interval(age), capacity, labels)
	if state == "ready" {
		s.hub.polled(id)
	}
}

// The ec2 hosts a move is judged against are those the planner would
// launch: the planned host (planCapacity's virtual(), the smallest of its
// current template's latest registrations) and the probe host (the
// catalog's size of its instanceType alone, never a fallback's); with no
// registration, the catalog's size of its instanceType. Nested only when
// the template opts in and those registrations do not lack it. Past hosts
// are not hosts the pool has.
func TestResumeIntoAnotherPoolPlannerHost(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, s *Server, id string) string
		res   *resumeResources
		says  string // the whole message; "": allowed
	}{
		{name: "beyond the instance type, within a fallback", res: &resumeResources{CPUs: f64(24)},
			says: "pool intel cannot take this Run: it needs 24 cpus; its largest host has 16",
			setup: func(t *testing.T, s *Server, id string) string {
				execSQL(t, s, context.Background(), `INSERT INTO instance_types (provider, region, instance_type, vcpus, memory_bytes, architectures, fetched_at)
					VALUES ('ec2', 'us-east-1', 'm7i.8xlarge', 32, $1, '{x86_64}', now())`, 128*gib)
				return ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.4xlarge","fallbackInstanceTypes":["m7i.8xlarge"]}`)
			}},
		{name: "a terminated larger host in the window", res: &resumeResources{CPUs: f64(32)},
			says: "pool burst cannot take this Run: it needs 32 cpus; its largest host has 16",
			setup: func(t *testing.T, s *Server, id string) string {
				pool := ec2Pool(t, s, "burst", `{"region":"us-east-1"}`)
				ec2Host(t, s, "e-big", pool, "terminated", 2*time.Minute, `{"cpus":64,"memory":274877906944}`, `{}`)
				ec2Host(t, s, "e-small", pool, "terminated", time.Minute, `{"cpus":16,"memory":68719476736}`, `{}`)
				return pool
			}},
		{name: "an older template's smaller registration", res: &resumeResources{CPUs: f64(12)},
			setup: func(t *testing.T, s *Server, id string) string {
				pool := ec2Pool(t, s, "burst", `{"region":"us-east-1"}`)
				ec2Host(t, s, "e-old", pool, "terminated", time.Second, `{"cpus":8,"memory":68719476736}`, `{}`)
				execSQL(t, s, context.Background(), `UPDATE hosts SET launch_template = '{"region":"us-east-1","version":0}' WHERE id = 'e-old'`)
				ec2Host(t, s, "e-cur", pool, "terminated", time.Minute, `{"cpus":16,"memory":68719476736}`, `{}`)
				return pool
			}},
		{name: "nested where the template opts in but its hosts registered without",
			says: "pool nest cannot take this Run: it does not offer nested containers (its template's latest hosts registered without them)",
			setup: func(t *testing.T, s *Server, id string) string {
				setSpec(t, s, id, "{sandbox}", `{"nestedContainers":true}`)
				pool := ec2Pool(t, s, "nest", `{"region":"us-east-1","nestedContainers":true}`)
				ec2Host(t, s, "e1", pool, "terminated", time.Minute, `{"cpus":16}`, `{}`)
				return pool
			}},
		{name: "an instance type the catalog lacks, with a fallback it knows", res: &resumeResources{CPUs: f64(64)},
			setup: func(t *testing.T, s *Server, id string) string {
				return ec2Pool(t, s, "odd", `{"region":"us-east-1","instanceType":"x9.huge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
			}},
		{name: "a smaller fallback's registration: fits the probe host", res: &resumeResources{CPUs: f64(24)},
			setup: func(t *testing.T, s *Server, id string) string {
				addType(t, s, "m7i.8xlarge", 32)
				pool := ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.8xlarge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
				ec2Host(t, s, "e1", pool, "terminated", time.Minute, `{"cpus":16,"memory":68719476736}`, `{}`)
				return pool
			}},
		{name: "a smaller fallback's identity cannot fit the probe host", res: &resumeResources{CPUs: f64(24)},
			says: "pool intel cannot take this Run: it needs 24 cpus; its largest host has 32; none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, s *Server, id string) string {
				addType(t, s, "m7i.8xlarge", 32)
				pool := ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.8xlarge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
				ec2Host(t, s, "e1", pool, "terminated", time.Minute, `{"cpus":16,"memory":68719476736}`, `{"name":"e1"}`)
				setSpec(t, s, id, "{placement,requires}", `{"name":"e1"}`)
				facts, err := s.loadPoolFacts(context.Background(), pool, "t1")
				if err != nil {
					t.Fatal(err)
				}
				if len(facts.Profiles) != 2 {
					t.Fatalf("profiles: %+v, want planned and probe", facts.Profiles)
				}
				if planned := facts.Profiles[0]; planned.Labels["name"] != "e1" || planned.Capacity.CPUs != 16 {
					t.Errorf("planned profile: %+v, want name=e1 and 16 cpus", planned)
				}
				if probe := facts.Profiles[1]; probe.Capacity.CPUs != 32 {
					t.Errorf("probe profile: %+v, want 32 cpus", probe)
				}
				if name, present := facts.Profiles[1].Labels["name"]; present {
					t.Errorf("probe inherited name=%q", name)
				}
				return pool
			}},
		{name: "a smaller arm64 fallback's arch cannot fit the x86_64 probe host", res: &resumeResources{CPUs: f64(24)},
			says: "pool intel cannot take this Run: it needs 24 cpus; its largest host has 32; none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, s *Server, id string) string {
				probeArchPool(t, s)
				setSpec(t, s, id, "{placement,requires}", `{"arch":"arm64"}`)
				facts, err := s.loadPoolFacts(context.Background(), "pool_intel", "t1")
				if err != nil {
					t.Fatal(err)
				}
				if len(facts.Profiles) != 2 || facts.Profiles[0].Labels["arch"] != "arm64" || facts.Profiles[1].Labels["arch"] != "amd64" {
					t.Errorf("profiles: %+v, want the planned host arm64 and the probe host amd64", facts.Profiles)
				}
				return "pool_intel"
			}},
		{name: "the probe host's arch is its instance type's: fits the probe host", res: &resumeResources{CPUs: f64(24)},
			setup: func(t *testing.T, s *Server, id string) string {
				probeArchPool(t, s)
				setSpec(t, s, id, "{placement,requires}", `{"arch":"amd64"}`)
				return "pool_intel"
			}},
		{name: "an instance type listing i386 and x86_64: the probe host is amd64", res: &resumeResources{CPUs: f64(24)},
			setup: func(t *testing.T, s *Server, id string) string {
				probeArchPool(t, s)
				execSQL(t, s, context.Background(), `UPDATE instance_types SET architectures = '{i386,x86_64}' WHERE instance_type = 'm7i.8xlarge'`)
				setSpec(t, s, id, "{placement,requires}", `{"arch":"amd64"}`)
				return "pool_intel"
			}},
		{name: "an instance type with no architectures: the probe host keeps no previous host's", res: &resumeResources{CPUs: f64(24)},
			says: "pool intel cannot take this Run: it needs 24 cpus; its largest host has 32; none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, s *Server, id string) string {
				probeArchPool(t, s)
				execSQL(t, s, context.Background(), `UPDATE instance_types SET architectures = '{}' WHERE instance_type = 'm7i.8xlarge'`)
				setSpec(t, s, id, "{placement,requires}", `{"arch":"arm64"}`)
				return "pool_intel"
			}},
		{name: "an instance type whose only arch no runner runs: the probe host has none", res: &resumeResources{CPUs: f64(24)},
			says: "pool intel cannot take this Run: none of its hosts has the labels placement.requires names",
			setup: func(t *testing.T, s *Server, id string) string {
				probeArchPool(t, s)
				execSQL(t, s, context.Background(), `UPDATE instance_types SET architectures = '{i386}' WHERE instance_type = 'm7i.8xlarge'`)
				setSpec(t, s, id, "{placement,requires}", `{"arch":"386"}`)
				return "pool_intel"
			}},
		{name: "registrations smaller than the instance type: fits the probe host", res: &resumeResources{CPUs: f64(12)},
			setup: func(t *testing.T, s *Server, id string) string {
				addType(t, s, "m7i.2xlarge", 8)
				pool := ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.4xlarge","fallbackInstanceTypes":["m7i.2xlarge"]}`)
				ec2Host(t, s, "e1", pool, "terminated", 2*time.Minute, `{"cpus":8,"memory":68719476736}`, `{}`)
				ec2Host(t, s, "e2", pool, "terminated", time.Minute, `{"cpus":8,"memory":68719476736}`, `{}`)
				return pool
			}},
		{name: "registrations smaller than the instance type: beyond the probe host too", res: &resumeResources{CPUs: f64(20)},
			says: "pool intel cannot take this Run: it needs 20 cpus; its largest host has 16",
			setup: func(t *testing.T, s *Server, id string) string {
				addType(t, s, "m7i.8xlarge", 32)
				pool := ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.4xlarge","fallbackInstanceTypes":["m7i.8xlarge"]}`)
				ec2Host(t, s, "e1", pool, "terminated", 2*time.Minute, `{"cpus":8,"memory":68719476736}`, `{}`)
				ec2Host(t, s, "e2", pool, "terminated", time.Minute, `{"cpus":8,"memory":68719476736}`, `{}`)
				return pool
			}},
		{name: "an instance type the catalog lacks: no probe size, the planned host too small", res: &resumeResources{CPUs: f64(12)},
			says: "pool odd cannot take this Run: it needs 12 cpus; its largest host has 8",
			setup: func(t *testing.T, s *Server, id string) string {
				pool := ec2Pool(t, s, "odd", `{"region":"us-east-1","instanceType":"x9.huge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
				ec2Host(t, s, "e1", pool, "terminated", time.Minute, `{"cpus":8,"memory":68719476736}`, `{}`)
				return pool
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, _ := movedRun(t, s)
			addCatalog(t, s)
			target := c.setup(t, s, id)
			before := binding(t, s, id)
			_, err := resumeTo(s, id, target, c.res)
			if c.says == "" {
				if err != nil {
					t.Fatal(err)
				}
				if b := binding(t, s, id); b.PoolID != target {
					t.Errorf("after the move: %+v", b)
				}
				return
			}
			he := refused(t, err, http.StatusUnprocessableEntity, "pool_unfit", "resume")
			if he.Message != c.says {
				t.Errorf("message %q, want %q", he.Message, c.says)
			}
			if after := binding(t, s, id); after != before {
				t.Errorf("after the refusal: %+v, want %+v", after, before)
			}
		})
	}
}

// What lux cannot tell it does not refuse on: an unknown image platform or
// an unknown pool platform moves the Run, saying platformUnchecked; both
// known and matching moves it checked. An ec2 template that opts into
// nested containers takes a nested Run with no host yet; one whose
// instance type the catalog lacks takes any size.
func TestResumeIntoAnotherPoolAllowed(t *testing.T) {
	for _, c := range []struct {
		name      string
		setup     func(t *testing.T, s *Server, id string) string
		res       *resumeResources
		unchecked bool
	}{
		{name: "image platforms unknown", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string { return poolID(t, s, "big", "t1") }},
		{name: "pool platform unknown: a static pool with no host", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string {
				execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, id)
				return poolID(t, s, "empty", "t1")
			}},
		{name: "pool platform unknown: an instance type the catalog lacks", unchecked: true, res: &resumeResources{CPUs: f64(256)},
			setup: func(t *testing.T, s *Server, id string) string {
				execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, id)
				return ec2Pool(t, s, "odd", `{"region":"us-east-1","instanceType":"x9.huge"}`)
			}},
		{name: "a built image into arm64 hosts: built on an amd64 host, rebuilt there", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string {
				setSpec(t, s, id, "{image}", `{"build":{"containerfile":"FROM alpine"}}`)
				execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, id)
				execSQL(t, s, context.Background(), `UPDATE hosts SET arch = 'arm64' WHERE id = 'hb'`)
				return poolID(t, s, "big", "t1")
			}},
		{name: "a multi-arch image on arm64 hosts",
			setup: func(t *testing.T, s *Server, id string) string {
				execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64,linux/arm64/v8}' WHERE run_id = $1`, id)
				execSQL(t, s, context.Background(), `UPDATE hosts SET arch = 'arm64' WHERE id = 'hb'`)
				return poolID(t, s, "big", "t1")
			}},
		{name: "the catalog's platform and size", res: &resumeResources{CPUs: f64(16), Memory: bytesPtr(64 * gib)},
			setup: func(t *testing.T, s *Server, id string) string {
				execSQL(t, s, context.Background(), `UPDATE placements SET image_platforms = '{linux/amd64}' WHERE run_id = $1`, id)
				return ec2Pool(t, s, "intel", `{"region":"us-east-1","instanceType":"m7i.4xlarge"}`)
			}},
		{name: "nested on an opted-in ec2 template", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string {
				setSpec(t, s, id, "{sandbox}", `{"nestedContainers":true}`)
				return ec2Pool(t, s, "nest", `{"region":"us-east-1","nestedContainers":true}`)
			}},
		{name: "required labels on an ec2 pool that never launched a host", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string {
				setSpec(t, s, id, "{placement,requires}", `{"gpu":"a100"}`)
				return ec2Pool(t, s, "burst", `{"region":"us-east-1"}`)
			}},
		{name: "nested on a static host with --nested", unchecked: true,
			setup: func(t *testing.T, s *Server, id string) string {
				setSpec(t, s, id, "{sandbox}", `{"nestedContainers":true}`)
				execSQL(t, s, context.Background(), `UPDATE hosts SET labels = '{"nested":"true"}' WHERE id = 'hb'`)
				return poolID(t, s, "big", "t1")
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, _ := movedRun(t, s)
			addCatalog(t, s)
			target := c.setup(t, s, id)
			out, err := resumeTo(s, id, target, c.res)
			if err != nil {
				t.Fatal(err)
			}
			if pl := out.Body.Placement; pl == nil || pl.PoolID != target || pl.PlatformUnchecked != c.unchecked {
				t.Errorf("answer's placement %+v, want %s unchecked=%v", pl, target, c.unchecked)
			}
			if b := binding(t, s, id); b.PoolID != target || b.SpecPoolID != target || b.State != StateResuming {
				t.Errorf("after the move: %+v", b)
			}
			pe, _ := resumeEvent(t, s, id)["pool"].(map[string]any)
			if (pe["platformUnchecked"] == true) != c.unchecked {
				t.Errorf("event's pool %v, want platformUnchecked=%v", pe, c.unchecked)
			}
		})
	}
}

// On a Run already resuming, a resume naming the pool its resume moved it
// to is that resume again (202, with the first's placement); one naming
// any other pool, the one it left included, is 409 not_resumable. Nothing
// changes, and only one resume.requested is written.
func TestResumeIntoAnotherPoolWhileResuming(t *testing.T) {
	for _, c := range []struct {
		name   string
		retry  func(big, other string) string
		status int
	}{
		{"the same pool", func(big, other string) string { return big }, 0},
		{"no placement", func(big, other string) string { return "" }, 0},
		{"the pool it left", func(big, other string) string { return "default" }, http.StatusConflict},
		{"a third pool", func(big, other string) string { return other }, http.StatusConflict},
		{"an unknown pool", func(big, other string) string { return "pool_nope" }, http.StatusUnprocessableEntity},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, big := movedRun(t, s)
			other := poolID(t, s, "other", "t1")
			first, err := resumeTo(s, id, big, &resumeResources{CPUs: f64(8)})
			if err != nil {
				t.Fatal(err)
			}
			want := binding(t, s, id)
			out, err := resumeTo(s, id, c.retry(big, other), nil)
			if c.status != 0 {
				refused(t, err, c.status, "", "retry")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				named := c.retry(big, other) != ""
				fp := first.Body.Placement
				if pl := out.Body.Placement; named && (pl == nil || pl.PoolID != fp.PoolID || pl.Pool != fp.Pool ||
					pl.From == nil || *pl.From != *fp.From || pl.PlatformUnchecked != fp.PlatformUnchecked) {
					t.Errorf("retry's placement %+v, want the first's %+v", pl, fp)
				} else if !named && pl != nil {
					t.Errorf("retry without placement answered %+v", pl)
				}
			}
			if got, events := binding(t, s, id), resumeRequests(t, s, id); got != want || events != 1 {
				t.Errorf("after the retry: %+v and %d resume.requested, want %+v and 1", got, events, want)
			}
		})
	}
}

// An operator's chosen host must be in the pool the resume names: a host
// elsewhere is 422 invalid_spec and changes nothing; one in it moves the Run.
// The host is checked before the Run's state and the pool's fit, so a
// resuming Run's retry and a resume that pool_unfit would also refuse get
// invalid_spec too.
func TestResumeIntoAnotherPoolChosenHost(t *testing.T) {
	setup := func(t *testing.T) (*Server, string, string, func(to string, res *resumeResources) (*resumeOutput, error)) {
		s := testServer(t)
		id, big := movedRun(t, s)
		return s, id, big, func(to string, res *resumeResources) (*resumeOutput, error) {
			return s.resumeRun(operatorCtx(), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{
				To: to, Resources: res, Placement: &resumeTarget{PoolID: big}, Secrets: []spec.Secret{{Name: "TOKEN", Value: "v"}}}})
		}
	}
	const outside = "the chosen host is in pool default, not big"
	t.Run("a stopped Run", func(t *testing.T) {
		s, id, big, resume := setup(t)
		before := binding(t, s, id)
		_, err := resume("hc", nil)
		he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume --to hc")
		if he.Message != outside {
			t.Errorf("message %q, want %q", he.Message, outside)
		}
		if after := binding(t, s, id); after != before {
			t.Errorf("after the refusal: %+v, want %+v", after, before)
		}
		if _, err := resume("hb", nil); err != nil {
			t.Fatal(err)
		}
		if b := binding(t, s, id); b.PoolID != big || b.State != StateResuming {
			t.Errorf("after resume --to hb: %+v", b)
		}
	})
	t.Run("a resuming Run's retry", func(t *testing.T) {
		s, id, big, resume := setup(t)
		if _, err := resumeTo(s, id, big, nil); err != nil {
			t.Fatal(err)
		}
		before := binding(t, s, id)
		_, err := resume("hc", nil)
		he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "retry --to hc")
		if he.Message != outside {
			t.Errorf("message %q, want %q", he.Message, outside)
		}
		if after := binding(t, s, id); after != before || before.State != StateResuming {
			t.Errorf("after the refusal: %+v, want %+v, resuming", after, before)
		}
	})
	t.Run("a host outside the pool and resources beyond it", func(t *testing.T) {
		s, id, _, resume := setup(t)
		before := binding(t, s, id)
		_, err := resume("hc", &resumeResources{CPUs: f64(32)})
		he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume --to hc, 32 cpus")
		if he.Message != outside {
			t.Errorf("message %q, want %q", he.Message, outside)
		}
		if after := binding(t, s, id); after != before {
			t.Errorf("after the refusal: %+v, want %+v", after, before)
		}
	})
}

// Through the HTTP API: placement.poolId decodes, the answer carries
// placement, and GET's pool is the new one.
func TestResumeIntoAnotherPoolHTTP(t *testing.T) {
	s := testServer(t)
	id, big := movedRun(t, s)
	key := apiKey(t, s, new("t1"), "run", "read")
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{
		"placement": map[string]any{"poolId": big}, "resources": map[string]any{"cpus": 8},
		"secrets": []map[string]string{{"name": "TOKEN", "value": "v"}}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	var answer struct {
		PoolID    string         `json:"poolId"`
		Placement map[string]any `json:"placement"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.PoolID != big || answer.Placement["poolId"] != big || answer.Placement["platformUnchecked"] != true {
		t.Errorf("answer %s", w.Body)
	}
	w = apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{"placement": map[string]any{"poolId": ""}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty poolId: %d %s", w.Code, w.Body)
	}
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
	var run struct {
		Pool   string `json:"pool"`
		PoolID string `json:"poolId"`
	}
	if g.Code != http.StatusOK || json.Unmarshal(g.Body.Bytes(), &run) != nil || run.Pool != "big" || run.PoolID != big {
		t.Errorf("GET: %d %s", g.Code, g.Body)
	}
}
