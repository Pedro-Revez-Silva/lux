package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func TestValidPoolName(t *testing.T) {
	for _, ok := range []string{"a", "7", "default", "arm64", "gpu-a100", "a-b-c", strings.Repeat("x", MaxPoolName)} {
		if err := ValidPoolName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "Arm64", "arm_64", "a.b", "a/b", "a b", "ü", strings.Repeat("x", MaxPoolName+1)} {
		if ValidPoolName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// putPool refuses a new pool whose name breaks the rule with 422, but a
// pool stored before the rule keeps its name and can still be updated.
func TestPutPoolName(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p_legacy', 't1', 'Legacy_Pool', 'static')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	as := func(tenant string) context.Context {
		return context.WithValue(ctx, principalKey, Principal{TenantID: tenant, Scopes: []string{"admin"}})
	}
	for _, name := range []string{"Burst", "burst_1", "-burst", strings.Repeat("b", MaxPoolName+1)} {
		_, err := s.putPool(as("t1"), &poolBody{Body: Pool{Name: name, Provider: "static"}})
		var he *HTTPError
		if err == nil || !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity {
			t.Errorf("new pool %q: err %v, want 422", name, err)
		}
	}
	if _, err := s.putPool(as("t1"), &poolBody{Body: Pool{Name: "burst-1", Provider: "static"}}); err != nil {
		t.Errorf("a valid name was refused: %v", err)
	}
	if _, err := s.putPool(as("t1"), &poolBody{Body: Pool{Name: "Legacy_Pool", Provider: "static", MaxHosts: 3}}); err != nil {
		t.Errorf("a stored pool's own name was refused on update: %v", err)
	}
	var maxHosts int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT max_hosts FROM pools WHERE tenant_id = 't1' AND name = 'Legacy_Pool'`).Scan(&maxHosts)
	}); err != nil || maxHosts != 3 {
		t.Errorf("legacy pool not updated: max_hosts %d, %v", maxHosts, err)
	}
	// The grandfathering is per owner: another tenant cannot create one.
	_, err := s.putPool(as("t2"), &poolBody{Body: Pool{Name: "Legacy_Pool", Provider: "static"}})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_pool" {
		t.Errorf("another tenant, stored invalid name: err %v, want 422 invalid_pool", err)
	}
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pools WHERE tenant_id = 't2'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("a refused pool was stored: %d rows, %v", n, err)
	}
}

// A static pool can exist with no pool row: hosts joined it by host token.
// Such a name, used before the rule, is kept for its owner, so a
// replacement token (or a pool row) can still be created for it.
func TestCheckPoolNameKeepsNamesInUse(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('ht1', 't1', 'Old_Static', 'x1')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('h1', NULL, 'h1', 'Plat_Static', 'ready')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t1 := "t1"
	for _, c := range []struct {
		name   string
		tenant *string
		pool   string
		ok     bool
	}{
		{"token in use", &t1, "Old_Static", true},
		{"host in use (platform)", nil, "Plat_Static", true},
		{"another owner's token", nil, "Old_Static", false},
		{"unused", &t1, "New_Static", false},
		{"valid", &t1, "new-static", true},
	} {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return CheckPoolName(ctx, tx, c.tenant, c.pool) })
		var pne *PoolNameError
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case !c.ok && !errors.As(err, &pne):
			t.Errorf("%s: err %v, want a *PoolNameError", c.name, err)
		}
	}
}
