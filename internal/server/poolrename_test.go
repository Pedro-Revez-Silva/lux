package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
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
	// launches: Launch starts an instance; otherwise it fails.
	launches bool
	// retags: each Retag's tag key and how many instances it named.
	retags []retagCall
	// listings: the tag filters of each Instances call.
	listings []map[string]string
	// before, if set, runs before each call, outside the lock: a test
	// blocks a call there to interleave it with other work.
	before func(ctx context.Context, call string, tags map[string]string) error
}

type retagCall struct {
	key string
	n   int
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

func (c *fakeCloud) terminatedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.terminated)
}

func (c *fakeCloud) tag(pid, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.insts[pid].tags[key]
}

func (c *fakeCloud) setTag(pid, key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insts[pid].tags[key] = value
}

// listedByName: Instances calls that filtered on lux:pool.
func (c *fakeCloud) listedByName() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.listings {
		if _, ok := l[tagPool]; ok {
			n++
		}
	}
	return n
}

func (c *fakeCloud) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (Launched, error) {
	// The instance exists, with the tags sent, as of the hook's return.
	if err := c.hook(ctx, "Launch", tags); err != nil {
		return Launched{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launched++
	if !c.launches {
		return Launched{}, errors.New("fakeCloud: no launches in this test")
	}
	pid := fmt.Sprintf("i-launched-%d", c.launched)
	c.insts[pid] = &fakeInstance{state: "pending", tags: maps.Clone(tags)}
	return Launched{ProviderID: pid}, nil
}

func (c *fakeCloud) launchedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for pid := range c.insts {
		if strings.HasPrefix(pid, "i-launched-") {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}

func (c *fakeCloud) hook(ctx context.Context, call string, tags map[string]string) error {
	c.mu.Lock()
	before := c.before
	c.mu.Unlock()
	if before == nil {
		return nil
	}
	return before(ctx, call, tags)
}

func (c *fakeCloud) Terminate(ctx context.Context, template json.RawMessage, pid string) error {
	if err := c.hook(ctx, "Terminate", nil); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminated = append(c.terminated, pid)
	if i := c.insts[pid]; i != nil {
		i.state = "terminated"
	}
	return nil
}

func (c *fakeCloud) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error) {
	// Evaluated after the hook: a blocked listing answers as of its return.
	if err := c.hook(ctx, "Instances", tags); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listings = append(c.listings, maps.Clone(tags))
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

// Describe answers by id, whatever the listings show (hidden).
func (c *fakeCloud) Describe(ctx context.Context, template json.RawMessage, pids []string) (map[string]Instance, error) {
	if err := c.hook(ctx, "Describe", nil); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]Instance{}
	for _, pid := range pids {
		if i := c.insts[pid]; i != nil {
			out[pid] = Instance{State: i.state, Tags: maps.Clone(i.tags)}
		}
	}
	return out, nil
}

func (c *fakeCloud) Retag(ctx context.Context, template json.RawMessage, pids []string, key, value string) error {
	if err := c.hook(ctx, "Retag", map[string]string{key: value}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deny {
		return errors.New("UnauthorizedOperation: not authorized to perform ec2:CreateTags (fake)")
	}
	c.retags = append(c.retags, retagCall{key, len(pids)})
	if len(pids) > 1000 {
		return errors.New("InvalidParameterValue: at most 1000 resources per CreateTags (fake)")
	}
	for _, pid := range pids {
		if i := c.insts[pid]; i != nil {
			i.tags[key] = value
		}
	}
	return nil
}

// renameConfirmed renames as a caller who typed the pool's name.
func renameConfirmed(ctx context.Context, s *Server, tenantID, from, to string, dryRun bool) (PoolRenamed, error) {
	return s.rename(ctx, tenantID, renameArgs{From: from, To: to, Confirm: from, DryRun: dryRun})
}

// renameFixture: tenant t1's ec2 pool "burst" (id pool1) with two live
// hosts (i-1, i-2, tagged lux:pool-id pool1 and lux:pool t1/burst, and
// confirmed so), a Run waiting for it, a finished Run that ran on it, and
// its host tokens.
type renameFixture struct {
	s     *Server
	ctx   context.Context
	cloud *fakeCloud
}

// testWait bounds each rename test: every database and provider call, and
// every wait for another goroutine, ends by then, so a regression fails
// rather than hangs.
const testWait = 30 * time.Second

// testCtx is the test's deadline context, cancelled when it ends.
func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	t.Cleanup(cancel)
	return ctx
}

func instanceTags(pool, host string) map[string]string {
	return map[string]string{tagManaged: "true", tagDeployment: "d1", tagPoolID: "pool1", tagPool: pool, tagHost: host}
}

func newRenameFixture(t *testing.T, settled bool) *renameFixture {
	t.Helper()
	s := testServer(t)
	s.deployment = "d1"
	s.cfg.ListingLag = 300 * time.Millisecond
	ctx := testCtx(t)
	cloud := newFakeCloud()
	s.cfg.Providers = map[string]Provider{"ec2": cloud}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template, id_migrated_at) VALUES ('pool1', 't1', 'burst', 'ec2', '{"region":"eu-west-1"}', now())`)
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('tok1', 't1', 'burst', 'x1'), ('tok2', 't1', 'burst', 'x2')`)
	// Settled: launched long ago, runner silent: a host its listing misses
	// is looked up by id. Otherwise heartbeating, as a live host is.
	heartbeat := "now()"
	if settled {
		heartbeat = "NULL"
	}
	for _, h := range []string{"1", "2"} {
		execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, token_id, state, provider_id, provision_requested_at, tagged, pool_id_tagged, launch_template, last_heartbeat, registered_at)
			VALUES ('h`+h+`', 't1', 'burst-h`+h+`', 'burst', 'tok`+h+`', 'ready', 'i-`+h+`', now() - interval '1 hour', true, true, '{"region":"eu-west-1"}', `+heartbeat+`, now())`)
		cloud.add("i-"+h, instanceTags("t1/burst", "h"+h))
	}
	// A luxd that discovers by lux:pool-id, as every luxd of this version
	// checks in.
	if err := s.checkIn(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('waiting', 't1', '{"placement":{"pool":"burst"}}', 'provisioning'),
		('stopped', 't1', '{"placement":{"pool":"burst"}}', 'stopped'),
		('done', 't1', '{"placement":{"pool":"burst"}}', 'succeeded'),
		('other', 't1', '{"placement":{"pool":"elsewhere"}}', 'provisioning')`)
	return &renameFixture{s: s, ctx: ctx, cloud: cloud}
}

// legacy turns the fixture's pool into one launched before lux:pool-id:
// its instances carry only the name, its hosts are not confirmed.
func (f *renameFixture) legacy(t *testing.T) {
	t.Helper()
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET pool_id_tagged = false`)
	execSQL(t, f.s, f.ctx, `UPDATE pools SET id_migrated_at = NULL WHERE id = 'pool1'`)
	f.cloud.mu.Lock()
	defer f.cloud.mu.Unlock()
	for _, i := range f.cloud.insts {
		delete(i.tags, tagPoolID)
	}
}

func (f *renameFixture) rename(t *testing.T, from, to string) PoolRenamed {
	t.Helper()
	out, err := renameConfirmed(f.ctx, f.s, "t1", from, to, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *renameFixture) pool(t *testing.T) poolRow {
	t.Helper()
	return readPoolRow(t, f.ctx, f.s, "pool1")
}

func readPoolRow(t *testing.T, ctx context.Context, s *Server, id string) poolRow {
	t.Helper()
	var pl poolRow
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+poolRowColumns+` FROM pools WHERE id = $1`, id)
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
	if err := f.s.reconcilePool(f.ctx, f.cloud, pl, true, takeLease(t, f.ctx, f.s)); err != nil {
		t.Fatal(err)
	}
}

// takeLease makes s the provisioner.
func takeLease(t *testing.T, ctx context.Context, s *Server) *passLease {
	t.Helper()
	l, err := s.provisionLease(ctx)
	if err != nil || l == nil {
		t.Fatalf("provisioner lease: %v, %v", l, err)
	}
	return l
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
	if got := f.cloud.terminatedIDs(); len(got) > 0 {
		t.Fatalf("instances terminated: %v", got)
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE state = 'terminated'`); n != "0" {
		t.Fatalf("%s host rows written off", n)
	}
}

// Every row follows a rename of a pool with live EC2 hosts in one step;
// the next provider check finds the instances by lux:pool-id, updates
// their name tag, terminates only the orphan, and launches nothing.
func TestRenamePoolWithLiveHosts(t *testing.T) {
	f := newRenameFixture(t, false)
	// A lost launch's instance under the old name: still an orphan.
	f.cloud.add("i-orphan", instanceTags("t1/burst", "gone"))

	out := f.rename(t, "burst", "burst-eu")
	if out.Hosts != 2 || out.Instances != 2 || out.Runs != 2 || out.Pool.Name != "burst-eu" {
		t.Fatalf("rename answered %+v", out)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM hosts`); got != "burst-eu" {
		t.Errorf("hosts.pool = %s", got)
	}
	if got := f.query(t, `SELECT string_agg(DISTINCT pool, ',') FROM host_tokens`); got != "burst-eu" {
		t.Errorf("host_tokens.pool = %s", got)
	}
	for id, want := range map[string]string{"waiting": "burst-eu", "stopped": "burst-eu", "done": "burst-eu", "other": "elsewhere"} {
		if got := f.runPool(t, id); got != want {
			t.Errorf("run %s names pool %q, want %q", id, got, want)
		}
	}
	if pl := f.pool(t); pl.RenamedAt == nil || !slices.Equal(pl.PreviousNames, []string{"burst"}) {
		t.Errorf("renamed_at %v, previous_names %v", pl.RenamedAt, pl.PreviousNames)
	}

	f.check(t, f.pool(t))
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Errorf("%s lux:pool = %q after a provider check", pid, got)
		}
	}
	if got := f.cloud.terminatedIDs(); !slices.Equal(got, []string{"i-orphan"}) {
		t.Fatalf("terminated %v, want only the orphan", got)
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE state = 'terminated'`); n != "0" {
		t.Fatalf("%s host rows written off", n)
	}
	if f.cloud.launched > 0 {
		t.Errorf("%d launches", f.cloud.launched)
	}
	if n := f.cloud.listedByName(); n != 0 {
		t.Errorf("a migrated pool listed by name %d times", n)
	}
}

// A name-only orphan is tagged before its first termination attempt, so a
// refusal is retried by id even after migration ends name discovery.
func TestLegacyPoolIsMigrated(t *testing.T) {
	f := newRenameFixture(t, true)
	f.legacy(t)
	f.cloud.add("i-orphan", map[string]string{tagManaged: "true", tagDeployment: "d1", tagPool: "t1/burst", tagHost: "gone"})
	refuse := true
	f.cloud.before = func(_ context.Context, call string, _ map[string]string) error {
		if call == "Terminate" && refuse {
			refuse = false
			return errors.New("termination refused")
		}
		return nil
	}
	if !f.pool(t).Legacy {
		t.Fatal("pool from before id tags is already migrated")
	}
	for _, dry := range []bool{true, false} {
		_, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", dry)
		if st, code, _ := httpErr(err); st != http.StatusConflict || code != "pool_not_migrated" {
			t.Fatalf("rename (dry run %v) of a legacy pool: %v", dry, err)
		}
	}
	f.check(t, f.pool(t))
	f.noneTerminated(t)
	if got := f.cloud.tag("i-orphan", tagPoolID); got != "pool1" {
		t.Fatalf("orphan still missing id tag after termination refusal: %q", got)
	}
	if !f.pool(t).Legacy {
		t.Fatal("migrated before a clean name listing")
	}
	f.check(t, f.pool(t))
	f.noneTerminatedBut(t, "i-orphan")
	if f.pool(t).Legacy {
		t.Fatal("still legacy after a clean name listing")
	}
	byName := f.cloud.listedByName()
	f.check(t, f.pool(t))
	if f.cloud.listedByName() != byName {
		t.Fatal("a migrated pool is still listed by name")
	}
	f.rename(t, "burst", "burst-eu")
}

// A lost launch reply may be written off before an untagged instance
// becomes listable. Migration must wait through the launch and listing
// window, then require a clean name listing; retirement must keep id
// discovery alive long enough to terminate one appearing later.
func TestLegacyLostLaunchAndRetiredLateInstance(t *testing.T) {
	f := newRenameFixture(t, false)
	f.legacy(t)
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET pool_id_tagged = true`)
	execSQL(t, f.s, f.ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provision_requested_at, tagged)
		VALUES ('h-lost', 't1', 'burst-lost', 'burst', 'provisioning', now() - interval '1 hour', true)`)
	f.check(t, f.pool(t))
	if got := f.query(t, `SELECT state FROM hosts WHERE id = 'h-lost'`); got != "terminated" {
		t.Fatalf("lost launch row: %s", got)
	}
	if !f.pool(t).Legacy {
		t.Fatal("written-off launch ended name discovery too early")
	}
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", false); err == nil {
		t.Fatal("renamed a pool with a recent lost launch reply")
	}
	// Simulate time passing, while the provider has yet to list the instance.
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET terminated_at = now() - interval '2 hours' WHERE id = 'h-lost'`)
	f.check(t, f.pool(t))
	if f.pool(t).Legacy {
		t.Fatal("no migration after the window passed and name listing was clean")
	}
	// Retire before a late id-tagged instance appears. No host row claims it.
	execSQL(t, f.s, f.ctx, `UPDATE pools SET retired = true, retired_at = now(), last_empty_listing_at = NULL WHERE id = 'pool1'`)
	f.cloud.add("i-late", instanceTags("t1/burst", "h-lost"))
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.cloud.terminatedIDs(), "i-late") {
		t.Fatal("late instance of a retired pool was not terminated")
	}
}

// An instance whose RunInstances reply was lost can become visible only
// after its row was written off and its pool retired. Retirement retains
// id discovery until that instance has been found and terminated.
func TestRetiredPoolFindsLostLaunchAfterWriteOff(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provision_requested_at, tagged, pool_id_tagged)
		VALUES ('h-lost', 't1', 'burst-lost', 'burst', 'provisioning', now() - interval '1 hour', true, true)`)
	execSQL(t, f.s, f.ctx, `UPDATE pools SET retired = true, retired_at = now(), last_empty_listing_at = NULL WHERE id = 'pool1'`)
	// No live rows remain: only the retirement discovery window selects it.
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET state = 'terminated' WHERE id IN ('h1', 'h2')`)
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.query(t, `SELECT state FROM hosts WHERE id = 'h-lost'`); got != "terminated" {
		t.Fatalf("lost launch row: %s", got)
	}
	f.cloud.add("i-late", instanceTags("t1/burst", "h-lost"))
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.cloud.terminatedIDs(), "i-late") {
		t.Fatal("a late instance of a retired pool was not terminated")
	}
	// The time window alone is not enough: a last listing must have
	// found no instances after retirement.
	execSQL(t, f.s, f.ctx, `UPDATE pools SET retired_at = now() - interval '2 hours', last_empty_listing_at = NULL WHERE id = 'pool1'`)
	f.cloud.add("i-later", instanceTags("t1/burst", "h-lost"))
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.cloud.terminatedIDs(), "i-later") {
		t.Fatal("retired pool dropped discovery without a clean final listing")
	}
	// EC2 eventually purges terminated instances from its listings.
	f.cloud.forget("i-late")
	f.cloud.forget("i-later")
	f.cloud.forget("i-1")
	f.cloud.forget("i-2")
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.query(t, `SELECT (last_empty_listing_at IS NOT NULL)::text FROM pools WHERE id = 'pool1'`); got != "true" {
		t.Fatalf("last empty listing not recorded: %s", got)
	}
	listed := len(f.cloud.listings)
	f.s.lastAliveCheck = time.Time{}
	if err := f.s.provision(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.cloud.listings) != listed {
		t.Fatal("retired pool still checked after the window and an empty listing")
	}
}

// noneTerminatedBut: only the given instances were terminated, and no
// host row written off.
func (f *renameFixture) noneTerminatedBut(t *testing.T, pids ...string) {
	t.Helper()
	if got := f.cloud.terminatedIDs(); !slices.Equal(got, pids) {
		t.Fatalf("terminated %v, want %v", got, pids)
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE state = 'terminated'`); n != "0" {
		t.Fatalf("%s host rows written off", n)
	}
}

// Without ec2:CreateTags a legacy pool stays legacy: still listed by name,
// nothing terminated, never renamed.
func TestLegacyPoolWithTaggingRefused(t *testing.T) {
	f := newRenameFixture(t, true)
	f.legacy(t)
	f.cloud.deny = true
	for range 3 {
		f.check(t, f.pool(t))
	}
	f.noneTerminated(t)
	if !f.pool(t).Legacy {
		t.Fatal("confirmed though no instance carries lux:pool-id")
	}
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", false); err == nil {
		t.Fatal("a legacy pool renamed")
	}
}

// Discovery never reads lux:pool once a pool is migrated. After a rename
// whose name re-tag is refused, and another pool of the owner takes the
// old name: the renamed pool keeps its instances, whatever name tag they
// carry, and the new pool, whose lux:pool listing would match them, never
// takes them for its orphans.
func TestRenameDiscoveryIgnoresTheNameTag(t *testing.T) {
	f := newRenameFixture(t, true)
	f.cloud.deny = true
	f.rename(t, "burst", "burst-eu")
	f.cloud.setTag("i-2", tagPool, "t1/anything")
	for range 2 {
		f.check(t, f.pool(t))
		time.Sleep(f.s.cfg.ListingLag)
	}
	f.noneTerminated(t)
	if got := f.cloud.tag("i-1", tagPool); got != "t1/burst" {
		t.Fatalf("i-1 lux:pool = %q with CreateTags refused", got)
	}

	// The old name is free at once.
	ctx := context.WithValue(f.ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	if _, err := f.s.putPool(ctx, poolIn(Pool{Name: "burst", Provider: "ec2", Template: map[string]any{"region": "eu-west-1"}})); err != nil {
		t.Fatalf("a new pool under the old name: %v", err)
	}
	newID := f.query(t, `SELECT id FROM pools WHERE name = 'burst'`)
	for range 2 {
		f.check(t, readPoolRow(t, f.ctx, f.s, newID))
		f.check(t, f.pool(t))
	}
	f.noneTerminated(t)
	if got := f.query(t, `SELECT string_agg(pool, ',' ORDER BY id) FROM hosts`); got != "burst-eu,burst-eu" {
		t.Fatalf("hosts in %s", got)
	}

	// Once allowed, the name tags follow.
	f.cloud.deny = false
	f.check(t, f.pool(t))
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Errorf("%s lux:pool = %q", pid, got)
		}
	}
	if n := f.cloud.listedByName(); n != 0 {
		t.Errorf("migrated pools listed by name %d times", n)
	}
}

// What is refused: a taken name (live or retired), an invalid name.
func TestRenamePoolRefusals(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't1', 'live', 'static'), ('pool3', 't1', 'old', 'static')`)
	execSQL(t, f.s, f.ctx, `UPDATE pools SET retired = true WHERE id = 'pool3'`)
	try := func(from, to string, wantStatus int, wantCode string) {
		t.Helper()
		_, err := renameConfirmed(f.ctx, f.s, "t1", from, to, false)
		if st, code, _ := httpErr(err); st != wantStatus || code != wantCode {
			t.Errorf("rename %s → %q: %v; want %d %s", from, to, err, wantStatus, wantCode)
		}
	}
	try("burst", "live", http.StatusConflict, "pool_exists")
	try("burst", "old", http.StatusConflict, "pool_exists")
	try("burst", "", http.StatusUnprocessableEntity, "invalid_pool")
	try("burst", "burst", http.StatusUnprocessableEntity, "invalid_pool")
	try("burst", "Burst", http.StatusUnprocessableEntity, "invalid_pool")
	try("nope", "other", http.StatusNotFound, "not_found")
	// A stored name that breaks the rule is kept, but never given anew.
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('legacy', 't1', 'Legacy_Pool', 'static')`)
	try("burst", "Legacy_Pool", http.StatusUnprocessableEntity, "invalid_pool")
	// A platform pool may not become "t1/burst": its lux:pool tag value
	// would be tenant t1's pool burst's.
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('plat', 'shared', 'ec2', '{}')`)
	if _, err := renameConfirmed(f.ctx, f.s, "", "shared", "t1/burst", false); err == nil {
		t.Error("a platform pool renamed to t1/burst")
	} else if st, code, _ := httpErr(err); st != http.StatusUnprocessableEntity || code != "invalid_pool" {
		t.Errorf("platform rename to t1/burst: %v", err)
	}
	if f.pool(t).Name != "burst" {
		t.Fatal("a refused rename renamed")
	}
	// Renamed twice in a row, and back: nothing waits for tags.
	f.rename(t, "burst", "burst-eu")
	f.rename(t, "burst-eu", "burst-us")
	f.rename(t, "burst-us", "burst")
	if pl := f.pool(t); pl.Name != "burst" || !slices.Equal(pl.PreviousNames, []string{"burst-eu", "burst-us"}) {
		t.Fatalf("pool %s, previous names %v", pl.Name, pl.PreviousNames)
	}
}

// A pool with no hosts: a static one and an ec2 one are renamed outright,
// and again; only the ec2 one records renamed_at and arms the lease fence.
func TestRenamePoolWithoutHosts(t *testing.T) {
	s := testServer(t)
	s.deployment = "d1"
	ctx := testCtx(t)
	if err := s.checkIn(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, id_migrated_at) VALUES ('ps', 'lab', 'static', now()), ('pe', 'burst', 'ec2', now())`)
	out, err := renameConfirmed(ctx, s, "", "lab", "lab2", false)
	if err != nil || out.Pool.Name != "lab2" || out.Hosts != 0 || !out.Pool.Platform {
		t.Fatalf("static: %+v, %v", out, err)
	}
	if _, err := renameConfirmed(ctx, s, "", "lab2", "lab3", false); err != nil {
		t.Fatalf("a static pool renamed again: %v", err)
	}
	if readPoolRow(t, ctx, s, "ps").RenamedAt != nil {
		t.Error("a static pool's rename set renamed_at")
	}
	armed := func() string {
		var n string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*)::text FROM pool_rename_fence`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := armed(); n != "0" {
		t.Error("a static pool's rename armed the lease fence")
	}
	if _, err := renameConfirmed(ctx, s, "", "burst", "burst2", false); err != nil {
		t.Fatalf("ec2: %v", err)
	}
	if _, err := renameConfirmed(ctx, s, "", "burst2", "burst3", false); err != nil {
		t.Fatalf("ec2 again: %v", err)
	}
	if readPoolRow(t, ctx, s, "pe").RenamedAt == nil {
		t.Error("an ec2 pool's rename left renamed_at unset")
	}
	if n := armed(); n != "1" {
		t.Errorf("lease fence rows after two ec2 renames: %s, want 1", n)
	}
}

// dryRun counts and renames nothing.
func TestRenamePoolDryRun(t *testing.T) {
	f := newRenameFixture(t, false)
	out, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", true)
	if err != nil || !out.DryRun || out.Hosts != 2 || out.Runs != 2 || out.Pool.Name != "burst" {
		t.Fatalf("%+v, %v", out, err)
	}
	if f.pool(t).Name != "burst" || f.runPool(t, "waiting") != "burst" {
		t.Fatal("a dry run renamed")
	}
	// Without a name it only counts; with a taken one it says so.
	out, err = renameConfirmed(f.ctx, f.s, "t1", "burst", "", true)
	if err != nil || out.Hosts != 2 || out.Runs != 2 {
		t.Fatalf("dry run without a name: %+v, %v", out, err)
	}
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't1', 'live', 'static')`)
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "live", true); err == nil {
		t.Fatal("a dry run onto a taken name passed")
	}
}

// A launch carries lux:pool-id with the other tags, and its host row is
// confirmed from the start: it is never what keeps a pool legacy.
func TestLaunchTagsThePoolID(t *testing.T) {
	f := newRenameFixture(t, false)
	f.cloud.launches = true
	if err := f.s.launch(f.ctx, f.cloud, f.pool(t), nil); err != nil {
		t.Fatal(err)
	}
	pids := f.cloud.launchedIDs()
	if len(pids) != 1 {
		t.Fatalf("launched %v", pids)
	}
	if got := f.cloud.tag(pids[0], tagPoolID); got != "pool1" {
		t.Errorf("lux:pool-id = %q", got)
	}
	if got := f.cloud.tag(pids[0], tagPool); got != "t1/burst" {
		t.Errorf("lux:pool = %q", got)
	}
	if got := f.query(t, `SELECT pool_id_tagged::text FROM hosts WHERE provider_id = $1`, pids[0]); got != "true" {
		t.Errorf("pool_id_tagged = %s", got)
	}
	if f.pool(t).Legacy {
		t.Error("a launch made the pool legacy")
	}
}

// A launch decided on the pool row as read before a rename does not start
// an instance under the old name: its host row would join no pool.
func TestRenamePoolLaunchUnderTheOldNameIsSkipped(t *testing.T) {
	f := newRenameFixture(t, false)
	before := f.pool(t)
	f.rename(t, "burst", "burst-eu")
	prov := &fakeLaunchProvider{}
	if err := f.s.launch(f.ctx, prov, before, nil); err != nil {
		t.Fatal(err)
	}
	if prov.env != nil {
		t.Error("launched under the old name")
	}
	if n := f.query(t, `SELECT count(*)::text FROM hosts WHERE pool = 'burst'`); n != "0" {
		t.Errorf("%s host rows under the old name", n)
	}
	if err := f.s.launch(f.ctx, prov, f.pool(t), nil); err != nil || prov.env == nil {
		t.Fatalf("launch under the new name: %v", err)
	}
}

// Who may rename what, through the API: a tenant's admin key its own
// pools (a platform pool is not one: 404); a read key nothing; an operator
// a platform pool, or a tenant's with ?tenant=.
func TestRenamePoolAPIPermissions(t *testing.T) {
	s := testServer(t)
	ctx := testCtx(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('mine', 't1', 'lab', 'static'), ('plat', NULL, 'shared', 'static')`)
	keys := map[string]string{"admin": ids.Secret("luxk"), "read": ids.Secret("luxk"), "operator": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('ka', 't1', 'a', $1, ARRAY['admin']), ('kr', 't1', 'r', $2, ARRAY['read']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["admin"]), ids.Hash(keys["read"]), ids.Hash(keys["operator"]))
	post := func(key, path, name string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"name": name})
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		who, path, name string
		want            int
	}{
		{"read", "/v1/pools/lab/rename", "lab2", http.StatusForbidden},
		{"admin", "/v1/pools/shared/rename", "shared2", http.StatusNotFound},
		{"admin", "/v1/pools/lab/rename", "", http.StatusUnprocessableEntity},
		{"admin", "/v1/pools/lab/rename", "lab2", http.StatusOK},
		{"admin", "/v1/pools/lab2/rename?dryRun=true", "", http.StatusOK},
		{"operator", "/v1/pools/lab2/rename", "lab3", http.StatusNotFound},
		{"operator", "/v1/pools/lab2/rename?tenant=t1", "lab3", http.StatusOK},
		{"operator", "/v1/pools/shared/rename", "shared2", http.StatusOK},
		{"operator", "/v1/pools/shared2/rename?tenant=t1", "lab3", http.StatusNotFound},
	} {
		if code, body := post(keys[c.who], c.path, c.name); code != c.want {
			t.Errorf("%s POST %s %q: %d %s, want %d", c.who, c.path, c.name, code, body, c.want)
		}
	}
	var got []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT name FROM pools ORDER BY id`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"lab3", "shared2"}) {
		t.Errorf("pools %v, want [lab3 shared2]", got)
	}
}
