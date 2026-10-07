package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// backoffFixture: planningFixture with one waiting Run, a provider whose
// launches fail with a non-capacity error, and a clock the test moves.
func backoffFixture(t *testing.T) (*Server, poolRow, *planningProvider, *time.Time) {
	t.Helper()
	s, pl, p := planningFixture(t, 1)
	p.fail = true
	now := time.Date(2026, 10, 7, 13, 52, 16, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, pl, p, &now
}

// backoffPass is one provisioner pass for the pool; a launch error is
// expected on an attempt and ignored.
func backoffPass(s *Server, pl poolRow, p *planningProvider) bool {
	before := p.calls
	_ = s.reconcilePool(context.Background(), p, pl, false)
	return p.calls > before
}

// Launches that keep failing are attempted 15s after the first failure,
// then 30s, 60s ... capped at 5m; the passes in between launch nothing and
// record one pool.scale_blocked, its count one per waiting pass, saying why.
func TestFailedLaunchesBackOffPerPool(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	var attempts []time.Duration
	passes := 0
	for off := time.Duration(0); off <= 1065*time.Second; off += 5 * time.Second {
		*now = start.Add(off)
		passes++
		if backoffPass(s, pl, p) {
			attempts = append(attempts, off)
		}
	}
	want := []time.Duration{0, 15 * time.Second, 45 * time.Second, 105 * time.Second, 225 * time.Second,
		465 * time.Second, 765 * time.Second, 1065 * time.Second}
	if !slices.Equal(attempts, want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	*now = start.Add(1070 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("launched 5s after a failure")
	}
	blocked := passes + 1 - len(want)
	evs := events(t, s, evScaleBlocked)
	if len(evs) != 1 || evs[0].Count != blocked {
		t.Fatalf("scale_blocked events %+v, want one with count %d", evs, blocked)
	}
	d := evs[0].Data
	if d["cause"] != causeBackoff || d["failures"] != float64(8) || d["error"] != "launch refused" {
		t.Errorf("scale_blocked data %+v", d)
	}
	if want := "launch backing off after 8 failures; next attempt in 4m55s: launch refused"; d["detail"] != want {
		t.Errorf("detail %q, want %q", d["detail"], want)
	}
	if n := len(events(t, s, evLaunchFailed)); n != 1 {
		t.Errorf("%d launch_failed events, want one folded", n)
	}
	if c := queryOne[int](t, s, `SELECT count FROM pool_events WHERE type = $1`, evLaunchFailed); c != len(want) {
		t.Errorf("launch_failed count %d, want one per attempt (%d)", c, len(want))
	}
}

// A change to the pool's stored configuration (here its template) ends
// the backoff: the next pass tries the fix at once.
func TestPoolChangeResetsLaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	backoffPass(s, pl, p)
	*now = start.Add(15 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("no second attempt after 15s")
	}
	*now = start.Add(16 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted during the backoff")
	}
	execSQL(t, s, context.Background(), `UPDATE pools SET template = '{"version":2}' WHERE id = 'pool1'`)
	pl.Template = json.RawMessage(`{"version":2}`)
	*now = start.Add(17 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("a changed template waited out the backoff")
	}
	// The reset starts over: the next attempt is 15s after this failure.
	*now = start.Add(31 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted 14s after the failure")
	}
	*now = start.Add(32 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("no attempt 15s after the failure that followed the change")
	}
}

// A luxd that loses the provisioner lease and takes it again forgets its
// backoffs: another holder may have launched for the pool in between.
func TestRetakenLeaseResetsLaunchBackoff(t *testing.T) {
	s, p, pass := provisionPasses(t)
	ctx := context.Background()
	pass(0)
	pass(15 * time.Second)
	if p.calls != 2 {
		t.Fatalf("%d attempts, want 2 before the lease is lost", p.calls)
	}
	execSQL(t, s, ctx, `UPDATE leases SET holder='other', expires_at=now()+interval '1 minute' WHERE name='provisioner'`)
	pass(20 * time.Second)
	if p.calls != 2 {
		t.Fatalf("attempted a launch without the lease")
	}
	execSQL(t, s, ctx, `UPDATE leases SET expires_at=now()-interval '1 second' WHERE name='provisioner'`)
	// Before the old deadline (15s + 30s).
	pass(25 * time.Second)
	if p.calls != 3 {
		t.Fatalf("%d attempts, want one on the retaken lease", p.calls)
	}
}

// provisionPasses is backoffFixture driven through provision(), the
// provisioner's own pass with its lease checks; pass fails the test on an
// error.
func provisionPasses(t *testing.T) (*Server, *planningProvider, func(time.Duration)) {
	t.Helper()
	s, _, p, now := backoffFixture(t)
	s.cfg.Providers = map[string]Provider{"ec2": p}
	start := *now
	return s, p, func(off time.Duration) {
		t.Helper()
		*now = start.Add(off)
		if err := s.provision(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// A luxd whose lease expired and was held by another luxd meanwhile, all
// without a pass of its own noticing, takes a new lease: its backoffs
// start afresh.
func TestNewLeaseEpochResetsLaunchBackoff(t *testing.T) {
	s, p, pass := provisionPasses(t)
	pass(0)
	pass(15 * time.Second)
	if p.calls != 2 {
		t.Fatalf("%d attempts, want 2 before the handover", p.calls)
	}
	execSQL(t, s, context.Background(), `UPDATE leases SET holder='other', expires_at=now()-interval '1 second' WHERE name='provisioner'`)
	// Before the old deadline (15s + 30s).
	pass(25 * time.Second)
	if p.calls != 3 {
		t.Fatalf("%d attempts, want one on the new lease", p.calls)
	}
}

// Renewing a lease this luxd still holds keeps the backoff.
func TestLeaseRenewalKeepsLaunchBackoff(t *testing.T) {
	_, p, pass := provisionPasses(t)
	pass(0)
	pass(15 * time.Second)
	for _, off := range []time.Duration{16 * time.Second, 30 * time.Second, 44 * time.Second} {
		pass(off)
		if p.calls != 2 {
			t.Fatalf("attempted at %v, during the backoff", off)
		}
	}
	pass(45 * time.Second)
	if p.calls != 3 {
		t.Fatalf("%d attempts, want one when the backoff ends", p.calls)
	}
}

// A lease check between two launches that fails marks the lease lost: the
// next pass that holds it starts afresh, even when the database row turns
// out to be this luxd's unexpired lease (the check failed transiently).
func TestFailedCheckBetweenLaunchesResetsLaunchBackoff(t *testing.T) {
	s, pa, pass := provisionPasses(t)
	ctx := context.Background()
	steal := &stealLeaseOnLaunch{planningProvider: pa, s: s, pool: "pool2"}
	s.cfg.Providers = map[string]Provider{"ec2": steal}
	pass(0)
	pass(15 * time.Second)
	if pa.calls != 2 {
		t.Fatalf("%d attempts, want 2 for pool A", pa.calls)
	}
	// Pool B wants two hosts; its first launch takes the lease away, so the
	// check before the second fails. B is listed after A, so that check is
	// the pass's last.
	execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template,min_hosts) VALUES ('pool2','t1','other','ec2','{"version":1}',2)`)
	pass(20 * time.Second)
	if steal.launched != 1 || pa.calls != 2 {
		t.Fatalf("pool B launched %d and pool A attempted %d, want 1 and 2", steal.launched, pa.calls)
	}
	if evs := events(t, s, evScaleBlocked); len(evs) != 1 || evs[0].Count != 1 {
		t.Fatalf("scale_blocked %+v, want pool A blocked once, before pool B's launch", evs)
	}
	execSQL(t, s, ctx, `UPDATE leases SET holder=$1, expires_at=now()+interval '1 minute' WHERE name='provisioner'`, instanceID)
	pass(25 * time.Second)
	if pa.calls != 3 {
		t.Fatalf("%d attempts for pool A, want one after the failed check", pa.calls)
	}
}

// stealLeaseOnLaunch launches pool's hosts, the first of them handing the
// provisioner lease to another holder; other pools' launches go to the
// planningProvider.
type stealLeaseOnLaunch struct {
	*planningProvider
	s        *Server
	pool     string
	launched int
}

func (p *stealLeaseOnLaunch) Launch(ctx context.Context, tmpl json.RawMessage, tags, env map[string]string) (Launched, error) {
	if tags[tagPoolID] != p.pool {
		return p.planningProvider.Launch(ctx, tmpl, tags, env)
	}
	p.launched++
	if p.launched == 1 {
		if err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE leases SET holder='other', expires_at=now()+interval '1 minute' WHERE name='provisioner'`)
			return err
		}); err != nil {
			return Launched{}, err
		}
	}
	return Launched{ProviderID: fmt.Sprintf("i-b%d", p.launched)}, nil
}

// A successful launch resets the count: the next failure waits 15s, not
// the doubled delay.
func TestSuccessfulLaunchResetsLaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	backoffPass(s, pl, p)
	p.fail = false
	*now = start.Add(15 * time.Second)
	if !backoffPass(s, pl, p) || len(p.hosts) != 1 {
		t.Fatal("no successful launch after 15s")
	}
	// The host goes away; a replacement is wanted and fails.
	s.markTerminated(context.Background(), p.hosts[0], "the provider terminated this host")
	p.fail = true
	*now = start.Add(16 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the replacement waited after a success")
	}
	*now = start.Add(30 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted 14s after the failure")
	}
	*now = start.Add(31 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the failure after a success waited longer than 15s")
	}
}

// A launch the provider accepted resets the count even when recording it
// fails afterwards: the next failure waits 15s, not the doubled delay.
func TestProviderSuccessResetsLaunchBackoffWhenItsWriteFails(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	backoffPass(s, pl, p)
	*now = start.Add(15 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("no second attempt after 15s")
	}
	p.fail = false
	*now = start.Add(45 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cut := &cancelAfterLaunch{planningProvider: p, cancel: cancel}
	if err := s.reconcilePool(ctx, cut, pl, false); err == nil || len(p.hosts) != 1 {
		t.Fatalf("pass returned %v with %d launched, want a write error after one launch", err, len(p.hosts))
	}
	s.markTerminated(context.Background(), p.hosts[0], "the provider terminated this host")
	p.fail = true
	*now = start.Add(46 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the replacement waited after a provider success")
	}
	*now = start.Add(60 * time.Second)
	if backoffPass(s, pl, p) {
		t.Fatal("attempted 14s after the failure")
	}
	*now = start.Add(61 * time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("the failure after a provider success waited longer than 15s")
	}
}

// cancelAfterLaunch cancels the pass's context once the provider has
// launched, so writing the launch's result fails.
type cancelAfterLaunch struct {
	*planningProvider
	cancel context.CancelFunc
}

func (p *cancelAfterLaunch) Launch(ctx context.Context, tmpl json.RawMessage, tags, env map[string]string) (Launched, error) {
	l, err := p.planningProvider.Launch(ctx, tmpl, tags, env)
	p.cancel()
	return l, err
}

// One pool's backoff is its own: pool B launches while pool A backs off,
// and neither B's successful launches nor its configuration change cut
// short A's schedule (attempts at 0, 15, 45 and 105s).
func TestLaunchBackoffIsPerPool(t *testing.T) {
	s, a, pa, now := backoffFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id,tenant_id,name,provider,template,min_hosts) VALUES ('pool2','t1','other','ec2','{"version":1}',1)`)
	b := poolRow{ID: "pool2", Name: "other", Provider: "ec2", TenantID: new("t1"), Template: json.RawMessage(`{"version":1}`), Min: 1}
	pb := &planningProvider{}
	start := *now
	var attempts []time.Duration
	for off := time.Duration(0); off <= 110*time.Second; off += time.Second {
		*now = start.Add(off)
		switch off {
		case 16 * time.Second:
			if !backoffPass(s, b, pb) {
				t.Fatal("pool B did not launch during A's backoff")
			}
		case 30 * time.Second:
			execSQL(t, s, ctx, `UPDATE pools SET min_hosts = 2 WHERE id = 'pool2'`)
			b.Min = 2
			if !backoffPass(s, b, pb) {
				t.Fatal("pool B did not launch for its new minimum")
			}
		}
		if backoffPass(s, a, pa) {
			attempts = append(attempts, off)
		}
	}
	want := []time.Duration{0, 15 * time.Second, 45 * time.Second, 105 * time.Second}
	if !slices.Equal(attempts, want) {
		t.Fatalf("pool A attempted at %v, want %v", attempts, want)
	}
	outcomes := func(pool string) string {
		return queryOne[string](t, s, `SELECT coalesce(string_agg(launch_outcome, ',' ORDER BY provision_requested_at, launch_outcome), '')
			FROM hosts WHERE pool_id = $1`, pool)
	}
	if got := outcomes("pool1"); got != "failed,failed,failed,failed" {
		t.Errorf("pool A launch outcomes %q, want four failures", got)
	}
	if got := outcomes("pool2"); got != "launched,launched" {
		t.Errorf("pool B launch outcomes %q, want two launches", got)
	}
}

// Attempts whose errors differ (EC2 names the subnet's zone) are still one
// backoff: one pool.scale_blocked row, holding the latest error.
func TestLaunchBackoffFoldsAcrossDifferentErrors(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	start := *now
	zoned := &zonedFailure{planningProvider: p}
	for _, off := range []time.Duration{0, 5 * time.Second, 15 * time.Second, 20 * time.Second, 25 * time.Second} {
		*now = start.Add(off)
		_ = s.reconcilePool(context.Background(), zoned, pl, false)
	}
	if zoned.calls != 2 {
		t.Fatalf("%d attempts, want 2", zoned.calls)
	}
	evs := events(t, s, evScaleBlocked)
	if len(evs) != 1 || evs[0].Count != 3 {
		t.Fatalf("scale_blocked events %+v, want one with count 3", evs)
	}
	if e := evs[0].Data["error"]; e != "no capacity in zone 2" {
		t.Errorf("error %q, want the latest", e)
	}
}

// zonedFailure fails every launch with an error naming a different zone.
type zonedFailure struct{ *planningProvider }

func (p *zonedFailure) Launch(context.Context, json.RawMessage, map[string]string, map[string]string) (Launched, error) {
	p.calls++
	return Launched{}, fmt.Errorf("no capacity in zone %d", p.calls)
}

// The backoff's error is cut at 200 characters, never inside one.
func TestLaunchBackoffErrorIsCutOnACharacter(t *testing.T) {
	ascii := strings.Repeat("a", 199)
	for _, tc := range []struct{ name, msg, want string }{
		{"multi-byte at the boundary", ascii + "é and more", ascii + "é"},
		{"multi-byte throughout", strings.Repeat("é", 300), strings.Repeat("é", 200)},
		{"short", "no capacity, request id: 0123-abcd", "no capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bo := &poolBackoff{failures: 1, lastErr: errors.New(tc.msg)}
			got := bo.evidence(time.Now())["error"].(string)
			if got != tc.want || !utf8.ValidString(got) {
				t.Errorf("error %q (%d runes), want %q", got, utf8.RuneCountInString(got), tc.want)
			}
		})
	}
}

// A launch refused by the tenant's host quota calls no provider and starts
// no backoff: once the quota allows it, the next pass launches.
func TestHostQuotaDoesNotStartALaunchBackoff(t *testing.T) {
	s, pl, p, now := backoffFixture(t)
	ctx := context.Background()
	p.fail = false
	execSQL(t, s, ctx, `UPDATE tenants SET max_hosts = 1 WHERE id = 't1'`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't1', 'other', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state) VALUES ('h-other', 'h-other', 't1', 'pool2', 'provisioning')`)
	if backoffPass(s, pl, p) {
		t.Fatal("launched past the quota")
	}
	if evs := events(t, s, evScaleBlocked); len(evs) != 1 || evs[0].Data["cause"] != causeQuota {
		t.Fatalf("scale_blocked %+v, want the quota", evs)
	}
	execSQL(t, s, ctx, `UPDATE tenants SET max_hosts = 0 WHERE id = 't1'`)
	*now = now.Add(time.Second)
	if !backoffPass(s, pl, p) {
		t.Fatal("a quota refusal started a backoff")
	}
}
