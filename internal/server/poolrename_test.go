package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// fakeCloud is a provider whose instances carry tags, listed by them.
type fakeCloud struct {
	mu    sync.Mutex
	insts map[string]*fakeInstance
	// deny refuses Retag, as EC2 does without IAM's ec2:CreateTags.
	deny bool
	// hidden instances are missing from every listing (tag filters lag).
	hidden     map[string]bool
	terminated []string
	launched   int
}

type fakeInstance struct {
	state string
	tags  map[string]string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{insts: map[string]*fakeInstance{}, hidden: map[string]bool{}}
}

func (c *fakeCloud) add(pid string, tags map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insts[pid] = &fakeInstance{state: "running", tags: maps.Clone(tags)}
}

func (c *fakeCloud) tag(pid, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.insts[pid].tags[key]
}

func (c *fakeCloud) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (Launched, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launched++
	return Launched{}, errors.New("fakeCloud: no launches in this test")
}

func (c *fakeCloud) Terminate(ctx context.Context, template json.RawMessage, pid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminated = append(c.terminated, pid)
	if i := c.insts[pid]; i != nil {
		i.state = "terminated"
	}
	return nil
}

func (c *fakeCloud) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]Instance{}
	for pid, i := range c.insts {
		match := !c.hidden[pid]
		for k, v := range tags {
			match = match && i.tags[k] == v
		}
		if match {
			out[pid] = Instance{State: i.state, Tags: maps.Clone(i.tags)}
		}
	}
	return out, nil
}

func (c *fakeCloud) Retag(ctx context.Context, template json.RawMessage, pids []string, key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deny {
		return errors.New("UnauthorizedOperation: not authorized to perform ec2:CreateTags (fake)")
	}
	for _, pid := range pids {
		if i := c.insts[pid]; i != nil {
			i.tags[key] = value
		}
	}
	return nil
}

// renameFixture: tenant t1's ec2 pool "burst" with two live hosts (i-1,
// i-2, tagged t1/burst), a Run waiting for it, a finished Run that ran on
// it, and its host token.
type renameFixture struct {
	s     *Server
	ctx   context.Context
	cloud *fakeCloud
}

func newRenameFixture(t *testing.T, settled bool) *renameFixture {
	t.Helper()
	s := testServer(t)
	s.deployment = "d1"
	s.cfg.ListingLag = 300 * time.Millisecond
	ctx := context.Background()
	cloud := newFakeCloud()
	s.cfg.Providers = map[string]Provider{"ec2": cloud}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES ('pool1', 't1', 'burst', 'ec2', '{"region":"eu-west-1"}')`)
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('tok1', 't1', 'burst', 'x1'), ('tok2', 't1', 'burst', 'x2')`)
	// Settled: launched long ago, runner silent: a host its listing misses
	// is written off. Otherwise heartbeating, as a live host is.
	heartbeat := "now()"
	if settled {
		heartbeat = "NULL"
	}
	for _, h := range []string{"1", "2"} {
		execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, token_id, state, provider_id, provision_requested_at, tagged, launch_template, last_heartbeat, registered_at)
			VALUES ('h`+h+`', 't1', 'burst-h`+h+`', 'burst', 'tok`+h+`', 'ready', 'i-`+h+`', now() - interval '1 hour', true, '{"region":"eu-west-1"}', `+heartbeat+`, now())`)
		cloud.add("i-"+h, map[string]string{tagManaged: "true", tagDeployment: "d1", tagPool: "t1/burst", tagHost: "h" + h})
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('waiting', 't1', '{"placement":{"pool":"burst"}}', 'provisioning'),
		('stopped', 't1', '{"placement":{"pool":"burst"}}', 'stopped'),
		('done', 't1', '{"placement":{"pool":"burst"}}', 'succeeded'),
		('other', 't1', '{"placement":{"pool":"elsewhere"}}', 'provisioning')`)
	return &renameFixture{s: s, ctx: ctx, cloud: cloud}
}

func (f *renameFixture) rename(t *testing.T, from, to string) PoolRenamed {
	t.Helper()
	out, err := renamePool(f.ctx, f.s.db, f.s.log, "t1", from, to, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *renameFixture) pool(t *testing.T) poolRow {
	t.Helper()
	var pl poolRow
	err := f.s.db.Tx(f.ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(f.ctx, `SELECT `+poolRowColumns+` FROM pools WHERE id = 'pool1'`)
		if err != nil {
			return err
		}
		pl, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[poolRow])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// check is one provisioner pass with a provider check.
func (f *renameFixture) check(t *testing.T, pl poolRow) {
	t.Helper()
	if err := f.s.reconcilePool(f.ctx, f.cloud, pl, true); err != nil {
		t.Fatal(err)
	}
}

func (f *renameFixture) query(t *testing.T, q string, args ...any) string {
	t.Helper()
	var v string
	if err := f.s.db.Tx(f.ctx, store.System(), func(tx pgx.Tx) error { return tx.QueryRow(f.ctx, q, args...).Scan(&v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *renameFixture) runPool(t *testing.T, id string) string {
	t.Helper()
	return f.query(t, `SELECT spec->'placement'->>'pool' FROM runs WHERE id = $1`, id)
}

func (f *renameFixture) noneTerminated(t *testing.T) {
	t.Helper()
	if len(f.cloud.terminated) > 0 {
		t.Fatalf("instances terminated: %v", f.cloud.terminated)
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE state = 'terminated'`); n != "0" {
		t.Fatalf("%s host rows written off", n)
	}
}

// Every row follows a rename of a pool with live EC2 hosts; the hosts'
// instances are re-tagged, none is terminated, and the rename finishes.
func TestRenamePoolWithLiveHosts(t *testing.T) {
	f := newRenameFixture(t, false)
	// A lost launch's instance under the old name: still an orphan.
	f.cloud.add("i-orphan", map[string]string{tagManaged: "true", tagDeployment: "d1", tagPool: "t1/burst", tagHost: "gone"})

	out := f.rename(t, "burst", "burst-eu")
	if out.Hosts != 2 || out.Instances != 2 || out.Runs != 2 || out.Pool.Name != "burst-eu" || out.Pool.RenamedFrom == nil || *out.Pool.RenamedFrom != "burst" {
		t.Fatalf("rename answered %+v", out)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM hosts`); got != "burst-eu" {
		t.Errorf("hosts.pool = %s", got)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM host_tokens`); got != "burst-eu" {
		t.Errorf("host_tokens.pool = %s", got)
	}
	for id, want := range map[string]string{"waiting": "burst-eu", "stopped": "burst-eu", "done": "burst", "other": "elsewhere"} {
		if got := f.runPool(t, id); got != want {
			t.Errorf("run %s names pool %q, want %q", id, got, want)
		}
	}

	pl := f.pool(t)
	f.check(t, pl)
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Errorf("%s lux:pool = %q after a provider check", pid, got)
		}
	}
	if !slices.Equal(f.cloud.terminated, []string{"i-orphan"}) {
		t.Fatalf("terminated %v, want only the orphan", f.cloud.terminated)
	}
	f.cloud.terminated = nil
	if f.pool(t).RenamedFrom == nil {
		t.Fatal("finished before listing_lag passed since the re-tag")
	}
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	f.check(t, f.pool(t))
	if rf := f.pool(t).RenamedFrom; rf != nil {
		t.Fatalf("renamed_from = %q after every instance was re-tagged and listing_lag passed", *rf)
	}
	f.noneTerminated(t)
	if f.cloud.launched > 0 {
		t.Errorf("%d launches", f.cloud.launched)
	}
}

// A provisioner pass that read the pool's row before the rename committed
// (its name the old one) must not see the pool's instances as orphans:
// under the old name, no host row claims them any more.
func TestRenamePoolStaleProvisionerPass(t *testing.T) {
	f := newRenameFixture(t, false)
	before := f.pool(t)
	f.rename(t, "burst", "burst-eu")
	f.check(t, before)
	f.noneTerminated(t)
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Errorf("%s lux:pool = %q", pid, got)
		}
	}
}

// luxd stopping between the steps: after the rename's commit (nothing
// tagged), and midway through re-tagging (one instance done, nothing
// recorded). Whichever pass comes next converges, terminating nothing, with
// the provider's listings lagging behind its tags and the hosts settled
// (a host missing from the listings is otherwise written off).
func TestRenamePoolConvergesAfterACrash(t *testing.T) {
	f := newRenameFixture(t, true)
	if _, err := renamePoolTx(f.ctx, f.s.db, "t1", "burst", "burst-eu", false); err != nil {
		t.Fatal(err)
	}
	// Re-tagged by a luxd that stopped before recording it.
	if err := f.cloud.Retag(f.ctx, nil, []string{"i-1"}, tagPool, "t1/burst-eu"); err != nil {
		t.Fatal(err)
	}
	f.check(t, f.pool(t))
	f.noneTerminated(t)
	if got := f.cloud.tag("i-2", tagPool); got != "t1/burst-eu" {
		t.Fatalf("i-2 lux:pool = %q", got)
	}
	// The listings have not caught up with the re-tag: both instances
	// match neither name for a while.
	f.cloud.hidden = map[string]bool{"i-1": true, "i-2": true}
	f.check(t, f.pool(t))
	f.noneTerminated(t)
	if f.pool(t).RenamedFrom == nil {
		t.Fatal("finished while the instances were missing from the listings")
	}
	f.cloud.hidden = map[string]bool{}
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	f.check(t, f.pool(t))
	if f.pool(t).RenamedFrom != nil {
		t.Fatal("never finished")
	}
	f.noneTerminated(t)
}

// With CreateTags refused (IAM), the rename stands, re-tagging is retried
// on every provider check, nothing is terminated; once allowed it finishes.
func TestRenamePoolRetagDenied(t *testing.T) {
	f := newRenameFixture(t, true)
	f.cloud.deny = true
	f.rename(t, "burst", "burst-eu")
	for range 3 {
		f.check(t, f.pool(t))
		time.Sleep(f.s.cfg.ListingLag)
	}
	f.noneTerminated(t)
	if pl := f.pool(t); pl.Name != "burst-eu" || pl.RenamedFrom == nil {
		t.Fatalf("pool %s renamed_from %v; want the rename recorded and unfinished", pl.Name, pl.RenamedFrom)
	}
	if got := f.cloud.tag("i-1", tagPool); got != "t1/burst" {
		t.Fatalf("i-1 lux:pool = %q", got)
	}
	if got := f.runPool(t, "waiting"); got != "burst-eu" {
		t.Errorf("the waiting Run names %q", got)
	}
	f.cloud.deny = false
	f.check(t, f.pool(t))
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	if f.pool(t).RenamedFrom != nil {
		t.Fatal("never finished once CreateTags was allowed")
	}
	f.noneTerminated(t)
}

// What is refused: a taken name (live or retired), a name an unfinished
// rename reserves, a second rename meanwhile, an invalid name.
func TestRenamePoolRefusals(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't1', 'live', 'static'), ('pool3', 't1', 'old', 'static')`)
	execSQL(t, f.s, f.ctx, `UPDATE pools SET retired = true WHERE id = 'pool3'`)
	status := func(err error) (int, string) {
		var he *HTTPError
		if !errors.As(err, &he) {
			return 0, ""
		}
		return he.Status, he.Code
	}
	try := func(from, to string, wantStatus int, wantCode string) {
		t.Helper()
		_, err := renamePool(f.ctx, f.s.db, f.s.log, "t1", from, to, false)
		if st, code := status(err); st != wantStatus || code != wantCode {
			t.Errorf("rename %s → %q: %v; want %d %s", from, to, err, wantStatus, wantCode)
		}
	}
	try("burst", "live", http.StatusConflict, "pool_exists")
	try("burst", "old", http.StatusConflict, "pool_exists")
	try("burst", "", http.StatusUnprocessableEntity, "invalid_pool")
	try("burst", "burst", http.StatusUnprocessableEntity, "invalid_pool")
	try("nope", "other", http.StatusNotFound, "not_found")
	if f.pool(t).Name != "burst" {
		t.Fatal("a refused rename renamed")
	}

	f.rename(t, "burst", "burst-eu")
	try("burst-eu", "burst-us", http.StatusConflict, "rename_in_progress")
	try("live", "burst", http.StatusConflict, "pool_exists")
	ctx := context.WithValue(f.ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	_, err := f.s.putPool(ctx, &poolBody{Body: Pool{Name: "burst", Provider: "static"}})
	if st, code := status(err); st != http.StatusConflict || code != "pool_name_reserved" {
		t.Errorf("pools set under the reserved old name: %v", err)
	}
	// Another tenant's pool may have the name all along.
	execSQL(t, f.s, f.ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
	ctx2 := context.WithValue(f.ctx, principalKey, Principal{TenantID: "t2", Scopes: []string{"admin"}})
	if _, err := f.s.putPool(ctx2, &poolBody{Body: Pool{Name: "burst", Provider: "static"}}); err != nil {
		t.Errorf("another tenant's pool named like the reserved name: %v", err)
	}
}

// A pool with no hosts: a static one is renamed outright; an ec2 one
// finishes on its next provider check.
func TestRenamePoolWithoutHosts(t *testing.T) {
	s := testServer(t)
	s.deployment = "d1"
	s.cfg.ListingLag = time.Millisecond
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('ps', 'lab', 'static'), ('pe', 'burst', 'ec2')`)
	out, err := renamePool(ctx, s.db, s.log, "", "lab", "lab2", false)
	if err != nil || out.Pool.Name != "lab2" || out.Pool.RenamedFrom != nil || out.Hosts != 0 || !out.Pool.Platform {
		t.Fatalf("static: %+v, %v", out, err)
	}
	if _, err := renamePool(ctx, s.db, s.log, "", "lab2", "lab3", false); err != nil {
		t.Fatalf("a static pool renamed again: %v", err)
	}
	out, err = renamePool(ctx, s.db, s.log, "", "burst", "burst2", false)
	if err != nil || out.Pool.RenamedFrom == nil {
		t.Fatalf("ec2: %+v, %v", out, err)
	}
	cloud := newFakeCloud()
	pl := poolRow{ID: "pe", Name: "burst2", Provider: "ec2"}
	time.Sleep(time.Millisecond)
	if err := s.reconcilePool(ctx, cloud, pl, true); err != nil {
		t.Fatal(err)
	}
	var renamed *string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT renamed_from FROM pools WHERE id = 'pe'`).Scan(&renamed)
	}); err != nil {
		t.Fatal(err)
	}
	if renamed != nil {
		t.Fatalf("renamed_from = %q with no instances to re-tag", *renamed)
	}
}

// dryRun counts and renames nothing.
func TestRenamePoolDryRun(t *testing.T) {
	f := newRenameFixture(t, false)
	out, err := renamePool(f.ctx, f.s.db, f.s.log, "t1", "burst", "burst-eu", true)
	if err != nil || !out.DryRun || out.Hosts != 2 || out.Runs != 2 || out.Pool.Name != "burst" {
		t.Fatalf("%+v, %v", out, err)
	}
	if f.pool(t).Name != "burst" || f.runPool(t, "waiting") != "burst" {
		t.Fatal("a dry run renamed")
	}
}

// The rename does not finish while a live host's instance is missing from
// the new name's listing, even past listing_lag (one just launched, not
// listed yet): finished, nothing would re-tag it, and the listings would
// never show it again.
func TestRenamePoolWaitsForEveryInstanceUnderTheNewName(t *testing.T) {
	f := newRenameFixture(t, false)
	f.rename(t, "burst", "burst-eu")
	// i-2 drops out of the listings before it is re-tagged.
	f.cloud.hidden = map[string]bool{"i-2": true}
	f.check(t, f.pool(t))
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	f.check(t, f.pool(t))
	if f.pool(t).RenamedFrom == nil {
		t.Fatal("finished with i-2 never listed under the new name")
	}
	f.cloud.hidden = map[string]bool{}
	f.check(t, f.pool(t))
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	f.check(t, f.pool(t))
	f.noneTerminated(t)
	if f.pool(t).RenamedFrom != nil || f.cloud.tag("i-2", tagPool) != "t1/burst-eu" {
		t.Fatal("never finished")
	}
}

// A launch decided on the pool row as read before a rename does not start
// an instance under the old name: its host row would join no pool, and
// its instance would carry a name nothing lists.
func TestRenamePoolLaunchUnderTheOldNameIsSkipped(t *testing.T) {
	f := newRenameFixture(t, false)
	before := f.pool(t)
	f.rename(t, "burst", "burst-eu")
	prov := &fakeLaunchProvider{}
	if err := f.s.launch(f.ctx, prov, before); err != nil {
		t.Fatal(err)
	}
	if prov.env != nil {
		t.Error("launched under the old name")
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE pool = 'burst'`); n != "0" {
		t.Errorf("%s host rows under the old name", n)
	}
	if err := f.s.launch(f.ctx, prov, f.pool(t)); err != nil || prov.env == nil {
		t.Fatalf("launch under the new name: %v", err)
	}
}
