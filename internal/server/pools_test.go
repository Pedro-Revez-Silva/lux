package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// A pool's provider changes only once it has no live provisioned host:
// the new provider would not list its instances.
func TestPutPoolRefusesEC2ToStaticWithLiveHosts(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := context.WithValue(f.ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	put := func(provider string) error {
		_, err := f.s.putPool(ctx, poolIn(Pool{Name: "burst", Provider: provider, MaxHosts: 5}))
		return err
	}
	if err := put("static"); err == nil {
		t.Fatal("switched to static with live EC2 hosts")
	} else if status, code, _ := httpErr(err); status != http.StatusConflict || code != "pool_has_hosts" {
		t.Fatalf("provider change: %v", err)
	}
	if pl := f.pool(t); pl.Provider != "ec2" || pl.Max != 0 {
		t.Fatalf("refused update changed pool: provider %s, maxHosts %d", pl.Provider, pl.Max)
	}
	if err := put("ec2"); err != nil {
		t.Fatalf("updating the EC2 pool: %v", err)
	}
	if pl := f.pool(t); pl.Provider != "ec2" || pl.Max != 5 {
		t.Fatalf("EC2 update lost: provider %s, maxHosts %d", pl.Provider, pl.Max)
	}
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET state = 'terminated' WHERE tenant_id = 't1' AND pool = 'burst'`)
	if err := put("static"); err != nil {
		t.Fatalf("switching after all hosts terminated: %v", err)
	}
	if pl := f.pool(t); pl.Provider != "static" {
		t.Fatalf("provider after switch: %s", pl.Provider)
	}
}

// putPool refuses an EC2 template.userData luxd would not know how to
// render, when the pool is set — not silently, and not only discovered
// at launch time.
func TestPutPoolRejectsUnknownUserData(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})

	bad := poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"userData": "cloud-init-yaml"}})
	if _, err := s.putPool(ctx, bad); err == nil {
		t.Fatal("an unknown userData was accepted")
	}

	good := poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"userData": "script"}})
	if _, err := s.putPool(ctx, good); err != nil {
		t.Fatalf("a valid userData was refused: %v", err)
	}

	// "" (unset) defaults to ignition and is not itself an error.
	unset := poolIn(Pool{Name: "burst2", Provider: "ec2", Template: map[string]any{}})
	if _, err := s.putPool(ctx, unset); err != nil {
		t.Fatalf("no userData set was refused: %v", err)
	}

	// A static pool never checks userData: irrelevant there.
	static := poolIn(Pool{Name: "burst3", Provider: "static", Template: map[string]any{"userData": "nonsense"}})
	if _, err := s.putPool(ctx, static); err != nil {
		t.Fatalf("a static pool's template.userData was checked: %v", err)
	}
}

// putPool refuses an EC2 template's lux:* tags: lux sets them on every
// instance and lists a pool's instances by them. Other tags still pass,
// and must be strings.
func TestPutPoolRejectsReservedTemplateTags(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	for _, tags := range []any{
		map[string]any{"lux:pool": "arm64"},
		map[string]any{"LUX:Host": "h"},
		map[string]any{"team": 1},
		"lux:pool=arm64",
		nil, // an explicit "tags": null; an absent key is fine
	} {
		pool := poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"tags": tags}})
		_, err := s.putPool(ctx, pool)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity {
			t.Errorf("tags %#v: err = %v, want a 422 HTTPError", tags, err)
		}
	}
	ok := poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"tags": map[string]any{"team": "platform"}}})
	if _, err := s.putPool(ctx, ok); err != nil {
		t.Fatalf("ordinary template tags were refused: %v", err)
	}
}

// A template.userData that isn't a string (a number or a bool, both valid
// JSON that unmarshal into map[string]any) is refused with 422, not
// stored: JSON later fails to decode it into hostboot's string field on
// every launch attempt, retried forever with no useful error at the point
// that matters.
func TestPutPoolRejectsNonStringUserData(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})

	for _, v := range []any{123, true} {
		pool := poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"userData": v}})
		_, err := s.putPool(ctx, pool)
		if err == nil {
			t.Fatalf("userData %#v (type %T) was accepted", v, v)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity {
			t.Fatalf("userData %#v: err = %v, want a 422 HTTPError", v, err)
		}
	}
}

func poolIn(pl Pool) *poolBody { return &poolBody{Body: poolInput(pl)} }
