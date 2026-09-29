package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// Interleavings of a provisioner pass with a rename, a lease handoff, a
// launch and a late re-tag. A fakeCloud call blocks on a channel while the
// test changes the world under it; no pass may terminate a live instance
// or write off its host. Every wait is bounded by the test's deadline
// (testCtx), and a gate still closed when the test ends is opened.

// gate blocks the first fakeCloud call match accepts until release is
// closed; entered is closed once it arrives. With onTheWire, the blocked
// call ignores its caller's context (a request already sent lands whatever
// happened to its caller), though not the test's.
type gate struct {
	entered, release chan struct{}
	once             sync.Once
	ctx              context.Context
}

func (f *renameFixture) gate(t *testing.T, onTheWire bool, match func(call string, tags map[string]string) bool) *gate {
	t.Helper()
	g := &gate{entered: make(chan struct{}), release: make(chan struct{}), ctx: f.ctx}
	f.cloud.mu.Lock()
	f.cloud.before = func(ctx context.Context, call string, tags map[string]string) error {
		hit := false
		if match(call, tags) {
			g.once.Do(func() { hit = true })
		}
		if !hit {
			return nil
		}
		close(g.entered)
		if onTheWire {
			ctx = f.ctx
		}
		select {
		case <-g.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.cloud.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	return g
}

func (g *gate) open() { close(g.release) }

// recv waits for a value on ch until ctx ends.
func recv[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func (g *gate) waitEntered(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-g.ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pass runs one provider-checking pass of s over pool pl in the
// background; its result arrives on the channel. Its context has no
// deadline of its own (the lease's is under test); it is cancelled when
// the test ends.
func (f *renameFixture) pass(t *testing.T, s *Server, pl poolRow, lease *passLease) <-chan error {
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	go func() { done <- s.reconcilePool(ctx, f.cloud, pl, true, lease) }()
	return done
}

func listing(tag string) func(string, map[string]string) bool {
	return func(call string, tags map[string]string) bool { return call == "Instances" && tags[tagPool] == tag }
}

// A pass lists the pool under its old name; meanwhile the rename commits
// and the instances' new tag lands. The listing, answered after that,
// finds none of the pool's hosts: they are looked up by id, found running,
// and kept.
func TestRenameRaceStalePassAfterRetag(t *testing.T) {
	f := newRenameFixture(t, true)
	g := f.gate(t, false, listing("t1/burst"))
	done := f.pass(t, f.s, f.pool(t), takeLease(t, f.ctx, f.s))
	g.waitEntered(t, "the pass's listing")

	f.rename(t, "burst", "burst-eu")
	if err := f.cloud.Retag(f.ctx, nil, []string{"i-1", "i-2"}, tagPool, "t1/burst-eu"); err != nil {
		t.Fatal(err)
	}
	g.open()
	if err := recv(t, f.ctx, done, "the pass"); err != nil {
		t.Fatal(err)
	}
	f.noneTerminated(t)
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Errorf("%s lux:pool = %q", pid, got)
		}
	}
}

// Another luxd takes the provisioner lease while a pass's listing is out,
// and a host the pass read as lost long enough to terminate comes back.
// The pass must not act on what it read: fenced, it terminates nothing.
// Its provider calls carry a deadline no later than the lease's expiry.
func TestRenameRaceLeaseHandoffFencesThePass(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET state = 'lost', lost_at = now() - interval '1 hour' WHERE id = 'h1'`)
	lease := takeLease(t, f.ctx, f.s)
	var deadline time.Time
	var hasDeadline bool
	g := f.gate(t, false, func(call string, tags map[string]string) bool { return call == "Instances" })
	inner := f.cloud.before
	f.cloud.before = func(ctx context.Context, call string, tags map[string]string) error {
		if call == "Instances" && !hasDeadline {
			deadline, hasDeadline = ctx.Deadline()
		}
		return inner(ctx, call, tags)
	}
	done := f.pass(t, f.s, f.pool(t), lease)
	g.waitEntered(t, "the pass's listing")
	if !hasDeadline || deadline.After(lease.expires) {
		t.Errorf("listing deadline %v (set: %v), want one no later than the lease's expiry", deadline, hasDeadline)
	}

	execSQL(t, f.s, f.ctx, `UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = 'provisioner'`)
	s2 := f.otherLuxd(t)
	if l2 := takeLease(t, f.ctx, s2); l2.token == lease.token {
		t.Fatalf("a new holder kept token %d", l2.token)
	}
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET state = 'ready', lost_at = NULL, last_heartbeat = now() WHERE id = 'h1'`)
	g.open()
	if err := recv(t, f.ctx, done, "the pass"); !errors.Is(err, errFenced) {
		t.Errorf("pass: %v, want it fenced", err)
	}
	f.noneTerminated(t)
}

// While a pass's listing is out, a host is launched (its row written, its
// instance id not yet recorded) and the pool renamed. The listing shows
// the instance, which no row the pass read claims: checked again before
// terminating, its host row, now under the new name, claims it.
func TestRenameRaceLaunchDuringListingIsNoOrphan(t *testing.T) {
	f := newRenameFixture(t, false)
	g := f.gate(t, false, listing("t1/burst"))
	done := f.pass(t, f.s, f.pool(t), takeLease(t, f.ctx, f.s))
	g.waitEntered(t, "the pass's listing")

	execSQL(t, f.s, f.ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('tok3', 't1', 'burst', 'x3')`)
	execSQL(t, f.s, f.ctx, `INSERT INTO hosts (id, tenant_id, name, pool, token_id, state, provision_requested_at, tagged, launch_template)
		VALUES ('h3', 't1', 'burst-h3', 'burst', 'tok3', 'provisioning', now(), true, '{"region":"eu-west-1"}')`)
	f.cloud.add("i-3", map[string]string{tagManaged: "true", tagDeployment: "d1", tagPool: "t1/burst", tagHost: "h3"})
	f.rename(t, "burst", "burst-eu")
	g.open()
	if err := recv(t, f.ctx, done, "the pass"); err != nil {
		t.Fatal(err)
	}
	f.noneTerminated(t)
	if got := f.query(t, `SELECT coalesce(provider_id, '') FROM hosts WHERE id = 'h3'`); got != "i-3" {
		t.Errorf("h3 provider_id = %q, want i-3 recorded", got)
	}
	if got := f.cloud.tag("i-3", tagPool); got != "t1/burst-eu" {
		t.Errorf("i-3 lux:pool = %q, want re-tagged to the pool's name", got)
	}
}

// An expired provisioner's burst → burst-eu CreateTags lands after its
// successor finished that rename and a second one, burst-eu → burst-us.
// The instances then carry a name no pool lists: looked up by id, found
// running, they are kept and re-tagged burst-us.
func TestRenameRaceLateRetagAfterSecondRename(t *testing.T) {
	f := newRenameFixture(t, true)
	f.rename(t, "burst", "burst-eu")
	g := f.gate(t, true, func(call string, tags map[string]string) bool {
		return call == "Retag" && tags[tagPool] == "t1/burst-eu"
	})
	late := f.pass(t, f.s, f.pool(t), takeLease(t, f.ctx, f.s))
	g.waitEntered(t, "the first provisioner's CreateTags")

	execSQL(t, f.s, f.ctx, `UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = 'provisioner'`)
	s2 := f.otherLuxd(t)
	l2 := takeLease(t, f.ctx, s2)
	converge := func(name string) {
		t.Helper()
		for range 2 {
			if err := s2.reconcilePool(f.ctx, f.cloud, f.pool(t), true, l2); err != nil {
				t.Fatal(err)
			}
			time.Sleep(f.s.cfg.ListingLag)
		}
		if err := s2.reconcilePool(f.ctx, f.cloud, f.pool(t), true, l2); err != nil {
			t.Fatal(err)
		}
		if pl := f.pool(t); pl.Name != name || pl.RenamedFrom != nil {
			t.Fatalf("pool %s renamed from %v, want %s finished", pl.Name, pl.RenamedFrom, name)
		}
	}
	converge("burst-eu")
	execSQL(t, f.s, f.ctx, `UPDATE pools SET rename_finished_at = now() - interval '1 hour'`)
	f.rename(t, "burst-eu", "burst-us")
	converge("burst-us")
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-us" {
			t.Fatalf("%s lux:pool = %q before the late re-tag", pid, got)
		}
	}

	g.open()
	recv(t, f.ctx, late, "the first provisioner's pass")
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-eu" {
			t.Fatalf("%s lux:pool = %q: the late re-tag did not land", pid, got)
		}
	}
	if err := s2.reconcilePool(f.ctx, f.cloud, f.pool(t), true, l2); err != nil {
		t.Fatal(err)
	}
	f.noneTerminated(t)
	for _, pid := range []string{"i-1", "i-2"} {
		if got := f.cloud.tag(pid, tagPool); got != "t1/burst-us" {
			t.Errorf("%s lux:pool = %q, want re-tagged to burst-us", pid, got)
		}
	}
}

// A settled host missing from its pool's listing (its lux:pool is not the
// pool's) is looked up by id: running, it is kept and re-tagged.
func TestRenameRaceUnlistedRunningHostIsKept(t *testing.T) {
	f := newRenameFixture(t, true)
	g := f.gate(t, false, listing("t1/burst"))
	done := f.pass(t, f.s, f.pool(t), takeLease(t, f.ctx, f.s))
	g.waitEntered(t, "the pass's listing")
	if err := f.cloud.Retag(f.ctx, nil, []string{"i-1"}, tagPool, "t1/elsewhere"); err != nil {
		t.Fatal(err)
	}
	g.open()
	if err := recv(t, f.ctx, done, "the pass"); err != nil {
		t.Fatal(err)
	}
	f.noneTerminated(t)
	if got := f.cloud.tag("i-1", tagPool); got != "t1/burst" {
		t.Errorf("i-1 lux:pool = %q, want re-tagged t1/burst", got)
	}
	// One the provider says is gone is written off.
	f.cloud.before = nil
	if err := f.cloud.Terminate(f.ctx, nil, "i-2"); err != nil {
		t.Fatal(err)
	}
	f.cloud.hidden = map[string]bool{"i-2": true}
	f.check(t, f.pool(t))
	if got := f.query(t, `SELECT state FROM hosts WHERE id = 'h2'`); got != "terminated" {
		t.Errorf("h2 is %s after the provider said its instance was terminated", got)
	}
	if got := f.query(t, `SELECT state FROM hosts WHERE id = 'h1'`); got == "terminated" {
		t.Error("h1 written off")
	}
}

// otherLuxd is a second luxd process on the fixture's database.
func (f *renameFixture) otherLuxd(t *testing.T) *Server {
	t.Helper()
	s2 := New(f.s.cfg, f.s.db, nil, f.s.log)
	s2.id = "luxd-other"
	s2.deployment = f.s.deployment
	if err := s2.checkIn(f.ctx); err != nil {
		t.Fatal(err)
	}
	return s2
}

// The lease's token stays while one luxd renews it, and is new whenever
// another takes it, also after a release (the row deleted).
func TestProvisionLeaseToken(t *testing.T) {
	f := newRenameFixture(t, false)
	a := takeLease(t, f.ctx, f.s)
	if again := takeLease(t, f.ctx, f.s); again.token != a.token {
		t.Errorf("renewal changed the token %d → %d", a.token, again.token)
	}
	s2 := f.otherLuxd(t)
	if l, err := s2.provisionLease(f.ctx); err != nil || l != nil {
		t.Fatalf("another luxd took a held lease: %v, %v", l, err)
	}
	f.s.releaseProvisionLease()
	b := takeLease(t, f.ctx, s2)
	if b.token == a.token {
		t.Errorf("a new holder after a release kept token %d", a.token)
	}
	if err := f.s.renewLease(f.ctx, a); !errors.Is(err, errFenced) {
		t.Errorf("the first holder renewing after losing the lease: %v", err)
	}
	execSQL(t, f.s, f.ctx, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	c := takeLease(t, f.ctx, f.s)
	if c.token == b.token || c.token == a.token {
		t.Errorf("taking over an expired lease kept a token (%d; before %d, %d)", c.token, a.token, b.token)
	}

	// The same luxd taking its own lease again after it expired: a new
	// token, and the pass that held it before is fenced.
	execSQL(t, f.s, f.ctx, `UPDATE leases SET expires_at = now() - interval '1 second'`)
	if err := f.s.renewLease(f.ctx, c); !errors.Is(err, errFenced) {
		t.Errorf("renewing an expired lease: %v, want fenced", err)
	}
	if got := f.query(t, `SELECT (expires_at < now())::text FROM leases`); got != "true" {
		t.Error("a refused renewal took the lease again")
	}
	d := takeLease(t, f.ctx, f.s)
	if d.token == c.token {
		t.Errorf("re-taking its own expired lease kept token %d", c.token)
	}
	if err := f.s.writeOff(f.ctx, c, "h1", "test"); !errors.Is(err, errFenced) {
		t.Errorf("the earlier pass's write-off after its lease was re-taken: %v, want fenced", err)
	}
	if got := f.query(t, `SELECT state FROM hosts WHERE id = 'h1'`); got == "terminated" {
		t.Error("the earlier pass wrote a host off")
	}
	if err := f.s.renewLease(f.ctx, d); err != nil {
		t.Errorf("renewing the current lease: %v", err)
	}
}

// A launch whose RunInstances, sent with the old tag, is answered only
// after the rename committed and finished (its host row timed out
// meanwhile): the instance is listed under the pool's alias and, claimed
// by no live row, terminated as an orphan, never leaked.
func TestRenameRaceLateLaunchUnderTheOldTag(t *testing.T) {
	f := newRenameFixture(t, false)
	f.cloud.launches = true
	execSQL(t, f.s, f.ctx, `UPDATE pools SET min_hosts = 3`)
	g := f.gate(t, true, func(call string, tags map[string]string) bool { return call == "Launch" })
	launching := f.pass(t, f.s, f.pool(t), takeLease(t, f.ctx, f.s))
	g.waitEntered(t, "the launch's RunInstances")
	if got := f.query(t, `SELECT pool FROM hosts WHERE provider_id IS NULL`); got != "burst" {
		t.Fatalf("the launching row is in pool %q", got)
	}
	execSQL(t, f.s, f.ctx, `UPDATE pools SET min_hosts = 0`)

	f.rename(t, "burst", "burst-eu")
	// The launch outlives LaunchTimeout: its row is written off.
	execSQL(t, f.s, f.ctx, `UPDATE hosts SET provision_requested_at = now() - interval '1 day' WHERE provider_id IS NULL`)
	f.check(t, f.pool(t))
	time.Sleep(f.s.cfg.ListingLag)
	f.check(t, f.pool(t))
	if pl := f.pool(t); pl.RenamedFrom != nil {
		t.Fatalf("rename unfinished (from %v) with no launch in flight", *pl.RenamedFrom)
	}

	g.open()
	if err := recv(t, f.ctx, launching, "the launching pass"); err != nil {
		t.Fatal(err)
	}
	late := f.cloud.launchedIDs()
	if len(late) != 1 || f.cloud.tag(late[0], tagPool) != "t1/burst" {
		t.Fatalf("launched %v, want one instance tagged with the old name", late)
	}
	f.check(t, f.pool(t))
	if got := f.cloud.terminatedIDs(); !slices.Equal(got, late) {
		t.Fatalf("terminated %v, want the late launch %v", got, late)
	}
	for _, h := range []string{"h1", "h2"} {
		if got := f.query(t, `SELECT state FROM hosts WHERE id = $1`, h); got == "terminated" {
			t.Errorf("%s written off", h)
		}
	}
}

// A renewal that waited for the lease row's lock past the lease's expiry
// is refused: expiry is judged by the clock after the wait, not by the
// transaction's start.
func TestLeaseRenewalAfterALockWait(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	l := takeLease(t, f.ctx, f.s)
	execSQL(t, f.s, ctx, `UPDATE leases SET expires_at = now() + interval '300 milliseconds'`)
	tx := systemTx(t, ctx, f.s)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE name = 'provisioner' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.renewLease(ctx, l) }()
	time.Sleep(600 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, f.ctx, done, "the renewal"); !errors.Is(err, errFenced) {
		t.Fatalf("a renewal that waited past the expiry: %v, want fenced", err)
	}
}

// Likewise taking the lease: this luxd's own lease that expired while the
// acquisition waited for the row's lock is taken with a new token.
func TestLeaseAcquisitionAfterALockWait(t *testing.T) {
	f := newRenameFixture(t, false)
	ctx := f.ctx
	l := takeLease(t, f.ctx, f.s)
	execSQL(t, f.s, ctx, `UPDATE leases SET expires_at = now() + interval '300 milliseconds'`)
	tx := systemTx(t, ctx, f.s)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE name = 'provisioner' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	type got struct {
		l   *passLease
		err error
	}
	done := make(chan got, 1)
	go func() {
		var g got
		g.l, g.err = f.s.provisionLease(ctx)
		done <- g
	}()
	time.Sleep(600 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	g := recv(t, f.ctx, done, "the acquisition")
	if g.err != nil || g.l == nil {
		t.Fatalf("taking the lease after it expired: %v, %v", g.l, g.err)
	}
	if g.l.token == l.token {
		t.Fatalf("kept token %d across an expiry", l.token)
	}
}
