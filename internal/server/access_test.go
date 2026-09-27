package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func TestAccessTenantBindingAndDeletion(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfAccess = &cfAccess{}
	s.cfg.ConsoleAuth.CFDefaultTenant = "default"

	// Missing at startup denies tenant access, even if created later.
	if err := s.initCFTenant(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.accessTenant(ctx); !accessForbidden(err) {
		t.Fatalf("missing at startup: got %v, want forbidden", err)
	}
	insert := func(id string) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'default')`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(id string) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("tenant-one")
	if _, err := s.accessTenant(ctx); !accessForbidden(err) {
		t.Fatalf("created after missing startup: got %v, want forbidden", err)
	}
	if err := s.initCFTenant(ctx); err != nil {
		t.Fatal(err)
	}
	if id, err := s.accessTenant(ctx); err != nil || id != "tenant-one" {
		t.Fatalf("bound tenant = %q, %v", id, err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET name = 'renamed' WHERE id = 'tenant-one'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	insert("tenant-two")
	if id, err := s.accessTenant(ctx); err != nil || id != "tenant-one" {
		t.Fatalf("renamed tenant = %q, %v", id, err)
	}
	remove("tenant-one")
	if _, err := s.accessTenant(ctx); !accessForbidden(err) {
		t.Fatalf("replacement with same name: got %v, want forbidden", err)
	}
	if err := s.initCFTenant(ctx); err != nil {
		t.Fatal(err)
	}
	if id, err := s.accessTenant(ctx); err != nil || id != "tenant-two" {
		t.Fatalf("restarted tenant = %q, %v", id, err)
	}
}

func accessForbidden(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusForbidden
}

// The picture comes from the identity's OIDC fields, and only as an https URL.
func TestAccessIdentityPicture(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	for _, tc := range []struct{ body, name, picture string }{
		{`{"name":"Ada","oidc_fields":{"picture":"https://img.example/ada.png"}}`, "Ada", "https://img.example/ada.png"},
		{`{"name":"Ada","oidc_fields":{"picture":"http://img.example/ada.png"}}`, "Ada", ""},
		{`{"name":"Ada","oidc_fields":{"picture":"javascript:alert(1)"}}`, "Ada", ""},
		{`{"name":"Ada"}`, "Ada", ""},
		{`{}`, "ada@example.com", ""},
	} {
		body = tc.body
		a := &cfAccess{team: srv.URL, client: srv.Client(), names: map[string]cachedIdentity{}}
		id := a.identity(context.Background(), "ada@example.com", "token")
		if id.Name != tc.name || id.Picture != tc.picture {
			t.Errorf("%s: got %+v, want name %q picture %q", tc.body, id, tc.name, tc.picture)
		}
	}
}
