package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func TestSchedulerFitWaitAndPlacementResources(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "pool")
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `UPDATE pools SET provider = 'ec2' WHERE id = 'pool'`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('h', 'h', 'pool', 'ready', '{"cpus":1,"memory":4096}', now())`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('r', 't1', '{"placement":{"pool":"pool"},"resources":{"cpus":2.5,"memory":8192,"disk":4096}}', 'submitted')`)
	s.hub.polled("h")
	for _, capacity := range []float64{1, 2} {
		execSQL(t, s, ctx, `UPDATE hosts SET capacity = jsonb_build_object('cpus', $1::float8, 'memory', 4096) WHERE id = 'h'`, capacity)
		if err := s.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
		var state, reason string
		systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'r'`, nil, &state, &reason)
		// Not the host's numbers: the reason stays the same as its capacity changes.
		want := "waiting for capacity: 1 host in its pool lacks cpus (requested 2.5), 1 lacks memory (requested 8.0 KiB)"
		if state != StateProvisioning || reason != want {
			t.Fatalf("Run = %s/%q, want provisioning/%q", state, reason, want)
		}
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = '{"cpus":4,"memory":16384}' WHERE id = 'h'`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'r'`, nil, &state)
	if state != StateScheduled {
		t.Fatalf("state = %s", state)
	}
	for _, query := range []string{
		`SELECT data FROM host_events WHERE data->>'run' = 'r'`,
		`SELECT data FROM pool_events WHERE data->>'run' = 'r'`,
	} {
		var data json.RawMessage
		systemScan(t, s, query, nil, &data)
		var event struct {
			Resources struct {
				CPUs   float64
				Memory int64
				Disk   int64
			}
		}
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Resources.CPUs != 2.5 || event.Resources.Memory != 8192 || event.Resources.Disk != 4096 {
			t.Fatalf("placement resources = %+v", event.Resources)
		}
	}
}

func TestPickHostAffinityAndSnapshotWait(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	r, cached := fitFixture()
	r.Spec.Image.Ref = "image"
	cached.Images = []string{"image"}
	local := *cached
	local.ID = "local"
	local.Images = nil
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('local', 'local', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r', 't1', '{}', 'resuming')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p', 't1', 'r', 'local', 1, 'exited')`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, host_id, manifest, available, uploaded, host_copy)
		VALUES ('snap', 't1', 'r', 'p', 1, 'local', '{}', true, false, true)`)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		h, wait, err := s.pickHost(ctx, tx, r, []*candidateHost{&local, cached})
		if err != nil {
			return err
		}
		if h != cached || wait != "" {
			return fmt.Errorf("image affinity: %v / %q", h, wait)
		}
		r.SnapshotID = new("snap")
		h, wait, err = s.pickHost(ctx, tx, r, []*candidateHost{cached, &local})
		if err != nil {
			return err
		}
		if h != &local || wait != "" {
			return fmt.Errorf("snapshot affinity: %v / %q", h, wait)
		}
		local.Capacity.CPUs = 1
		h, wait, err = s.pickHost(ctx, tx, r, []*candidateHost{cached, &local})
		if err != nil {
			return err
		}
		if h != nil || wait != "waiting for snapshot upload" {
			return fmt.Errorf("snapshot wait: %v / %q", h, wait)
		}
		if _, err := tx.Exec(ctx, `UPDATE snapshots SET uploaded = true WHERE id = 'snap'`); err != nil {
			return err
		}
		h, wait, err = s.pickHost(ctx, tx, r, []*candidateHost{cached, &local})
		if err != nil {
			return err
		}
		if h != cached || wait != "" {
			return fmt.Errorf("uploaded snapshot: %v / %q", h, wait)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A scheduler waiting on a finalizer's host lock must not admit a host
// registered after discovery into its placement decision.
func TestSchedulerCandidateLockBoundary(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "default", "other", "unrelated")
	s.cfg.LeaseDuration = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	tok := testHostToken(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat) VALUES
		('a', 'a', 'default', 'ready', '{"runs":1}', now()),
		('b', 'b', 'default', 'lost', '{"runs":1}', now())`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
		('old', 't1', '{}', 'running', 1),
		('new', 't1', '{"image":{"ref":"preferred"},"placement":{"pool":"default"}}', 'submitted', 0)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		VALUES ('p-old', 't1', 'old', 'a', 1, 'running')`)
	s.hub.polled("a")
	s.hub.polled("b")

	entered := make(chan int, 1)
	release := make(chan struct{})
	finalized := make(chan error, 1)
	go func() {
		finalized <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'old' FOR UPDATE`); err != nil {
				return err
			}
			if err := lockCostHost(ctx, tx, "a"); err != nil {
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			entered <- pid
			<-release
			code := 0
			return s.placementExited(ctx, tx, "t1", "old", 1,
				proto.Status{State: "exited", ExitCode: &code}, StateRunning, false)
		})
	}()
	var finalizerPID int
	select {
	case finalizerPID = <-entered:
	case err := <-finalized:
		t.Fatalf("finalizer before lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
		if err := <-finalized; err != nil {
			t.Errorf("finalizer: %v", err)
		}
	}()

	scheduled := make(chan error, 1)
	go func() {
		_, _, err := s.scheduleBatch(ctx, cursorPos{})
		scheduled <- err
	}()
	// The advisory wait proves discovery finished before the hello commits.
	for {
		var waiting bool
		systemScan(t, s, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity a
			WHERE a.datname = current_database() AND a.wait_event = 'advisory'
			  AND $1 = ANY(pg_blocking_pids(a.pid)))`, []any{finalizerPID}, &waiting)
		if waiting {
			break
		}
		select {
		case err := <-scheduled:
			t.Fatalf("scheduler finished before finalization: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := s.registerHost(ctx, tok, proto.Hello{
		Name: "b", ProtocolVersion: proto.Version, Arch: "arm64",
		Capacity: proto.Capacity{Runs: 1}, Images: []string{"preferred"},
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	released = true
	select {
	case err := <-scheduled:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var host, state string
	systemScan(t, s, `SELECT p.host_id, r.state FROM placements p JOIN runs r ON r.id = p.run_id
		WHERE p.run_id = 'new'`, nil, &host, &state)
	if host != "a" || state != StateScheduled {
		t.Fatalf("new Run: host=%q state=%q, want a/scheduled", host, state)
	}
	var oldState, placementState string
	systemScan(t, s, `SELECT r.state, p.state FROM runs r JOIN placements p ON p.run_id = r.id
		WHERE r.id = 'old'`, nil, &oldState, &placementState)
	if oldState != StateSucceeded || placementState != "exited" {
		t.Fatalf("finalized Run: %q/%q", oldState, placementState)
	}
}

func TestSchedulerUnrelatedHostLockDoesNotBlock(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "default", "other", "unrelated")
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, last_heartbeat) VALUES
		('a-other', 'a-other', 'other', 'ready', now()),
		('z-target', 'z-target', 'default', 'ready', now())`)

	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockCostHost(ctx, tx, "a-other"); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Errorf("unrelated host lock: %v", err)
		}
	}()

	deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.db.Tx(deadline, store.System(), func(tx pgx.Tx) error {
		ids, err := s.eligibleHostIDs(deadline, tx, []*string{new("default")}, []string{"t1"}, []string{""})
		if err != nil {
			return err
		}
		if !slices.Equal(ids, []string{"z-target"}) {
			return fmt.Errorf("discovered %v, want [z-target]", ids)
		}
		for _, id := range ids {
			if err := lockCostHost(deadline, tx, id); err != nil {
				return err
			}
		}
		hosts, err := s.candidateHosts(deadline, tx, ids)
		if err != nil {
			return err
		}
		if len(hosts) != 1 || hosts[0].ID != "z-target" {
			return fmt.Errorf("candidates: %+v", hosts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerEligibleHostsScopeAndRevalidation(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "default", "other", "unrelated")
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	for _, h := range []struct{ id, pool, tenant string }{
		{"a", "default", "t1"}, {"b", "other", ""}, {"c", "default", "t2"},
		{"d", "unrelated", ""}, {"e", "default", ""},
	} {
		var tenant any
		if h.tenant != "" {
			tenant = h.tenant
		}
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, tenant_id, state, last_heartbeat)
			VALUES ($1, $1, $2, $3, 'ready', now())`, h.id, h.pool, tenant)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		ids, err := s.eligibleHostIDs(ctx, tx,
			[]*string{new("default"), new("other")}, []string{"t1", "t2"}, []string{"", "c"})
		if err != nil {
			return err
		}
		if !slices.Equal(ids, []string{"a", "b", "c", "e"}) {
			return fmt.Errorf("mixed batch discovered %v, want [a b c e]", ids)
		}
		ids, err = s.eligibleHostIDs(ctx, tx,
			[]*string{new("default")}, []string{"t1"}, []string{"c"})
		if err != nil {
			return err
		}
		if !slices.Equal(ids, []string{"a", "e"}) {
			return fmt.Errorf("cross-tenant choice discovered %v, want [a e]", ids)
		}
		ids, err = s.eligibleHostIDs(ctx, tx,
			[]*string{new("default"), new("other")}, []string{"t1", "t2"}, []string{"", ""})
		if err != nil {
			return err
		}
		if !slices.Equal(ids, []string{"a", "b", "e"}) {
			return fmt.Errorf("mixed pools discovered %v, want [a b e]", ids)
		}
		ids, err = s.eligibleHostIDs(ctx, tx, []*string{new("default")}, []string{"t1"}, []string{"b"})
		if err != nil {
			return err
		}
		if !slices.Equal(ids, []string{"a", "b", "e"}) {
			return fmt.Errorf("discovered %v, want [a b e]", ids)
		}
		for _, id := range ids {
			if err := lockCostHost(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET draining = true WHERE id = 'e'`); err != nil {
			return err
		}
		hosts, err := s.candidateHosts(ctx, tx, ids)
		if err != nil {
			return err
		}
		got := make([]string, 0, len(hosts))
		for _, h := range hosts {
			got = append(got, h.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"a", "b"}) {
			return fmt.Errorf("revalidated %v, want [a b]", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A Run's wait reason counts only hosts it could otherwise use: another
// tenant's hosts in the same batch are neither named nor counted.
func TestSchedulerWaitReasonTenantScope(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'ta'), ('tb', 'tb')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pa', 'ta', 'pa', 'ec2'), ('pb', 'tb', 'pb', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state, capacity, last_heartbeat) VALUES
		('a-full', 'a-full', 'ta', 'pa', 'ready', '{"cpus":1}', now()),
		('tenant-b-secret-host', 'b', 'tb', 'pb', 'ready', '{"cpus":1}', now())`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('ra', 'ta', 'pa', '{"placement":{"pool":"pa"},"resources":{"cpus":2}}', 'submitted'),
		('rb', 'tb', 'pb', '{"placement":{"pool":"pb"},"resources":{"cpus":2}}', 'submitted')`)
	s.hub.polled("a-full")
	s.hub.polled("tenant-b-secret-host")
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for run, host := range map[string]string{"ra": "tenant-b-secret-host", "rb": "a-full"} {
		var reason string
		systemScan(t, s, `SELECT state_reason FROM runs WHERE id = $1`, []any{run}, &reason)
		if want := "waiting for capacity: 1 host in its pool lacks cpus (requested 2)"; reason != want || strings.Contains(reason, host) {
			t.Fatalf("%s reason %q, want %q", run, reason, want)
		}
	}
}

// Many full hosts summarise to a bounded reason.
func TestSchedulerWaitReasonBounded(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool', 't1', 'pool', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state, capacity, last_heartbeat)
		SELECT 'full-' || i, 'full-' || i, 't1', 'pool', 'ready', '{"cpus":1,"memory":1024}', now() FROM generate_series(1, 200) i`)
	for i := 1; i <= 200; i++ {
		s.hub.polled(fmt.Sprintf("full-%d", i))
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state) VALUES
		('r', 't1', 'pool', '{"placement":{"pool":"pool","requires":{"zone":"z"}},"resources":{"cpus":2,"memory":2048,"disk":1}}', 'submitted')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	systemScan(t, s, `SELECT state_reason FROM runs WHERE id = 'r'`, nil, &reason)
	want := "waiting for capacity: 200 hosts in its pool lack cpus (requested 2), 200 lack memory (requested 2.0 KiB), 200 lack its required labels"
	if reason != want || len(reason) > maxWaitReason {
		t.Fatalf("reason (%d bytes) %q, want %q", len(reason), reason, want)
	}
}

// Usage changing on a host that still cannot take the waiting Runs rewrites
// neither their rows nor their events.
func TestSchedulerWaitReasonStableAcrossUsage(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool', 't1', 'pool', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state, capacity, last_heartbeat)
		VALUES ('h', 'h', 't1', 'pool', 'ready', '{"cpus":4}', now())`)
	s.hub.polled("h")
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('live', 't1', '{}', 'running')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES ('p', 't1', 'live', 'h', 1, 'running', '{"cpus":3}')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state)
		SELECT 'w' || i, 't1', 'pool', '{"placement":{"pool":"pool"},"resources":{"cpus":2}}', 'submitted' FROM generate_series(1, 5) i`)
	snapshot := func() (string, int) {
		var xmins string
		var events int
		systemScan(t, s, `SELECT string_agg(xmin::text, ',' ORDER BY id), (SELECT count(*) FROM run_events WHERE run_id LIKE 'w%')
			FROM runs WHERE id LIKE 'w%'`, nil, &xmins, &events)
		return xmins, events
	}
	for range 2 {
		if err := s.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rows, evs := snapshot()
	for _, cpus := range []string{`{"cpus":3.5}`, `{"cpus":2.5}`} {
		execSQL(t, s, ctx, `UPDATE placements SET resources = $1 WHERE id = 'p'`, cpus)
		if err := s.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if r, e := snapshot(); r != rows || e != evs {
			t.Fatalf("usage %s rewrote waiting Runs: rows %s→%s, events %d→%d", cpus, rows, r, evs, e)
		}
	}
}
