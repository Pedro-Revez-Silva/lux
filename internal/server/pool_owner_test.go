package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// submitAs submits a Run naming pool ("" for none) as tenant, with an
// idempotency key when key is not "", and returns its id.
func submitAs(t *testing.T, s *Server, tenant, pool, key string) string {
	t.Helper()
	out, err := s.submitRun(tenantCtx(tenant), &submitRunInput{IdempotencyKey: key, Body: spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Placement: spec.Placement{Pool: pool},
	}})
	if err != nil {
		t.Fatalf("%s submitting pool %q: %v", tenant, pool, err)
	}
	return out.Body.ID
}

// runPool is a Run's stored pool, the owner of the pool its pool_id binds
// it to ("" the platform, "<nil>" for none), and its submitted event's
// poolOwner.
func runPool(t *testing.T, s *Server, id string) (pool, owner, eventOwner string) {
	t.Helper()
	systemScan(t, s, `SELECT r.spec->'placement'->>'pool', coalesce((SELECT coalesce(p.tenant_id, '') FROM pools p WHERE p.id = r.pool_id), '<nil>'),
			coalesce(e.data->>'poolOwner', '')
		FROM runs r JOIN run_events e ON e.run_id = r.id AND e.type = 'submitted' WHERE r.id = $1`,
		[]any{id}, &pool, &owner, &eventOwner)
	return pool, owner, eventOwner
}

// poolID is the id of tenant's ("" the platform's) pool name, created as
// a static pool if it has no row.
func poolID(t *testing.T, s *Server, pool, tenant string) string {
	t.Helper()
	var id string
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		var err error
		id, err = EnsureStaticPool(context.Background(), tx, tenant, pool)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// readyHost adds a connected, ready host in pool, tenant's ("" for a
// platform host), with the alpine image cached when cached (which makes it
// the scheduler's preference among otherwise equal hosts).
func readyHost(t *testing.T, s *Server, id, pool, tenant string, cached bool) {
	t.Helper()
	var tenantArg any
	if tenant != "" {
		tenantArg = tenant
	}
	caches := `{}`
	if cached {
		caches = `{"images":["alpine"]}`
	}
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id, name, pool_id, tenant_id, state, capacity, caches, last_heartbeat)
		VALUES ($1, $1, $2, $3, 'ready', '{"runs":10}', $4, now())`, id, poolID(t, s, pool, tenant), tenantArg, caches)
	s.hub.polled(id)
}

func placedOn(t *testing.T, s *Server, id string) (host, state, reason string) {
	t.Helper()
	systemScan(t, s, `SELECT coalesce(p.host_id, ''), r.state, r.state_reason FROM runs r
		LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch WHERE r.id = $1`, []any{id}, &host, &state, &reason)
	return host, state, reason
}

func schedule(t *testing.T, s *Server) {
	t.Helper()
	if _, _, err := s.scheduleBatch(context.Background(), cursorPos{}); err != nil {
		t.Fatal(err)
	}
}

// eligible: hosts discovered for a Run of tenant bound to owner's pool name.
func eligible(t *testing.T, s *Server, pool string, owner *string, tenant string) []string {
	t.Helper()
	id := poolID(t, s, pool, *owner)
	var ids []string
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		var err error
		ids, err = s.eligibleHostIDs(context.Background(), tx, []*string{&id}, []string{tenant}, []string{""})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// A tenant pool and a platform pool named alike: a Run resolved to the
// platform's default goes to platform hosts only, even beside a Run bound
// for the tenant's pool of that name; and the tenant's default, shadowing
// a platform pool of the same name, keeps the tenant's Runs on its hosts.
func TestPoolOwnerSeparatesSameNamedPools(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared, is_default) VALUES ('pp', NULL, 'burst', 'static', true, true)`)
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static"})
	// The tenant's host is the more attractive one (the image is cached).
	readyHost(t, s, "h-plat", "burst", "", false)
	readyHost(t, s, "h-t1", "burst", "t1", true)

	byDefault := submitAs(t, s, "t1", "", "")
	if pool, owner, ev := runPool(t, s, byDefault); pool != "burst" || owner != "" || ev != "platform" {
		t.Fatalf("platform default: pool %q owner %q event %q", pool, owner, ev)
	}
	// An explicit name: the tenant's pool shadows the platform's.
	named := submitAs(t, s, "t1", "burst", "")
	if pool, owner, ev := runPool(t, s, named); pool != "burst" || owner != "t1" || ev != "tenant" {
		t.Fatalf("explicit name: pool %q owner %q event %q", pool, owner, ev)
	}

	// Discovery alone: the platform Run's pool has only the platform host.
	if got := eligible(t, s, "burst", new(""), "t1"); !slices.Equal(got, []string{"h-plat"}) {
		t.Fatalf("hosts eligible for the platform's burst: %v", got)
	}
	if got := eligible(t, s, "burst", new("t1"), "t1"); !slices.Equal(got, []string{"h-t1"}) {
		t.Fatalf("hosts eligible for t1's burst: %v", got)
	}

	// One batch: both hosts are candidates, each Run takes its own pool's.
	schedule(t, s)
	if host, _, _ := placedOn(t, s, byDefault); host != "h-plat" {
		t.Fatalf("the platform default's Run went to %q, want h-plat", host)
	}
	if host, _, _ := placedOn(t, s, named); host != "h-t1" {
		t.Fatalf("the Run naming burst went to %q, want h-t1", host)
	}

	// The mirror: t1's default is its burst; t2 has no burst, so its Run
	// naming burst is the platform's, and puts h-plat in the same batch.
	execSQL(t, s, ctx, `UPDATE hosts SET caches = '{"images":["alpine"]}' WHERE id = 'h-plat'`)
	execSQL(t, s, ctx, `UPDATE hosts SET caches = '{}' WHERE id = 'h-t1'`)
	mustPut(t, s, "t1", Pool{Name: "burst", IsDefault: mark(true)})
	tenantDefault := submitAs(t, s, "t1", "", "")
	if pool, owner, ev := runPool(t, s, tenantDefault); pool != "burst" || owner != "t1" || ev != "tenant" {
		t.Fatalf("tenant default: pool %q owner %q event %q", pool, owner, ev)
	}
	other := submitAs(t, s, "t2", "burst", "")
	if _, owner, ev := runPool(t, s, other); owner != "" || ev != "platform" {
		t.Fatalf("t2 naming burst: owner %q event %q", owner, ev)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, tenantDefault); host != "h-t1" {
		t.Fatalf("the tenant default's Run went to %q, want h-t1", host)
	}
	if host, _, _ := placedOn(t, s, other); host != "h-plat" {
		t.Fatalf("t2's Run went to %q, want h-plat", host)
	}
}

// With no hosts, a Run resolved to the platform's default counts toward
// the platform pool's demand, never the same-named tenant pool's, and
// waits for the platform's provider.
func TestPoolOwnerDemand(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES ('pp', NULL, 'burst', 'ec2', true)`)
	// The tenant's pool is static: were its provider the one consulted,
	// the Run would not go to provisioning.
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static"})
	id := submitAs(t, s, "t1", "", "")
	schedule(t, s)
	if _, state, _ := placedOn(t, s, id); state != StateProvisioning {
		t.Fatalf("state %q, want provisioning (the platform pool's provider)", state)
	}
	// Demand is counted per pool; make the tenant's ec2 too, so it would
	// count a Run it were given.
	execSQL(t, s, ctx, `UPDATE pools SET provider = 'ec2' WHERE tenant_id = 't1'`)
	demand := func(pl poolRow) int {
		var st poolState
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
			t.Fatal(err)
		}
		return st.demand
	}
	t1Burst := poolID(t, s, "burst", "t1")
	if d := demand(poolRow{ID: "pp", Name: "burst", Provider: "ec2"}); d != 1 {
		t.Errorf("platform burst demand %d, want 1", d)
	}
	if d := demand(poolRow{ID: t1Burst, Name: "burst", Provider: "ec2", TenantID: new("t1")}); d != 0 {
		t.Errorf("t1's burst demand %d, want 0", d)
	}

	// A Run bound to no pool is bound by the scheduler by name, the
	// tenant's shadowing the platform's, and counts there.
	execSQL(t, s, ctx, `UPDATE runs SET pool_id = NULL WHERE id = $1`, id)
	schedule(t, s)
	if d := demand(poolRow{ID: t1Burst, Name: "burst", Provider: "ec2", TenantID: new("t1")}); d != 1 {
		t.Errorf("unbound Run: t1's burst demand %d, want 1", d)
	}
}

// A name no pool row has: no pool yet; once a static host joins a pool of
// that name, the scheduler binds the Run to it and places it there.
func TestPoolOwnerUnknownName(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	id := submitAs(t, s, "t1", "gpu", "")
	if pool, owner, ev := runPool(t, s, id); pool != "gpu" || owner != "<nil>" || ev != "" {
		t.Fatalf("pool %q owner %q event %q, want gpu, no owner", pool, owner, ev)
	}
	readyHost(t, s, "h-gpu", "gpu", "", false)
	schedule(t, s)
	if host, _, _ := placedOn(t, s, id); host != "h-gpu" {
		t.Fatalf("placed on %q, want h-gpu", host)
	}
}

// A resume after the default moved, even to a same-named pool of another
// owner, keeps the Run's pool and owner.
func TestPoolOwnerKeptOnResume(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared, is_default) VALUES ('pp', NULL, 'burst', 'static', true, true)`)
	id := submitAs(t, s, "t1", "", "")
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static", IsDefault: mark(true)})
	readyHost(t, s, "h-plat", "burst", "", false)
	readyHost(t, s, "h-t1", "burst", "t1", true)
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}}); err != nil {
		t.Fatal(err)
	}
	if pool, owner, _ := runPool(t, s, id); pool != "burst" || owner != "" {
		t.Fatalf("after resume: pool %q owner %q, want the platform's burst", pool, owner)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, id); host != "h-plat" {
		t.Fatalf("resumed onto %q, want h-plat", host)
	}
}

// A Run whose pool is removed waits, saying so, rather than moving to
// another owner's pool of that name; re-creating the pool serves it.
func TestPoolOwnerRetiredPool(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pp', NULL, 'burst', 'ec2')`)
	id := submitAs(t, s, "t1", "burst", "")
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "ec2"})
	readyHost(t, s, "h-t1", "burst", "t1", false)
	execSQL(t, s, ctx, `UPDATE pools SET retired = true WHERE id = 'pp'`)
	schedule(t, s)
	host, state, reason := placedOn(t, s, id)
	if host != "" || state != StateSubmitted || reason != "its pool burst was removed" {
		t.Fatalf("host %q state %q reason %q, want waiting for its removed pool", host, state, reason)
	}
	execSQL(t, s, ctx, `UPDATE pools SET retired = false WHERE id = 'pp'`)
	schedule(t, s)
	if _, state, _ := placedOn(t, s, id); state != StateProvisioning {
		t.Fatalf("state %q after the pool came back, want provisioning", state)
	}
}

// Removing a static pool leaves its hosts in service (only provisioned
// hosts are cordoned), but a Run bound to that pool waits, saying so,
// rather than taking one of them; a Run submitted after the removal
// resolves to no pool row and keeps the by-name rule.
func TestPoolOwnerRemovedStaticPool(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static"})
	id := submitAs(t, s, "t1", "burst", "")
	if _, owner, _ := runPool(t, s, id); owner != "t1" {
		t.Fatalf("owner %q, want t1", owner)
	}
	if _, err := s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "burst"}); err != nil {
		t.Fatal(err)
	}
	readyHost(t, s, "h-t1", "burst", "t1", false)

	// Each filter on its own: discovery, then pickHost over the host.
	if got := eligible(t, s, "burst", new("t1"), "t1"); len(got) != 0 {
		t.Fatalf("hosts eligible for the removed pool: %v", got)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		hosts, err := s.candidateHosts(ctx, tx, []string{"h-t1"})
		if err != nil || len(hosts) != 1 {
			t.Fatalf("candidates %v: %v", hosts, err)
		}
		h, _, err := s.pickHost(ctx, tx, pendingRun{ID: id, TenantID: "t1", PoolID: new(poolID(t, s, "burst", "t1")),
			Spec: spec.RunSpec{Placement: spec.Placement{Pool: "burst"}}}, hosts)
		if h != nil {
			t.Errorf("pickHost chose %s for a Run of the removed pool", h.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	schedule(t, s)
	host, state, reason := placedOn(t, s, id)
	if host != "" || state != StateSubmitted || reason != "its pool burst was removed" {
		t.Fatalf("host %q state %q reason %q, want waiting for its removed pool", host, state, reason)
	}

	// A Run naming the removed pool finds no pool, and its hosts (the
	// removed pool's) are not another pool's to take it.
	later := submitAs(t, s, "t1", "burst", "")
	if _, owner, _ := runPool(t, s, later); owner != "<nil>" {
		t.Fatalf("a Run after the removal: owner %q, want none", owner)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, later); host != "" {
		t.Fatalf("the unbound Run went to %q, want no host", host)
	}
}

// A submit retried with the same idempotency key after the default moved
// returns the first Run, with its pool and owner, and creates no other.
func TestIdempotentResubmitKeepsItsPool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES ('pp', NULL, 'burst', 'static', true)`)
	first := submitAs(t, s, "t1", "", "key-1")
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static", IsDefault: mark(true)})
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{IdempotencyKey: "key-1", Body: spec.RunSpec{
		Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != 200 || out.Body.ID != first || out.Body.Spec.Placement.Pool != "burst" {
		t.Fatalf("re-submit: %d %s pool %q, want 200 %s burst", out.Status, out.Body.ID, out.Body.Spec.Placement.Pool, first)
	}
	if pool, owner, ev := runPool(t, s, first); pool != "burst" || owner != "" || ev != "platform" {
		t.Fatalf("after re-submit: pool %q owner %q event %q, want the platform's burst", pool, owner, ev)
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM runs`, nil, &n)
	if n != 1 {
		t.Fatalf("%d Runs after a re-submit", n)
	}
}

// namedPools adds platform static pools whose ids are their names, for
// fixtures that put hosts and tokens in a pool by a literal id.
func namedPools(t *testing.T, s *Server, names ...string) {
	t.Helper()
	for _, n := range names {
		execSQL(t, s, context.Background(), `INSERT INTO pools (id, name, provider) VALUES ($1, $1, 'static') ON CONFLICT (id) DO NOTHING`, n)
	}
}
