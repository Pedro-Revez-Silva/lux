package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Renames against default pools (#19) and Runs bound to a pool's owner
// (runs.pool_owner).

func renameStatic(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	if err := s.checkIn(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
}

// runOf is a Run's pool name and pool_owner ("<nil>" for NULL).
func runOf(t *testing.T, s *Server, id string) (string, string) {
	t.Helper()
	var pool, owner string
	systemScan(t, s, `SELECT spec->'placement'->>'pool', coalesce(pool_owner, '<nil>') FROM runs WHERE id = $1`, []any{id}, &pool, &owner)
	return pool, owner
}

// A default pool renamed stays its owner's default under the new name,
// and a Run naming no pool goes there; the Runs that move keep their
// pool_owner; the rename's pool.renamed event says isDefault. A platform
// default renamed changes no tenant's default.
func TestRenameKeepsTheDefaultMark(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	renameStatic(t, s, ctx)
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static", IsDefault: mark(true)})
	bound := submitAs(t, s, "t1", "", "")
	if p, o := runOf(t, s, bound); p != "burst" || o != "t1" {
		t.Fatalf("Run naming no pool: %s owner %s", p, o)
	}

	out, err := renameConfirmed(ctx, s, "t1", "burst", "burst-eu", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Pool.IsDefault == nil || !*out.Pool.IsDefault {
		t.Fatalf("renamed pool %+v lost its default mark", out.Pool)
	}
	if got := defaultPools(t, s); got["t1"] != "burst-eu" {
		t.Fatalf("defaults after the rename: %v", got)
	}
	if p, o := runOf(t, s, bound); p != "burst-eu" || o != "t1" {
		t.Fatalf("moved Run: %s owner %s, want burst-eu owner t1", p, o)
	}
	if got, data := submitPool(t, s, "t1", ""); got != "burst-eu" || data["poolFrom"] != poolFromTenant {
		t.Fatalf("a Run naming no pool after the rename: %s %v", got, data)
	}
	var ev map[string]any
	systemScan(t, s, `SELECT e.data FROM pool_events e JOIN pools p ON p.id = e.pool_id
		WHERE p.name = 'burst-eu' AND e.type = $1`, []any{evRenamed}, &ev)
	if ev["from"] != "burst" || ev["to"] != "burst-eu" || ev["isDefault"] != true || ev["runs"] != float64(1) {
		t.Fatalf("pool.renamed %v", ev)
	}

	// The platform's default renamed: t1 keeps its own, t2 (none of its
	// own) follows the platform's under its new name.
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES ('pp', NULL, 'plat', 'static', true)`)
	if _, err := renameConfirmed(ctx, s, "", "plat", "plat2", false); err != nil {
		t.Fatal(err)
	}
	if got := defaultPools(t, s); got["t1"] != "burst-eu" || got[""] != "plat2" || len(got) != 2 {
		t.Fatalf("defaults after the platform rename: %v", got)
	}
	if got, data := submitPool(t, s, "t2", ""); got != "plat2" || data["poolFrom"] != poolFromPlatform {
		t.Fatalf("t2's Run naming no pool: %s %v", got, data)
	}
	var ev2 map[string]any
	systemScan(t, s, `SELECT data FROM pool_events WHERE pool_id = 'pp' AND type = $1`, []any{evRenamed}, &ev2)
	if ev2["isDefault"] != true {
		t.Fatalf("platform pool.renamed %v", ev2)
	}
}

// Runs follow the pool they are bound to: a tenant's rename of burst
// leaves Runs bound to the platform's burst, and the platform's rename
// Runs bound to the tenant's. Runs bound to no pool row follow by the
// rule the provisioner counts them by.
func TestRenameMovesRunsByTheirOwner(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	renameStatic(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pt', 't1', 'burst', 'static'), ('pp', NULL, 'burst', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_owner) VALUES
		('t1-plat', 't1', '{"placement":{"pool":"burst"}}', 'provisioning', ''),
		('t1-own', 't1', '{"placement":{"pool":"burst"}}', 'provisioning', 't1'),
		('t1-done', 't1', '{"placement":{"pool":"burst"}}', 'succeeded', 't1'),
		('t1-unbound', 't1', '{"placement":{"pool":"burst"}}', 'provisioning', NULL),
		('t2-plat', 't2', '{"placement":{"pool":"burst"}}', 'provisioning', ''),
		('t2-unbound', 't2', '{"placement":{"pool":"burst"}}', 'provisioning', NULL)`)
	want := func(when string, pools map[string]string) {
		t.Helper()
		for id, pool := range pools {
			if got, _ := runOf(t, s, id); got != pool {
				t.Errorf("%s: Run %s names %s, want %s", when, id, got, pool)
			}
		}
	}

	out, err := renameConfirmed(ctx, s, "t1", "burst", "burst-t", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Runs != 2 {
		t.Errorf("tenant rename counted %d Runs, want 2 (t1-own, t1-unbound)", out.Runs)
	}
	want("tenant rename", map[string]string{"t1-plat": "burst", "t1-own": "burst-t", "t1-done": "burst-t",
		"t1-unbound": "burst-t", "t2-plat": "burst", "t2-unbound": "burst"})

	// t1 no longer has a burst of its own: its unbound Runs would follow
	// the platform's now, but t1-unbound already moved with its pool.
	out, err = renameConfirmed(ctx, s, "", "burst", "burst-p", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Runs != 3 {
		t.Errorf("platform rename counted %d Runs, want 3 (t1-plat, t2-plat, t2-unbound)", out.Runs)
	}
	want("platform rename", map[string]string{"t1-plat": "burst-p", "t1-own": "burst-t", "t1-done": "burst-t",
		"t1-unbound": "burst-t", "t2-plat": "burst-p", "t2-unbound": "burst-p"})
	for id, owner := range map[string]string{"t1-plat": "", "t1-own": "t1", "t1-unbound": "<nil>", "t2-plat": ""} {
		if _, got := runOf(t, s, id); got != owner {
			t.Errorf("Run %s pool_owner %s, want %s", id, got, owner)
		}
	}

	// And back: the platform renamed onto a name its bound Runs' tenant
	// owns a pool by is no obstacle (they keep the platform as owner), but
	// an unbound Run of that tenant is.
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pt2', 't2', 'mine', 'static')`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'succeeded' WHERE id = 't2-unbound'`)
	if _, err := renameConfirmed(ctx, s, "", "burst-p", "mine", false); err != nil {
		t.Fatalf("platform rename onto a name only a tenant with bound Runs owns: %v", err)
	}
	if got, owner := runOf(t, s, "t2-plat"); got != "mine" || owner != "" {
		t.Fatalf("t2-plat names %s owner %s, want mine owner platform", got, owner)
	}
}

// A failed Run is final but resumable. Unbound, following the platform's
// shared, it moves with the platform's rename onto gpu, a name its tenant
// also has a pool by: resumed, it goes to the platform's host, not the
// tenant's.
func TestPlatformRenameBindsAFailedUnboundRun(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := testCtx(t)
	renameStatic(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('plat', NULL, 'shared', 'static')`)
	id := submitAs(t, s, "t1", "shared", "")
	execSQL(t, s, ctx, `UPDATE runs SET state = 'failed', pool_owner = NULL WHERE id = $1`, id)
	mustPut(t, s, "t1", Pool{Name: "gpu", Provider: "static"})
	readyHost(t, s, "h-plat", "shared", "", false)
	// The tenant's host is the more attractive one (the image is cached).
	readyHost(t, s, "h-t1", "gpu", "t1", true)

	if _, err := renameConfirmed(ctx, s, "", "shared", "gpu", false); err != nil {
		t.Fatal(err)
	}
	if p, o := runOf(t, s, id); p != "gpu" || o != "" {
		t.Fatalf("failed Run after the platform rename: %s owner %s, want gpu owner platform", p, o)
	}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}}); err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, id); host != "h-plat" {
		t.Fatalf("resumed onto %q, want h-plat (the platform's gpu)", host)
	}
}

// pidTx begins a system-scope transaction held by the test and returns
// it with its backend pid.
func pidTx(t *testing.T, s *Server, ctx context.Context) (pgx.Tx, int) {
	t.Helper()
	tx := systemTx(t, ctx, s)
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

// A rename and a default mark of the same owner, each held open while the
// other starts: the second waits for the first, both then commit, and
// neither deadlocks. A Run naming no pool waits for a rename in flight too,
// and resolves the default under its new name.
func TestRenameAndDefaultMarkWaitForEachOther(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	renameStatic(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, is_default) VALUES
		('pb', 't1', 'burst', 'static', false), ('po', 't1', 'other', 'static', false)`)

	// A mark in flight first: the rename waits for its default lock.
	tx, pid := pidTx(t, s, ctx)
	if err := SetDefaultPool(ctx, tx, "t1", "other", true); err != nil {
		t.Fatal(err)
	}
	renamed := make(chan error, 1)
	go func() {
		_, err := renameConfirmed(ctx, s, "t1", "burst", "burst-eu", false)
		renamed <- err
	}()
	waitBlockedBy(t, s, ctx, pid, renamed)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := await(t, ctx, renamed, 1)[0]; err != nil {
		t.Fatalf("rename after the mark: %v", err)
	}
	if got := defaultPools(t, s); got["t1"] != "other" {
		t.Fatalf("defaults %v, want other", got)
	}

	// A rename in flight first (held on the pool row, after its default
	// and name locks): a mark, and a Run naming no pool, wait for it.
	execSQL(t, s, ctx, `UPDATE pools SET is_default = (name = 'burst-eu') WHERE tenant_id = 't1'`)
	row, rowPID := pidTx(t, s, ctx)
	if _, err := row.Exec(ctx, `SELECT 1 FROM pools WHERE id = 'pb' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := renameConfirmed(ctx, s, "t1", "burst-eu", "burst-us", false)
		renamed <- err
	}()
	waitBlockedOnRow(t, s, ctx, rowPID, renamed)
	marked := make(chan error, 1)
	go func() {
		_, err := s.markDefaultPool(ctx, "t1", "other", true)
		marked <- err
	}()
	blocked(t, marked, "a default mark during a rename")
	submitted := make(chan string, 1)
	go func() {
		out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: noPoolSpec()})
		if err != nil {
			submitted <- "error: " + err.Error()
			return
		}
		submitted <- out.Body.ID
	}()
	blocked(t, submitted, "a Run naming no pool during a rename of the default")
	if err := row.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := await(t, ctx, renamed, 1)[0]; err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := await(t, ctx, marked, 1)[0]; err != nil {
		t.Fatalf("mark after the rename: %v", err)
	}
	id := recv(t, ctx, submitted, "the submission")
	if p, o := runOf(t, s, id); p != "burst-us" && p != "other" || o != "t1" {
		t.Fatalf("Run naming no pool during the rename: %s owner %s (%s), want burst-us or other", p, o, id)
	}
	if got := defaultPools(t, s); got["t1"] != "other" {
		t.Fatalf("defaults %v, want other", got)
	}
}

// waitBlockedOnRow returns once a backend waits on a lock pid holds (a
// row lock: pg_blocking_pids names the holder whatever the lock type).
func waitBlockedOnRow(t *testing.T, s *Server, ctx context.Context, pid int, done <-chan error) {
	t.Helper()
	for {
		var waiting bool
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
				AND $1 = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
		}); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("finished without waiting for %d: %v", pid, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
	}
}

func noPoolSpec() spec.RunSpec {
	return spec.RunSpec{Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}}}
}

// A tenant pool created through SavePool (putPool, luxd admin
// create-pool) while a platform pool is renamed onto its name: the rename
// waits for it, then sees it (the tenant's unbound Runs would resolve to
// it) and is refused.
func TestSavePoolHoldsTheNameAcrossOwners(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	renameStatic(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('plat', NULL, 'shared', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{"placement":{"pool":"shared"}}', 'provisioning')`)

	tx, pid := pidTx(t, s, ctx)
	if err := SavePool(ctx, tx, "t1", "gpu", mark(true), func() error {
		_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('mine', 't1', 'gpu', 'static')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := renameConfirmed(ctx, s, "", "shared", "gpu", false)
		done <- err
	}()
	waitBlockedBy(t, s, ctx, pid, done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if st, code, _ := httpErr(await(t, ctx, done, 1)[0]); st != http.StatusConflict || code != "pool_exists" {
		t.Fatalf("platform rename onto a tenant pool SavePool created meanwhile: %d %s", st, code)
	}
}
