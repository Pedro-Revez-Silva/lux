package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

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
	out, err := s.putPool(tenantCtx(tenant), &poolBody{Body: pl})
	if err != nil {
		return Pool{}, err
	}
	return out.Body, nil
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
				_, errs[i] = s.putPool(tenantCtx("t1"), &poolBody{Body: Pool{Name: n, IsDefault: mark(true)}})
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
