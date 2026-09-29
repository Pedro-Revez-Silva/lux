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

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// The lease fence (migration 036): once a provisioned pool has been
// renamed, a luxd that never checked in with the pool-id-discovery
// capability (an older binary) cannot take the provisioner lease; before,
// it can.

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
	t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx)
	})
	if _, err := tx.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestProvisionLeaseFence(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	old := f.oldLuxd()

	// No rename yet: a mixed fleet provisions as before. A static pool's
	// rename changes nothing an older luxd lists.
	execSQL(t, f.s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ps', 't1', 'lab', 'static')`)
	f.rename(t, "lab", "lab2")
	l, err := old.provisionLease(ctx)
	if err != nil || l == nil {
		t.Fatalf("an older luxd taking a free lease before any rename: %v, %v", l, err)
	}
	old.releaseProvisionLease()

	f.rename(t, "burst", "burst-eu")
	if _, err := old.provisionLease(ctx); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking the lease after a rename: %v, want the fence's refusal", err)
	}
	// Nor after the lease expired from a luxd that holds it.
	takeLease(t, ctx, f.s)
	execSQL(t, f.s, ctx, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	if _, err := old.provisionLease(ctx); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking over an expired lease after a rename: %v", err)
	}
	// A luxd that checked in may.
	s2 := f.otherLuxd(t)
	if l, err := s2.provisionLease(ctx); err != nil || l == nil {
		t.Fatalf("a luxd that checked in: %v, %v", l, err)
	}
}

// An older luxd takes the lease in a transaction that has not committed
// when a rename begins: the rename waits for it, then sees the holder and
// is refused. Without the lock, it would read the lease as free, and
// rename a pool the older luxd then lists by its new name only.
func TestRenameWaitsForALeaseAcquisition(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
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
	err := recv(t, ctx, done, "the rename")
	if _, code, _ := httpErr(err); code != "rename_unsupported_by_deployment" {
		t.Fatalf("rename after an older luxd took the lease: %v", err)
	}
	if pl := f.pool(t); pl.Name != "burst" || pl.RenamedAt != nil {
		t.Fatalf("pool %s renamed at %v", pl.Name, pl.RenamedAt)
	}
}

// A rename holds the lease row until it commits; an older luxd taking the
// lease meanwhile waits, then is refused: the pool has been renamed.
func TestLeaseAcquisitionWaitsForARename(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	tx := systemTx(t, ctx, f.s)
	// What the rename's transaction does, up to its commit.
	if err := lockProvisionerLease(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkDeploymentCanRename(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE pools SET name = 'burst-eu', renamed_at = now() WHERE id = 'pool1'`); err != nil {
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
	if err := recv(t, ctx, done, "the older luxd's lease"); !isFenceRefusal(err) {
		t.Fatalf("an older luxd taking the lease after the rename committed: %v", err)
	}
}

// A luxd checks in before its first attempt at the provisioner lease: the
// fence would refuse it otherwise once a pool has been renamed.
func TestProvisionerChecksInBeforeTheLease(t *testing.T) {
	s := testServer(t)
	s.cfg.Tick = 10 * time.Millisecond
	s.cfg.Providers = map[string]Provider{"ec2": newFakeCloud()}
	ctx := testCtx(t)
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
	ctx := f.ctx
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
	if err := recv(t, ctx, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	err := recv(t, ctx, minted, "the mint")
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "pool_renamed" || !strings.Contains(msg, "burst-eu") {
		t.Fatalf("a mint for the old name after the rename: %v", err)
	}

	// The mint first: the rename waits, and moves the token.
	tx := systemTx(t, ctx, f.s)
	if _, err := CreateHostToken(ctx, tx, &t1, "burst-eu", nil); err != nil {
		t.Fatal(err)
	}
	renamed = rename("burst-eu", "burst-us")
	blocked(t, renamed, "a rename during a mint")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, ctx, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM host_tokens`); got != "burst-us" {
		t.Fatalf("host tokens name %s, want burst-us only", got)
	}
}

// After a static pool's rename, a token for its old name is refused
// (pool_renamed, naming the new name) as long as no pool of the owner has
// that name: minting never creates a pool. Once a pool takes the name, it
// is that pool's; another owner's old names are no obstacle; and a name
// no pool ever had still mints (a static pool needs no row).
func TestHostTokenForARenamedStaticPool(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	t1 := "t1"
	mint := func(tenant *string, pool string) error {
		return f.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := CreateHostToken(ctx, tx, tenant, pool, nil)
			return err
		})
	}
	execSQL(t, f.s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ps', 't1', 'lab', 'static')`)
	f.rename(t, "lab", "lab2")
	err := mint(&t1, "lab")
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "pool_renamed" || !strings.Contains(msg, "lab2") {
		t.Fatalf("a token for the renamed pool's old name: %v", err)
	}
	if n := f.query(t, `SELECT count(*)::text FROM pools WHERE name = 'lab'`); n != "0" {
		t.Fatalf("minting created %s pools", n)
	}
	if err := mint(&t1, "lab2"); err != nil {
		t.Fatalf("a token for the new name: %v", err)
	}
	if err := mint(nil, "lab"); err != nil {
		t.Fatalf("a platform token for a name only a tenant's pool had: %v", err)
	}
	if err := mint(&t1, "never-a-pool"); err != nil {
		t.Fatalf("a token for a pool with no row: %v", err)
	}
	pctx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	if _, err := f.s.putPool(pctx, &poolBody{Body: Pool{Name: "lab", Provider: "static"}}); err != nil {
		t.Fatal(err)
	}
	if err := mint(&t1, "lab"); err != nil {
		t.Fatalf("a token for a new pool under the old name: %v", err)
	}
}

// A Run submitted while its pool is renamed: the submission waits for the
// rename, then is refused, naming the new name; submitted first, the
// rename waits for it and moves it. Never a Run left under the old name.
// A Run for the new name, or for a name some pool has again, is accepted.
func TestSubmitRunDuringARename(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	submit := func(pool string) <-chan error {
		done := make(chan error, 1)
		go func() {
			sctx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"run"}})
			in := &submitRunInput{}
			in.Body.Image.Ref = "alpine"
			in.Body.Workload = spec.Workload{Adapter: "generic", Command: []string{"true"}}
			in.Body.Placement.Pool = pool
			_, err := f.s.submitRun(sctx, in)
			done <- err
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
	pools := func() string {
		return f.query(t, `SELECT string_agg(spec->'placement'->>'pool', ',' ORDER BY created_at) FROM runs WHERE state = 'submitted'`)
	}

	// The rename first.
	row := holdPoolRow(t, ctx, f.s, "pool1")
	renamed := rename("burst", "burst-eu")
	time.Sleep(200 * time.Millisecond)
	submitted := submit("burst")
	blocked(t, submitted, "a submission during the rename")
	if err := row.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, ctx, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	err := recv(t, ctx, submitted, "the submission")
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "pool_renamed" || !strings.Contains(msg, "burst-eu") {
		t.Fatalf("a Run for the old name after the rename: %v", err)
	}

	// The submission first: its transaction holds the name until it
	// commits, and the rename then moves the Run.
	tx := systemTx(t, ctx, f.s)
	if err := checkRunPool(ctx, tx, "t1", "burst-eu"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('mid', 't1', '{"placement":{"pool":"burst-eu"}}', 'submitted')`); err != nil {
		t.Fatal(err)
	}
	renamed = rename("burst-eu", "burst-us")
	blocked(t, renamed, "a rename during a submission")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, ctx, renamed, "the rename"); err != nil {
		t.Fatal(err)
	}
	if got := pools(); got != "burst-us" {
		t.Fatalf("submitted Runs name %s, want burst-us", got)
	}
	if err := recv(t, ctx, submit("burst-us"), "a submission to the new name"); err != nil {
		t.Fatal(err)
	}
	execSQL(t, f.s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('again', 't1', 'burst', 'static')`)
	if err := recv(t, ctx, submit("burst"), "a submission to a pool that took the old name"); err != nil {
		t.Fatal(err)
	}
}

// A tenant creating a pool named like a platform pool's new name while it
// is renamed: the rename waits for it, then sees it (the tenant's Runs
// that follow the platform pool would resolve to it) and is refused.
func TestTenantPoolCreatedDuringAPlatformRename(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	if err := s.checkIn(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'acme')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('plat', NULL, 'shared', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{"placement":{"pool":"shared"}}', 'provisioning')`)

	tx := systemTx(t, ctx, s)
	if err := LockPoolName(ctx, tx, "t1", "gpu"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('mine', 't1', 'gpu', 'static')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := renameConfirmed(ctx, s, "", "shared", "gpu", false)
		done <- err
	}()
	blocked(t, done, "a platform rename during a tenant's pool creation")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err := recv(t, ctx, done, "the rename")
	if st, code, _ := httpErr(err); st != http.StatusConflict || code != "pool_exists" {
		t.Fatalf("platform rename onto a tenant's pool created meanwhile: %v", err)
	}
}
