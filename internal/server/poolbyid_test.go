package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

func submitPoolID(s *Server, tenant, pool, poolID string) (*submitRunOutput, error) {
	return s.submitRun(tenantCtx(tenant), &submitRunInput{Body: spec.RunSpec{
		Image:     spec.Image{Ref: "alpine"},
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Placement: spec.Placement{Pool: pool, PoolID: poolID},
	}})
}

func wantRefused(t *testing.T, err error, code string) *HTTPError {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != code {
		t.Fatalf("got %v, want 422 %s", err, code)
	}
	return he
}

// A Run naming its pool by id is placed in that pool, which its stored
// spec and submitted event name, and stays there after a rename.
func TestPoolByID(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "gpu", Provider: "static"})
	mustPut(t, s, "t1", Pool{Name: "default", Provider: "static", IsDefault: mark(true)})
	id := poolID(t, s, "gpu", "t1")
	readyHost(t, s, "h-gpu", "gpu", "t1", false)
	readyHost(t, s, "h-default", "default", "t1", true)

	out, err := submitPoolID(s, "t1", "", id)
	if err != nil {
		t.Fatal(err)
	}
	run := out.Body.ID
	var pool, storedID, evPool, evFrom, evOwner, boundTo string
	systemScan(t, s, `SELECT r.spec->'placement'->>'pool', r.spec->'placement'->>'poolId', e.data->>'pool', e.data->>'poolFrom', e.data->>'poolOwner', r.pool_id
		FROM runs r JOIN run_events e ON e.run_id = r.id AND e.type = 'submitted' WHERE r.id = $1`,
		[]any{run}, &pool, &storedID, &evPool, &evFrom, &evOwner, &boundTo)
	if pool != "gpu" || storedID != id || evPool != "gpu" || evFrom != "spec" || evOwner != "tenant" || boundTo != id {
		t.Fatalf("stored pool %q id %q, event %q/%q/%q, bound to %q; want gpu, %s, gpu/spec/tenant, %s",
			pool, storedID, evPool, evFrom, evOwner, boundTo, id, id)
	}
	if out.Body.PoolID != id || out.Body.Spec.Placement.Pool != "gpu" {
		t.Fatalf("returned Run pool %s, spec pool %q", out.Body.PoolID, out.Body.Spec.Placement.Pool)
	}

	rename := &renamePoolInput{Name: "gpu"}
	rename.Body.Name = "gpu2"
	if _, err := s.renamePool(tenantCtx("t1"), rename); err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, run); host != "h-gpu" {
		t.Fatalf("Run by id placed on %q, want h-gpu", host)
	}
	// The id still finds the renamed pool for a new Run.
	again, err := submitPoolID(s, "t1", "", id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Body.PoolID != id || again.Body.Spec.Placement.Pool != "gpu2" {
		t.Fatalf("after rename: pool %s %q, want %s gpu2", again.Body.PoolID, again.Body.Spec.Placement.Pool, id)
	}
}

// An id no pool of the tenant or the platform has is refused, and no Run
// is created: an unknown id, another tenant's pool, a removed pool.
func TestPoolByIDUnknown(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	mustPut(t, s, "t2", Pool{Name: "theirs", Provider: "static"})
	theirs := poolID(t, s, "theirs", "t2")
	mustPut(t, s, "t1", Pool{Name: "gone", Provider: "static"})
	gone := poolID(t, s, "gone", "t1")
	if _, err := s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "gone"}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"pool_nosuchpool", theirs, gone} {
		_, err := submitPoolID(s, "t1", "", id)
		if he := wantRefused(t, err, "unknown_pool"); he.Message != "no pool has id "+id {
			t.Errorf("message %q", he.Message)
		}
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d Runs created by refused submits", n)
	}
}

// Naming the pool both ways is an invalid spec.
func TestPoolByIDAndName(t *testing.T) {
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "gpu", Provider: "static"})
	_, err := submitPoolID(s, "t1", "gpu", poolID(t, s, "gpu", "t1"))
	he := wantRefused(t, err, "invalid_spec")
	if len(he.Details) != 1 || he.Details[0] != "placement: name the pool by pool or poolId, not both" {
		t.Fatalf("details %q", he.Details)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d Runs created", n)
	}
}

// A tenant names a platform pool by id and is placed on its host.
func TestPoolByIDPlatform(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared) VALUES ('pool_plat', NULL, 'plat', 'static', true)`)
	readyHost(t, s, "h-plat", "plat", "", false)

	out, err := submitPoolID(s, "t1", "", "pool_plat")
	if err != nil {
		t.Fatal(err)
	}
	if _, owner, ev := runPool(t, s, out.Body.ID); owner != "" || ev != "platform" || out.Body.Spec.Placement.Pool != "plat" {
		t.Fatalf("owner %q event %q pool %q, want the platform's plat", owner, ev, out.Body.Spec.Placement.Pool)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, out.Body.ID); host != "h-plat" {
		t.Fatalf("placed on %q, want h-plat", host)
	}
}
