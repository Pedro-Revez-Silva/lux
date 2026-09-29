package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

// The lease fence (migration 031): while a pool has a live alias, a luxd
// that never checked in with the pool-rename capability (an older binary)
// cannot take the provisioner lease; with none, it can.

// oldLuxd is a luxd process of an older version on the fixture's
// database: it never checks in.
func (f *renameFixture) oldLuxd() *Server {
	s := New(f.s.cfg, f.s.db, nil, f.s.log)
	s.id = "luxd-old"
	s.deployment = f.s.deployment
	return s
}

func isFenceRefusal(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "LX001"
}

// systemTx begins a transaction in the system scope, held open by the
// test; rolled back at the end unless committed.
func systemTx(t *testing.T, ctx context.Context, s *Server) pgx.Tx {
	t.Helper()
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestProvisionLeaseFence(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx, cancel := context.WithTimeout(f.ctx, raceWait)
	defer cancel()
	old := f.oldLuxd()

	// No alias: a mixed fleet provisions as before.
	l, err := old.provisionLease(ctx)
	if err != nil || l == nil {
		t.Fatalf("an older luxd taking a free lease with no alias: %v, %v", l, err)
	}
	old.releaseProvisionLease()

	f.rename(t, "burst", "burst-eu")
	if _, err := old.provisionLease(ctx); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking the lease with a live alias: %v, want the fence's refusal", err)
	}
	// Nor after the lease expired from a luxd that holds it.
	takeLease(t, f.s)
	execSQL(t, f.s, ctx, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	if _, err := old.provisionLease(ctx); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking over an expired lease with a live alias: %v", err)
	}
	// A luxd that checked in may.
	s2 := f.otherLuxd(t)
	if l, err := s2.provisionLease(ctx); err != nil || l == nil {
		t.Fatalf("a luxd that checked in: %v, %v", l, err)
	}
	// And once the alias is retired, the older one may again.
	execSQL(t, f.s, ctx, `UPDATE pool_tag_aliases SET retired_at = now()`)
	execSQL(t, f.s, ctx, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	if l, err := old.provisionLease(ctx); err != nil || l == nil {
		t.Fatalf("an older luxd once no alias is live: %v, %v", l, err)
	}
}

// An older luxd takes the lease in a transaction that has not committed
// when a rename begins: the rename waits for it, then sees the holder and
// is refused. Without the lock, it would read the lease as free, and add
// an alias the older luxd's provisioning ignores.
func TestRenameWaitsForALeaseAcquisition(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx, cancel := context.WithTimeout(f.ctx, raceWait)
	defer cancel()
	tx := systemTx(t, ctx, f.s)
	if _, err := tx.Exec(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', 'luxd-old', now() + interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := renameConfirmed(ctx, f.s, "t1", "burst", "burst-eu", false)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the rename did not wait for the lease's acquisition: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err := recv(t, done, "the rename")
	if _, code, _ := httpErr(err); code != "rename_unsupported_by_deployment" {
		t.Fatalf("rename after an older luxd took the lease: %v", err)
	}
	if n := f.query(t, `SELECT count(*)::text FROM pool_tag_aliases`); n != "0" {
		t.Fatalf("%s aliases", n)
	}
}

// A rename holds the lease row until it commits; an older luxd taking the
// lease meanwhile waits, then is refused: the alias is there.
func TestLeaseAcquisitionWaitsForARename(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx, cancel := context.WithTimeout(f.ctx, raceWait)
	defer cancel()
	tx := systemTx(t, ctx, f.s)
	// What the rename's transaction does, up to its commit.
	if err := lockProvisionerLease(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkDeploymentCanRename(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pool_tag_aliases (pool_id, tenant_id, name) VALUES ('pool1', 't1', 'burst')`); err != nil {
		t.Fatal(err)
	}
	old := f.oldLuxd()
	done := make(chan error, 1)
	go func() {
		_, err := old.provisionLease(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the older luxd did not wait for the rename: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, done, "the older luxd's lease"); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking the lease after the rename committed: %v", err)
	}
}

// A luxd checks in before its first attempt at the provisioner lease: the
// fence would refuse it otherwise while an alias is live.
func TestProvisionerChecksInBeforeTheLease(t *testing.T) {
	s := testServer(t)
	s.cfg.Tick = 10 * time.Millisecond
	s.cfg.Providers = map[string]Provider{"ec2": newFakeCloud()}
	ctx, cancel := context.WithTimeout(context.Background(), raceWait)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO settings (name, value) VALUES ('deployment', 'd1') ON CONFLICT (name) DO NOTHING`)
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.provisionerLoop(loop) }()
	defer func() { stop(); <-done }()
	holder := func() string {
		var h string
		_ = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce((SELECT holder FROM leases WHERE name = 'provisioner'), '')`).Scan(&h)
		})
		return h
	}
	time.Sleep(20 * s.cfg.Tick)
	if h := holder(); h != "" {
		t.Fatalf("lease taken by %q before this luxd checked in", h)
	}
	go s.checkInLoop(loop)
	for holder() != s.id {
		select {
		case <-ctx.Done():
			t.Fatal("the lease was never taken after the check-in")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// holdPoolRow locks pool id's row in a transaction the test holds: a
// rename of that pool waits there, after it took its name locks.
func holdPoolRow(t *testing.T, ctx context.Context, s *Server, id string) pgx.Tx {
	t.Helper()
	tx := systemTx(t, ctx, s)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM pools WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	return tx
}

func blocked[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s did not wait: %v", what, v)
	case <-time.After(300 * time.Millisecond):
	}
}

// A host token minted for a pool while it is renamed: the mint waits for
// the rename, then is refused, naming the new name; minted first, the
// rename waits for it and moves the token. Never a token for the old name.
func TestHostTokenDuringARename(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx, cancel := context.WithTimeout(f.ctx, raceWait)
	defer cancel()
	t1 := "t1"
	mint := func(pool string) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				_, err := CreateHostToken(ctx, tx, &t1, pool, nil)
				return err
			})
		}()
		return done
	}
	rename := func(from, to string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := renameConfirmed(ctx, f.s, "t1", from, to, false)
			done <- err
		}()
		return done
	}

	// The rename first: it holds the name while it waits for the pool row.
	row := holdPoolRow(t, ctx, f.s, "pool1")
	renamed := rename("burst", "burst-eu")
	time.Sleep(200 * time.Millisecond)
	minted := mint("burst")
	blocked(t, minted, "a mint during the rename")
	if err := row.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	err := recv(t, minted, "the mint")
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "pool_name_reserved" || !strings.Contains(msg, "burst-eu") {
		t.Fatalf("a mint for the old name after the rename: %v", err)
	}

	// The mint first: the rename waits, and moves the token.
	execSQL(t, f.s, ctx, `UPDATE pool_tag_aliases SET finished_at = now()`)
	execSQL(t, f.s, ctx, `UPDATE pools SET rename_finished_at = now() - interval '1 day'`)
	tx := systemTx(t, ctx, f.s)
	if _, err := CreateHostToken(ctx, tx, &t1, "burst-eu", nil); err != nil {
		t.Fatal(err)
	}
	renamed = rename("burst-eu", "burst-us")
	blocked(t, renamed, "a rename during a mint")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM host_tokens`); got != "burst-us" {
		t.Fatalf("host tokens name %s, want burst-us only", got)
	}
}
