package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func tenantCtx(tenant string) context.Context {
	return context.WithValue(context.Background(), principalKey, Principal{TenantID: tenant, KeyID: "k-" + tenant, Scopes: []string{"admin", "run", "read"}})
}

// submitPool submits a Run whose spec names pool ("" for none) as tenant,
// and returns the pool stored in its spec and its submitted event's data.
func submitPool(t *testing.T, s *Server, tenant, pool string) (string, map[string]any) {
	t.Helper()
	ctx := tenantCtx(tenant)
	sp := spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Placement: spec.Placement{Pool: pool},
	}
	out, err := s.submitRun(ctx, &submitRunInput{Body: sp})
	if err != nil {
		t.Fatalf("submit (pool %q): %v", pool, err)
	}
	var stored string
	var data map[string]any
	err = s.db.Tx(ctx, store.Tenant(tenant), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT spec->'placement'->>'pool' FROM runs WHERE id = $1`, out.Body.ID).Scan(&stored); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT data FROM run_events WHERE run_id = $1 AND type = 'submitted'`, out.Body.ID).Scan(&data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Spec.Placement.Pool != stored {
		t.Fatalf("returned spec's pool %q, stored %q", out.Body.Spec.Placement.Pool, stored)
	}
	return stored, data
}

func defaultPools(t *testing.T, s *Server) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT coalesce(tenant_id, ''), name FROM pools WHERE is_default`)
		if err != nil {
			return err
		}
		var owner, name string
		_, err = pgx.ForEachRow(rows, []any{&owner, &name}, func() error {
			if prev, ok := got[owner]; ok {
				t.Errorf("owner %q has two defaults: %s and %s", owner, prev, name)
			}
			got[owner] = name
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func putPoolAs(t *testing.T, s *Server, tenant string, pl Pool) (Pool, error) {
	t.Helper()
	out, err := s.putPool(tenantCtx(tenant), poolIn(pl))
	if err != nil {
		return Pool{}, err
	}
	return Pool(out.Body), nil
}

func mark(v bool) *bool { return &v }

func mustPut(t *testing.T, s *Server, tenant string, pl Pool) Pool {
	t.Helper()
	out, err := putPoolAs(t, s, tenant, pl)
	if err != nil {
		t.Fatalf("put pool %s: %v", pl.Name, err)
	}
	return out
}

// A Run that names no pool goes to the tenant's default pool, else the
// platform's, else the pool named "default". The choice is stored in its
// spec and said in its submitted event; a Run that names a pool, "default"
// included, keeps it.
func TestDefaultPoolResolution(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pp', NULL, 'plat', 'static'), ('pp2', NULL, 'plat2', 'static')`)

	check := func(tenant, pool, wantPool, wantFrom string) {
		t.Helper()
		got, data := submitPool(t, s, tenant, pool)
		if got != wantPool || data["pool"] != wantPool || data["poolFrom"] != wantFrom {
			t.Fatalf("%s submitting pool %q: stored %q, event %v; want %q from %s", tenant, pool, got, data, wantPool, wantFrom)
		}
		if data["by"] != "k-"+tenant {
			t.Fatalf("event lost its actor: %v", data)
		}
	}

	// Nothing marked: the literal "default", as before.
	check("t1", "", "default", "fallback")

	// A platform default serves every tenant without its own.
	execSQL(t, s, ctx, `UPDATE pools SET is_default = true WHERE id = 'pp'`)
	check("t1", "", "plat", "platform-default")
	check("t2", "", "plat", "platform-default")

	// The tenant's own comes first; other tenants still get the platform's.
	mustPut(t, s, "t1", Pool{Name: "arm64", Provider: "static", IsDefault: mark(true)})
	check("t1", "", "arm64", "tenant-default")
	check("t2", "", "plat", "platform-default")

	// A named pool is never replaced, "default" included.
	check("t1", "x86", "x86", "spec")
	check("t1", "default", "default", "spec")

	// The platform's mark moves in one statement too.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return SetDefaultPool(ctx, tx, "", "plat2", true) }); err != nil {
		t.Fatal(err)
	}
	check("t2", "", "plat2", "platform-default")
	if got := defaultPools(t, s); got[""] != "plat2" || got["t1"] != "arm64" {
		t.Fatalf("defaults %v", got)
	}
}

// The pool is fixed at submit: a later change of default leaves submitted
// Runs where they were, and a resume does not resolve it again.
func TestDefaultPoolIsStoredAtSubmit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static"})
	tc := tenantCtx("t1")
	out, err := s.submitRun(tc, &submitRunInput{Body: spec.RunSpec{
		Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	mustPut(t, s, "t1", Pool{Name: "b", IsDefault: mark(true)})
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := s.resumeRun(tc, &resumeRunInput{RunPath: RunPath{ID: id}}); err != nil {
		t.Fatal(err)
	}
	var pool string
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT spec->'placement'->>'pool' FROM runs WHERE id = $1`, id).Scan(&pool)
	}); err != nil {
		t.Fatal(err)
	}
	if pool != "a" {
		t.Fatalf("after the default moved and a resume, the Run's pool is %q, want a", pool)
	}
}

// Marking a pool moves the mark; false clears it; a body with only the
// name and isDefault leaves the pool's settings alone; a pool set without
// isDefault keeps its mark.
func TestMarkDefaultPool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	a := mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", MinHosts: 1, MaxHosts: 3, IsDefault: mark(true)})
	if a.IsDefault == nil || !*a.IsDefault || a.MaxHosts != 3 {
		t.Fatalf("put returned %+v", a)
	}
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "ec2", MaxHosts: 5})
	if got := defaultPools(t, s); got["t1"] != "a" {
		t.Fatalf("defaults %v, want a", got)
	}

	b := mustPut(t, s, "t1", Pool{Name: "b", IsDefault: mark(true)})
	if b.Provider != "ec2" || b.MaxHosts != 5 || !*b.IsDefault {
		t.Fatalf("marking changed the pool: %+v", b)
	}
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("defaults %v, want b", got)
	}

	// Updating a pool without isDefault keeps its mark.
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "ec2", MaxHosts: 6})
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("an update without isDefault moved the mark: %v", got)
	}

	// Clearing another pool's mark leaves the default where it is.
	mustPut(t, s, "t1", Pool{Name: "a", IsDefault: mark(false)})
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("defaults %v, want b", got)
	}
	mustPut(t, s, "t1", Pool{Name: "b", IsDefault: mark(false)})
	if got := defaultPools(t, s); len(got) != 0 {
		t.Fatalf("defaults %v, want none", got)
	}

	list, err := s.listPools(tenantCtx("t1"), &TenantQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Body.Pools {
		if p.IsDefault == nil || *p.IsDefault {
			t.Fatalf("listed %s isDefault %v", p.Name, p.IsDefault)
		}
	}

	var he *HTTPError
	if _, err := putPoolAs(t, s, "t1", Pool{Name: "nope", IsDefault: mark(true)}); !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("marking a pool that does not exist: %v, want 404", err)
	}
}

// Removing the default pool clears its mark: Runs naming no pool go on to
// the platform's default, and re-creating the pool does not bring it back.
func TestRetiringTheDefaultPoolClearsIt(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES ('pp', NULL, 'plat', 'static', true)`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	if _, err := s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := defaultPools(t, s); got["t1"] != "" {
		t.Fatalf("the retired pool is still the default: %v", got)
	}
	if got, data := submitPool(t, s, "t1", ""); got != "plat" || data["poolFrom"] != "platform-default" {
		t.Fatalf("after retiring the default: %q %v", got, data)
	}
	var he *HTTPError
	if _, err := putPoolAs(t, s, "t1", Pool{Name: "a", IsDefault: mark(true)}); !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("marking a retired pool: %v, want 404", err)
	}
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static"})
	if got := defaultPools(t, s); got["t1"] != "" {
		t.Fatalf("re-creating the pool restored its mark: %v", got)
	}
}

// Two marks at once, of different pools: both succeed, one after the
// other, and the tenant is left with one default.
func TestConcurrentMarks(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	for _, n := range []string{"a", "b", "c"} {
		mustPut(t, s, "t1", Pool{Name: n, Provider: "static"})
	}
	mustPut(t, s, "t1", Pool{Name: "c", IsDefault: mark(true)})
	for range 10 {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, n := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.putPool(tenantCtx("t1"), poolIn(Pool{Name: n, IsDefault: mark(true)}))
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("concurrent marks: %v, %v", errs[0], errs[1])
		}
		if got := defaultPools(t, s); got["t1"] != "a" && got["t1"] != "b" {
			t.Fatalf("defaults after concurrent marks: %v", got)
		}
	}
}

// A tenant marks only its own pools: not another tenant's, not a platform
// pool, and another tenant's default never serves it.
func TestDefaultPoolIsTenantScoped(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pp', NULL, 'plat', 'static')`)
	mustPut(t, s, "t2", Pool{Name: "theirs", Provider: "static", IsDefault: mark(true)})
	var he *HTTPError
	for _, name := range []string{"theirs", "plat"} {
		if _, err := putPoolAs(t, s, "t1", Pool{Name: name, IsDefault: mark(true)}); !errors.As(err, &he) || he.Status != http.StatusNotFound {
			t.Fatalf("t1 marking %s: %v, want 404", name, err)
		}
	}
	if got, data := submitPool(t, s, "t1", ""); got != "default" || data["poolFrom"] != "fallback" {
		t.Fatalf("t1 got t2's default: %q %v", got, data)
	}
	if got := defaultPools(t, s); len(got) != 1 || got["t2"] != "theirs" {
		t.Fatalf("defaults %v", got)
	}
}

// An operator marks a platform pool as the platform's default (no tenant
// named), and a tenant's pool with ?tenant=; anything else from an
// operator still needs a tenant.
func TestOperatorMarksDefaultPools(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pp', NULL, 'plat', 'static'), ('p1', 't1', 'plat', 'static')`)
	op := context.WithValue(ctx, principalKey, Principal{Operator: true, Scopes: []string{"admin"}})
	out, err := s.putPool(op, poolIn(Pool{Name: "plat", IsDefault: mark(true)}))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.Platform || !*out.Body.IsDefault {
		t.Fatalf("operator marked %+v, want the platform pool", out.Body)
	}
	if got := defaultPools(t, s); len(got) != 1 || got[""] != "plat" {
		t.Fatalf("defaults %v, want only the platform's", got)
	}
	opT1 := context.WithValue(ctx, principalKey, Principal{Operator: true, TenantID: "t1", Scopes: []string{"admin"}})
	if _, err := s.putPool(opT1, poolIn(Pool{Name: "plat", IsDefault: mark(true)})); err != nil {
		t.Fatal(err)
	}
	if got := defaultPools(t, s); got[""] != "plat" || got["t1"] != "plat" {
		t.Fatalf("defaults %v", got)
	}
	var he *HTTPError
	if _, err := s.putPool(op, poolIn(Pool{Name: "x", Provider: "static"})); !errors.As(err, &he) || he.Code != "tenant_required" {
		t.Fatalf("an operator creating a pool without a tenant: %v", err)
	}
}

// holdDefaultLock takes tenantID's default-pool lock in a transaction of
// its own, and returns its backend pid and a release that commits it.
func holdDefaultLock(t *testing.T, s *Server, ctx context.Context, tenantID string) (int, func()) {
	t.Helper()
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockDefaultPool(ctx, tx, tenantID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			tx.Commit(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// waitBlockedBy returns once a backend waits on an advisory lock pid holds,
// and fails if done delivers first.
func waitBlockedBy(t *testing.T, s *Server, ctx context.Context, pid int, done <-chan error) {
	t.Helper()
	for {
		var waiting bool
		systemScan(t, s, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event = 'advisory' AND $1 = ANY(pg_blocking_pids(pid)))`, []any{pid}, &waiting)
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("finished without waiting for the default-pool lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Removing a pool waits for its owner's default-pool lock, as a mark
// does, before it touches the pool's row.
func TestDeletePoolTakesTheDefaultLock(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	pid, release := holdDefaultLock(t, s, ctx, "t1")
	done := make(chan error, 1)
	go func() {
		_, err := s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "a"})
		done <- err
	}()
	waitBlockedBy(t, s, ctx, pid, done)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := defaultPools(t, s); len(got) != 0 {
		t.Fatalf("defaults %v after removing the default", got)
	}
}

// Creating a pool with a mark (luxd admin create-pool --default) takes the
// owner's lock before the upsert touches the pool's row, as putPool does.
func TestSavePoolLocksBeforeTheUpsert(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pid, release := holdDefaultLock(t, s, ctx, "")
	upserted := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return SavePool(ctx, tx, "", "plat", mark(true), func() error {
				upserted <- struct{}{}
				_, err := tx.Exec(ctx, `INSERT INTO pools (id, name, provider) VALUES ('pp', 'plat', 'static')`)
				return err
			})
		})
	}()
	waitBlockedBy(t, s, ctx, pid, done)
	select {
	case <-upserted:
		t.Fatal("the upsert ran before the default-pool lock was held")
	default:
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := defaultPools(t, s); got[""] != "plat" {
		t.Fatalf("defaults %v", got)
	}
}

// A pool retired while still marked (a mark racing a removal could leave
// one before both took the lock) comes back unmarked when re-created, and
// the next mark clears any such leftover.
func TestRetiredPoolIsNeverMarked(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, retired, is_default) VALUES ('pa', 't1', 'a', 'static', true, true)`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static"})
	if got := defaultPools(t, s); len(got) != 0 {
		t.Fatalf("re-creating a retired pool kept its mark: %v", got)
	}

	// Another left marked, beside a live default: the next mark moves the
	// mark past both and leaves neither retired pool marked.
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, retired, is_default) VALUES ('px', 't1', 'x', 'static', true, true)`)
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static", IsDefault: mark(true)})
	var retiredMarked int
	systemScan(t, s, `SELECT count(*) FROM pools WHERE retired AND is_default`, nil, &retiredMarked)
	if retiredMarked != 0 {
		t.Fatalf("%d retired pools still marked", retiredMarked)
	}
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("defaults %v, want b", got)
	}
}

// Marks and removals of the same pools at once: whatever the order, no
// retired pool is left marked and the tenant never has two defaults.
func TestConcurrentMarkAndDelete(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	for range 20 {
		mustPut(t, s, "t1", Pool{Name: "a", Provider: "static"})
		mustPut(t, s, "t1", Pool{Name: "b", Provider: "static", IsDefault: mark(true)})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() {
			_, errs[0] = s.putPool(tenantCtx("t1"), poolIn(Pool{Name: "a", IsDefault: mark(true)}))
		})
		wg.Go(func() { _, errs[1] = s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "a"}) })
		wg.Wait()
		var he *HTTPError
		if errs[0] != nil && !(errors.As(errs[0], &he) && he.Status == http.StatusNotFound) {
			t.Fatalf("mark: %v", errs[0])
		}
		if errs[1] != nil {
			t.Fatalf("delete: %v", errs[1])
		}
		var retiredMarked int
		systemScan(t, s, `SELECT count(*) FROM pools WHERE retired AND is_default`, nil, &retiredMarked)
		if retiredMarked != 0 {
			t.Fatal("a retired pool is marked")
		}
		// The mark lost to the removal (404) or won and was cleared by it.
		if got := defaultPools(t, s); errs[0] != nil && got["t1"] != "b" || errs[0] == nil && len(got) != 0 {
			t.Fatalf("mark err %v, defaults %v", errs[0], got)
		}
	}
}

// A body without provider only marks the default, so any other field with
// a value would be silently ignored: it is refused, naming the field, and
// the pool is left as it was.
func TestMarkerOnlyBodyIsExact(t *testing.T) {
	s, keys := priceFixture(t)
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", map[string]any{"name": "a", "provider": "static", "maxHosts": 3}); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	for _, extra := range []map[string]any{{"maxHosts": 9}, {"template": map[string]any{"x": 1}}, {"hourlyPrice": "0.40"}, {"MinHosts": 1}} {
		req := map[string]any{"name": "a", "isDefault": true}
		for k, v := range extra {
			req[k] = v
		}
		code, body := call(t, s, keys["t1"], "POST", "/v1/pools", req)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"invalid_pool"`) {
			t.Fatalf("marker with %v: %d %s, want 422 invalid_pool", extra, code, body)
		}
		for k := range extra {
			if !strings.Contains(body, strings.ToLower(k[:1])+k[1:]) {
				t.Fatalf("marker with %v: %s does not name the field", extra, body)
			}
		}
	}
	var maxHosts int
	systemScan(t, s, `SELECT max_hosts FROM pools WHERE name = 'a'`, nil, &maxHosts)
	if got := defaultPools(t, s); len(got) != 0 || maxHosts != 3 {
		t.Fatalf("a refused body changed the pool: defaults %v, maxHosts %d", got, maxHosts)
	}

	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", map[string]any{"name": "a", "isDefault": true}); code != http.StatusOK {
		t.Fatalf("marker-only: %d %s", code, body)
	}
	systemScan(t, s, `SELECT max_hosts FROM pools WHERE name = 'a'`, nil, &maxHosts)
	if got := defaultPools(t, s); got["t1"] != "a" || maxHosts != 3 {
		t.Fatalf("marker-only: defaults %v, maxHosts %d", got, maxHosts)
	}
	// A full body without isDefault keeps the mark.
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", map[string]any{"name": "a", "provider": "static", "maxHosts": 4}); code != http.StatusOK {
		t.Fatalf("full set: %d %s", code, body)
	}
	if got := defaultPools(t, s); got["t1"] != "a" {
		t.Fatalf("a full set without isDefault moved the mark: %v", got)
	}
	// Unknown fields are refused as before.
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", map[string]any{"name": "a", "isDefault": true, "bogus": 1}); code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %d %s", code, body)
	}
}

// The CLI before marker-only bodies were exact sent `pools set NAME
// --default` as a whole Pool with only name and isDefault set: every
// other field zero. That still only moves the mark, as does a key in
// another case (the decoder matches keys case-insensitively); a non-zero
// setting is still refused.
func TestMarkerOnlyBodyFromTheEarlierCLI(t *testing.T) {
	s, keys := priceFixture(t)
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", map[string]any{"name": "a", "provider": "ec2", "maxHosts": 3}); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	// json.Marshal(server.Pool{Name: "a", IsDefault: &true}) at 80e1057.
	earlier := json.RawMessage(`{"name":"a","provider":"","minHosts":0,"maxHosts":0,"warmHosts":0,"scaleDownAfter":"0s","shared":false,"platform":false,"isDefault":true}`)
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", earlier); code != http.StatusOK {
		t.Fatalf("the earlier CLI's marker: %d %s", code, body)
	}
	var provider string
	var maxHosts int
	systemScan(t, s, `SELECT provider, max_hosts FROM pools WHERE name = 'a'`, nil, &provider, &maxHosts)
	if got := defaultPools(t, s); got["t1"] != "a" || provider != "ec2" || maxHosts != 3 {
		t.Fatalf("defaults %v, provider %q, maxHosts %d; want a marked and unchanged", got, provider, maxHosts)
	}

	clear := json.RawMessage(`{"name":"a","template":{},"minHosts":0,"hourlyPrice":"","currency":null,"IsDefault":false}`)
	if code, body := call(t, s, keys["t1"], "POST", "/v1/pools", clear); code != http.StatusOK {
		t.Fatalf("a zero-valued marker with IsDefault: %d %s", code, body)
	}
	if got := defaultPools(t, s); len(got) != 0 {
		t.Fatalf("defaults %v after clearing", got)
	}

	withMax := json.RawMessage(`{"name":"a","provider":"","minHosts":0,"maxHosts":3,"warmHosts":0,"scaleDownAfter":"0s","shared":false,"platform":false,"isDefault":true}`)
	code, body := call(t, s, keys["t1"], "POST", "/v1/pools", withMax)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "maxHosts") {
		t.Fatalf("a marker with maxHosts 3: %d %s, want 422 naming maxHosts", code, body)
	}
	if got := defaultPools(t, s); len(got) != 0 {
		t.Fatalf("a refused marker moved the mark: %v", got)
	}
}
