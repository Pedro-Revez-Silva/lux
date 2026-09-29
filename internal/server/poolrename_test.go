package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A rename changes only the pool's name: its host and a Run bound to it
// stay with it, a new Run naming the new name lands on the same host, the
// old name finds no pool, the default mark stays, and pool.renamed says
// from and to. A taken name is a 409 pool_exists.
func TestRenamePoolKeepsHostsAndRuns(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	pl := mustPut(t, s, "t1", Pool{Name: "old", Provider: "static", IsDefault: mark(true)})
	mustPut(t, s, "t1", Pool{Name: "taken", Provider: "static"})
	id := poolID(t, s, "old", "t1")
	readyHost(t, s, "h1", "old", "t1", false)
	waiting := submitAs(t, s, "t1", "old", "")

	out, err := s.renamePool(tenantCtx("t1"), &renamePoolInput{Name: pl.Name, Body: struct {
		Name string `json:"name" doc:"The new name."`
	}{Name: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Name != "new" || out.Body.IsDefault == nil || !*out.Body.IsDefault {
		t.Fatalf("renamed pool %+v, want new and still the default", out.Body)
	}
	if got := poolID(t, s, "new", "t1"); got != id {
		t.Fatalf("new name is pool %s, want %s", got, id)
	}
	var from, to string
	systemScan(t, s, `SELECT data->>'from', data->>'to' FROM pool_events WHERE pool_id = $1 AND type = $2`, []any{id, evRenamed}, &from, &to)
	if from != "old" || to != "new" {
		t.Fatalf("pool.renamed from %q to %q", from, to)
	}

	fresh := submitAs(t, s, "t1", "new", "")
	stale := submitAs(t, s, "t1", "old", "")
	schedule(t, s)
	for _, r := range []string{waiting, fresh} {
		if host, _, _ := placedOn(t, s, r); host != "h1" {
			t.Errorf("Run %s placed on %q, want h1", r, host)
		}
	}
	if _, owner, _ := runPool(t, s, stale); owner != "<nil>" {
		t.Errorf("a Run naming the old name was bound to a pool (owner %q)", owner)
	}
	if host, _, _ := placedOn(t, s, stale); host != "" {
		t.Errorf("a Run naming the old name was placed on %q", host)
	}
	// Lists show the pool's current name; the spec keeps the one submitted.
	r, err := s.getRun(tenantCtx("t1"), &RunPath{ID: waiting})
	if err != nil {
		t.Fatal(err)
	}
	if r.Body.Pool != "new" || r.Body.PoolID != id || r.Body.Spec.Placement.Pool != "old" {
		t.Errorf("Run pool %q (%s), spec %q; want new, %s, old", r.Body.Pool, r.Body.PoolID, r.Body.Spec.Placement.Pool, id)
	}

	_, err = s.renamePool(tenantCtx("t1"), &renamePoolInput{Name: "new", Body: struct {
		Name string `json:"name" doc:"The new name."`
	}{Name: "taken"}})
	if he := (*HTTPError)(nil); !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "pool_exists" {
		t.Fatalf("rename onto a taken name: %v, want 409 pool_exists", err)
	}
}
