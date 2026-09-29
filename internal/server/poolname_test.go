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
	// The grandfathering is per owner: another tenant cannot create one.
	if _, err := s.putPool(as("t2"), &poolBody{Body: Pool{Name: "Legacy_Pool", Provider: "static"}}); err == nil {
		t.Error("another tenant created a pool under a stored invalid name")
	}
}
