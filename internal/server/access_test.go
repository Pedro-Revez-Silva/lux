package server

import (
	"context"
	"errors"
	"net/http"
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
