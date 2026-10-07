package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

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

// template.nestedContainers is an ec2 pool's JSON boolean: a string or a
// number would read as false at launch and strand nested Runs, and a static
// pool's hosts get it from lux-runner --nested, not from the pool.
func TestPutPoolValidatesNestedContainers(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	for i, v := range []any{"true", 1, nil, map[string]any{}} {
		bad := poolIn(Pool{Name: fmt.Sprintf("bad%d", i), Provider: "ec2", Template: map[string]any{"nestedContainers": v}})
		if _, err := s.putPool(ctx, bad); err == nil || !strings.Contains(err.Error(), "nestedContainers must be true or false") {
			t.Errorf("nestedContainers %#v: err %v", v, err)
		}
	}
	for i, v := range []bool{true, false} {
		good := poolIn(Pool{Name: fmt.Sprintf("good%d", i), Provider: "ec2", Template: map[string]any{"nestedContainers": v}})
		if _, err := s.putPool(ctx, good); err != nil {
			t.Errorf("nestedContainers %v refused: %v", v, err)
		}
	}
	static := poolIn(Pool{Name: "static", Provider: "static", Template: map[string]any{"nestedContainers": true}})
	if _, err := s.putPool(ctx, static); err == nil || !strings.Contains(err.Error(), "is for ec2 pools") {
		t.Errorf("a static pool's nestedContainers: err %v", err)
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

// template.fallbackInstanceTypes: an ec2 pool's array of distinct type
// names, tried after its instanceType, which it needs; at most four, so a
// launch's attempts stay bounded.
func TestPutPoolValidatesFallbackInstanceTypes(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	withType := func(fb any) map[string]any {
		return map[string]any{"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": fb}
	}
	// Each cause says which it is, not "the instanceType or listed twice".
	for i, c := range []struct {
		template map[string]any
		want     string
	}{
		{withType("m7g.2xlarge"), "template.fallbackInstanceTypes must be an array of strings, got string"},
		{withType(nil), "template.fallbackInstanceTypes must be an array of strings, got <nil>"},
		{withType([]any{"m7g.2xlarge", 1}), "template.fallbackInstanceTypes[1] must be a non-empty string, got 1"},
		{withType([]any{""}), `template.fallbackInstanceTypes[0] must be a non-empty string, got ""`},
		{withType([]any{nil}), "template.fallbackInstanceTypes[0] must be a non-empty string, got <nil>"},
		{withType([]any{"m7g.2xlarge", "m7g.2xlarge"}), `template.fallbackInstanceTypes[1] "m7g.2xlarge" is listed twice`},
		{withType([]any{"m8g.2xlarge"}), `template.fallbackInstanceTypes[0] "m8g.2xlarge" is the instanceType`},
		{withType([]any{"m7g.2xlarge", "m8g.2xlarge"}), `template.fallbackInstanceTypes[1] "m8g.2xlarge" is the instanceType`},
		{withType([]any{"a", "b", "c", "d", "e"}), "template.fallbackInstanceTypes has 5 entries; at most 4"},
		{map[string]any{"fallbackInstanceTypes": []any{"m7g.2xlarge"}},
			"template.fallbackInstanceTypes needs template.instanceType: the fallbacks are tried after it"},
		{map[string]any{"instanceType": "", "fallbackInstanceTypes": []any{"m7g.2xlarge"}},
			"template.fallbackInstanceTypes needs template.instanceType: the fallbacks are tried after it"},
	} {
		_, err := s.putPool(ctx, poolIn(Pool{Name: fmt.Sprintf("bad%d", i), Provider: "ec2", Template: c.template}))
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_pool" || err.Error() != c.want {
			t.Errorf("%v: err %v, want 422 invalid_pool %q", c.template, err, c.want)
		}
	}
	for i, fb := range []any{[]any{}, []any{"m7g.2xlarge"}, []any{"m7g.2xlarge", "c8g.2xlarge", "c7g.2xlarge", "r8g.xlarge"}} {
		pool := poolIn(Pool{Name: fmt.Sprintf("good%d", i), Provider: "ec2", Template: withType(fb)})
		if _, err := s.putPool(ctx, pool); err != nil {
			t.Errorf("fallbackInstanceTypes %v refused: %v", fb, err)
		}
	}
	static := poolIn(Pool{Name: "static", Provider: "static", Template: withType([]any{"m7g.2xlarge"})})
	if _, err := s.putPool(ctx, static); err == nil || !strings.Contains(err.Error(), "is for ec2 pools") {
		t.Errorf("a static pool's fallbackInstanceTypes: err %v", err)
	}
}

func poolIn(pl Pool) *poolBody { return &poolBody{Body: poolInput(pl)} }
