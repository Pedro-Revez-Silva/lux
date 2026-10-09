package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// BeforeCommit runs each key's hook once, in first-registered order, after
// fn and inside the transaction, including a hook registered by a hook; a
// hook's error rolls the whole transaction back; a savepoint registers
// nothing.
func TestBeforeCommit(t *testing.T) {
	_, appDSN := testDB(t)
	ctx := context.Background()
	db, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rename := func(tx pgx.Tx, name string) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET name = $1 WHERE id = 't1'`, name)
		return err
	}
	name := func() string {
		var n string
		if err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT name FROM tenants WHERE id = 't1'`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var ran []string
	err = db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		store.BeforeCommit(tx, "a", func() error {
			ran = append(ran, "a")
			store.BeforeCommit(tx, "c", func() error { ran = append(ran, "c"); return rename(tx, "hooked") })
			return nil
		})
		store.BeforeCommit(tx, "b", func() error { ran = append(ran, "b"); return nil })
		store.BeforeCommit(tx, "a", func() error { ran = append(ran, "a again"); return nil })
		if len(ran) != 0 {
			t.Errorf("hooks ran before fn returned: %v", ran)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if store.BeforeCommit(sp, "sp", func() error { ran = append(ran, "sp"); return nil }) {
			t.Error("a savepoint accepted a hook")
		}
		return sp.Commit(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(ran, want) {
		t.Fatalf("hooks ran %v, want %v", ran, want)
	}
	if n := name(); n != "hooked" {
		t.Fatalf("name %q: the hook's write was not committed", n)
	}

	boom := errors.New("boom")
	err = db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		store.BeforeCommit(tx, "fail", func() error { return boom })
		return rename(tx, "rolled back")
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want the hook's error", err)
	}
	if n := name(); n != "hooked" {
		t.Fatalf("name %q: a failing hook did not roll fn's write back", n)
	}
}
