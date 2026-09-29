package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// drainState reads back a host's draining flag and its live placement's
// stop_requested_at and stop_reason (empty runID: only the host is checked).
func drainState(t *testing.T, s *Server, ctx context.Context, hostID, runID string) (draining bool, stopRequested bool, stopReason string) {
	t.Helper()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT draining FROM hosts WHERE id = $1`, hostID).Scan(&draining); err != nil {
			return err
		}
		if runID == "" {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT stop_requested_at IS NOT NULL, stop_reason FROM placements WHERE run_id = $1`, runID).
			Scan(&stopRequested, &stopReason)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// drainFixture is a tenant with one ready host and one running Run placed
// on it (current_epoch = 1, matching its placement, so requestStop can
// find it): the common setup every drain test starts from.
func drainFixture(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state) VALUES ('h1', 't1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)
}

// A competing Run transaction may hold the Run while an operator forces a
// drain. The drain must wait for it, then stop the committed placement.
func TestForceDrainWaitsForRunBeforeHost(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainFixture(t, s, ctx)
	locked := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'r1' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = now() WHERE id = 'h1'`)
			return err
		})
	}()
	<-locked
	actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	second := make(chan error, 1)
	go func() {
		_, err := s.drainHost(actx, &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}})
		second <- err
	}()
	// The second session has reached the Run row; releasing the first now
	// exercises the lock order without relying on a sleep or a deadlock timeout.
	waitForRunWaiter(t, s, ctx, "r1")
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if draining, stopped, reason := drainState(t, s, ctx, "h1", "r1"); !draining || !stopped || reason != "drain" {
		t.Fatalf("draining=%v stopped=%v reason=%q", draining, stopped, reason)
	}
}

func waitForRunWaiter(t *testing.T, s *Server, ctx context.Context, runID string) {
	t.Helper()
	for {
		var waiting bool
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
				AND pid <> pg_backend_pid() AND wait_event_type = 'Lock')`).Scan(&waiting)
		})
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("no Run lock waiter for %s: %v", runID, ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A force drain holding the Run commits its stop while the timeout reaper
// waits for that Run. The reaper must not replace the drain reason with timeout.
func TestTimeoutReaperInterleavesWithForceDrain(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"timeout":"1s"}', first_started_at = now() - interval '1 hour' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - interval '1 hour' WHERE id = 'p1'`)
	locked, release := make(chan int, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	drain := make(chan error, 1)
	go func() {
		drain <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockReaperRuns(ctx, tx, []string{"r1"}); err != nil {
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			locked <- pid
			<-release
			_, err := s.drainHosts(ctx, tx, "drain requested", causeManual, "drain", "id = $1", "h1")
			return err
		})
	}()
	drainPID := <-locked
	reaper := make(chan error, 1)
	go func() { reaper <- s.reapTimeouts(ctx) }()
	// Observe the reaper blocked by the drain's Run lock before committing
	// the stop; scheduling alone does not prove the reaper saw the due Run.
	for {
		var waiting bool
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))`, drainPID).Scan(&waiting)
		})
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("reaper did not wait for drain Run lock: %v", ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	for _, ch := range []chan error{drain, reaper} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if draining, stopped, reason := drainState(t, s, ctx, "h1", "r1"); !draining || !stopped || reason != "drain" {
		t.Fatalf("draining=%v stopped=%v reason=%q, want true/true/drain", draining, stopped, reason)
	}
	var runState string
	var timeoutStops int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = 'r1'`).Scan(&runState); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = 'r1' AND data->>'reason' = 'timeout'`).Scan(&timeoutStops)
	}); err != nil {
		t.Fatal(err)
	}
	if runState != StateStopping || timeoutStops != 0 {
		t.Fatalf("run state=%q timeout events=%d, want stopping/0", runState, timeoutStops)
	}
}

// Reconciliation takes the Run lock after registration commits. A competing
// session holding that Run can update the host and commit before reconciliation.
func TestHelloReconciliationWaitsForRunBeforeHost(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok := testHostToken(t, s, ctx)
	hostID := hello(t, s, ctx, tok, "", "")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		VALUES ('p1', 't1', 'r1', $1, 1, 'running')`, hostID)
	locked := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'r1' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = now() WHERE id = $1`, hostID)
			return err
		})
	}()
	<-locked
	second := make(chan error, 1)
	go func() {
		_, err := s.registerHost(ctx, tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64"})
		second <- err
	}()
	waitForRunWaiter(t, s, ctx, "r1")
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM placements WHERE id = 'p1'`).Scan(&state)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "lost" {
		t.Fatalf("placement state = %q, want lost after hello omitted it", state)
	}
}

// The reaper discovers a stale host before locking its Run. A heartbeat
// committed while it waits prevents both retirement and placement loss.
func TestStaleHostReaperRechecksAfterRunLock(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'h1'`)
	locked := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'r1' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = now() WHERE id = 'h1'`)
			return err
		})
	}()
	<-locked
	second := make(chan error, 1)
	go func() { second <- s.reapHosts(ctx) }()
	waitForRunWaiter(t, s, ctx, "r1")
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	var hostState, placementState string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM hosts WHERE id = 'h1'`).Scan(&hostState); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state FROM placements WHERE id = 'p1'`).Scan(&placementState)
	}); err != nil {
		t.Fatal(err)
	}
	if hostState != "ready" || placementState != "running" {
		t.Fatalf("host=%q placement=%q, want ready/running", hostState, placementState)
	}
}

func waitForLockWaiters(t *testing.T, s *Server, ctx context.Context, want int) {
	t.Helper()
	for {
		var waiting int
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting)
		})
		if err != nil {
			t.Fatal(err)
		}
		if waiting >= want {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("wanted %d lock waiters: %v", want, ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Hold the host advisory lock while the two real paths reach their lock
// boundaries, then release it to check that neither can invert Run/host order.
func TestForceDrainInterleavesWithHostReaper(t *testing.T) {
	for _, first := range []string{"drain", "reaper"} {
		t.Run(first, func(t *testing.T) {
			s := testServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			drainFixture(t, s, ctx)
			execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'h1'`)
			gate := make(chan struct{})
			gateReady := make(chan struct{})
			gateErr := make(chan error, 1)
			go func() {
				gateErr <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					if err := lockCostHost(ctx, tx, "h1"); err != nil {
						return err
					}
					close(gateReady)
					<-gate
					return nil
				})
			}()
			<-gateReady
			actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
			drain := func() error {
				_, err := s.drainHost(actx, &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}})
				return err
			}
			actions := map[string]func() error{"drain": drain, "reaper": func() error { return s.reapHosts(ctx) }}
			one := make(chan error, 1)
			two := make(chan error, 1)
			go func() { one <- actions[first]() }()
			waitForLockWaiters(t, s, ctx, 1)
			other := "drain"
			if first == "drain" {
				other = "reaper"
			}
			go func() { two <- actions[other]() }()
			waitForLockWaiters(t, s, ctx, 2)
			close(gate)
			for _, ch := range []chan error{gateErr, one, two} {
				if err := <-ch; err != nil {
					t.Fatal(err)
				}
			}
			var state string
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT state FROM placements WHERE id = 'p1'`).Scan(&state)
			}); err != nil {
				t.Fatal(err)
			}
			if state != "lost" {
				t.Fatalf("placement = %q, want lost on stale host", state)
			}
		})
	}
}

// The runner's registration commits before reconciliation. With both paths
// contending on the same host, either Run-first winner must complete.
func TestForceDrainInterleavesWithHelloReconciliation(t *testing.T) {
	for _, first := range []string{"drain", "hello"} {
		t.Run(first, func(t *testing.T) {
			s := testServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
			tok := testHostToken(t, s, ctx)
			tenant := "t1"
			tok.TenantID = &tenant
			hostID := hello(t, s, ctx, tok, "", "")
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
				VALUES ('p1', 't1', 'r1', $1, 1, 'running')`, hostID)
			gate := make(chan struct{})
			ready := make(chan struct{})
			gateErr := make(chan error, 1)
			go func() {
				gateErr <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					if err := lockCostHost(ctx, tx, hostID); err != nil {
						return err
					}
					close(ready)
					<-gate
					return nil
				})
			}()
			<-ready
			actx := context.WithValue(ctx, principalKey, Principal{TenantID: tenant, Scopes: []string{"admin"}})
			actions := map[string]func() error{
				"drain": func() error {
					_, err := s.drainHost(actx, &drainHostInput{HostPath: HostPath{ID: hostID}, Body: &drainHostRequest{ForceEvict: true}})
					return err
				},
				"hello": func() error {
					_, err := s.registerHost(ctx, tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64"})
					return err
				},
			}
			one, two := make(chan error, 1), make(chan error, 1)
			go func() { one <- actions[first]() }()
			waitForLockWaiters(t, s, ctx, 1)
			other := "drain"
			if first == "drain" {
				other = "hello"
			}
			go func() { two <- actions[other]() }()
			waitForLockWaiters(t, s, ctx, 2)
			close(gate)
			for _, ch := range []chan error{gateErr, one, two} {
				if err := <-ch; err != nil {
					t.Fatal(err)
				}
			}
			draining, _, _ := drainState(t, s, ctx, hostID, "r1")
			if !draining {
				t.Fatal("force drain was lost after hello")
			}
			var state string
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT state FROM placements WHERE id = 'p1'`).Scan(&state)
			}); err != nil {
				t.Fatal(err)
			}
			if state != "lost" {
				t.Fatalf("placement = %q, want lost after hello omitted it", state)
			}
		})
	}
}

// A pool host that becomes eligible while the drain waits on an advisory
// lock must be discovered in a new transaction, not updated unlocked.
func TestForceDeletePoolRetriesNewlyEligibleHost(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "other", "default")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id) VALUES
		('h1', 't1', 'h1', 'pool1', 'ready', 'i-1'),
		('h2', 't1', 'h2', 'other', 'ready', 'i-2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r2', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state)
		VALUES ('p2', 't1', 'r2', 'h2', 1, 'running')`)
	gate := make(chan struct{})
	ready := make(chan struct{})
	gateErr := make(chan error, 1)
	go func() {
		gateErr <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockCostHost(ctx, tx, "h1"); err != nil {
				return err
			}
			close(ready)
			<-gate
			return nil
		})
	}()
	<-ready
	actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	done := make(chan error, 1)
	go func() {
		_, err := s.deletePool(actx, &deletePoolInput{Name: "burst", ForceEvict: true})
		done <- err
	}()
	waitForLockWaiters(t, s, ctx, 1)
	execSQL(t, s, ctx, `UPDATE hosts SET pool_id = 'pool1' WHERE id = 'h2'`)
	close(gate)
	if err := <-gateErr; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"h1", "h2"} {
		if draining, _, _ := drainState(t, s, ctx, host, ""); !draining {
			t.Fatalf("%s not drained", host)
		}
	}
	if _, stopped, reason := drainState(t, s, ctx, "h2", "r2"); !stopped || reason != "drain" {
		t.Fatalf("newly eligible placement: stopped=%v reason=%q", stopped, reason)
	}
}

// Eviction uses the same Run-first drain path as an operator force drain.
func TestRunnerEvictionInterleavesWithForceDrain(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainFixture(t, s, ctx)
	gate, ready := make(chan struct{}), make(chan struct{})
	gateErr := make(chan error, 1)
	go func() {
		gateErr <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockCostHost(ctx, tx, "h1"); err != nil {
				return err
			}
			close(ready)
			<-gate
			return nil
		})
	}()
	<-ready
	actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	one, two := make(chan error, 1), make(chan error, 1)
	go func() { one <- s.hostEvicting(ctx, "h1", proto.Evicting{Reason: "spot"}) }()
	waitForLockWaiters(t, s, ctx, 1)
	go func() {
		_, err := s.drainHost(actx, &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}})
		two <- err
	}()
	waitForLockWaiters(t, s, ctx, 2)
	close(gate)
	for _, ch := range []chan error{gateErr, one, two} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if draining, stopped, reason := drainState(t, s, ctx, "h1", "r1"); !draining || !stopped || reason != "preempt" {
		t.Fatalf("draining=%v stopped=%v reason=%q, want preempt to retain priority", draining, stopped, reason)
	}
}

// A plain drainHost cordons the host but leaves its live placement alone;
// forceEvict then stops it, even though the host is already draining.
func TestDrainHostCordonsThenForceEvictStopsRun(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})

	if _, err := s.drainHost(ctx, &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if draining, stopRequested, _ := drainState(t, s, context.Background(), "h1", "r1"); !draining || stopRequested {
		t.Fatalf("after plain drain: draining=%v stopRequested=%v", draining, stopRequested)
	}

	in := &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}}
	if _, err := s.drainHost(ctx, in); err != nil {
		t.Fatal(err)
	}
	draining, stopRequested, reason := drainState(t, s, context.Background(), "h1", "r1")
	if !draining || !stopRequested || reason != "drain" {
		t.Fatalf("after forceEvict on an already-draining host: draining=%v stopRequested=%v stopReason=%q, want draining/true/\"drain\"", draining, stopRequested, reason)
	}
}

// A POST with no body at all (curl scripts, and clients generated from
// the spec, send none) must still cordon the host cordon-only: the body
// is optional, not required.
func TestDrainHostHTTPWithNoBodyCordonsOnly(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('key1', 't1', 'k', $1, ARRAY['admin'])`, ids.Hash(key))

	h := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/hosts/h1/drain", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /drain with no body: %d %s, want 202", w.Code, w.Body)
	}
	var body struct {
		Draining bool `json:"draining"`
	}
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Draining {
		t.Error("response says draining=false")
	}

	draining, stopRequested, _ := drainState(t, s, context.Background(), "h1", "r1")
	if !draining {
		t.Error("host was not cordoned")
	}
	if stopRequested {
		t.Error("a bodiless drain requested a stop; want cordon-only")
	}
}

// deletePool cordons the pool's provisioned hosts and retires the pool;
// only with forceEvict are their live Runs stopped.
func TestDeletePoolForceEvict(t *testing.T) {
	for _, forceEvict := range []bool{false, true} {
		t.Run(fmt.Sprintf("forceEvict=%v", forceEvict), func(t *testing.T) {
			s := testServer(t)
			namedPools(t, s, "other", "default")
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
			execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
			execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id) VALUES ('h1', 't1', 'h1', 'pool1', 'ready', 'i-123')`)
			execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

			actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
			if _, err := s.deletePool(actx, &deletePoolInput{Name: "burst", ForceEvict: forceEvict}); err != nil {
				t.Fatal(err)
			}

			draining, stopRequested, reason := drainState(t, s, ctx, "h1", "r1")
			if !draining {
				t.Error("the pool's host was not cordoned")
			}
			if stopRequested != forceEvict || (forceEvict && reason != "drain") {
				t.Errorf("stopRequested=%v stopReason=%q, want a \"drain\" stop only with forceEvict", stopRequested, reason)
			}
			var retired bool
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT retired FROM pools WHERE id = 'pool1'`).Scan(&retired)
			}); err != nil {
				t.Fatal(err)
			}
			if !retired {
				t.Error("the pool was not retired")
			}
		})
	}
}

// deletePool never touches hosts outside the pool it removes: neither a
// platform host in another pool, nor a static host inside the very pool
// being deleted (deletePool only selects provider_id IS NOT NULL rows).
func TestDeletePoolOnlyTouchesItsOwnHosts(t *testing.T) {
	s := testServer(t)
	namedPools(t, s, "other", "default")
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state) VALUES ('h-other', 't1', 'h-other', 'default', 'ready')`)
	// A static host inside the pool being deleted: no provider_id, so it
	// stays uncordoned (the provisioner never terminates it either).
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state) VALUES ('h-static', 't1', 'h-static', 'pool1', 'ready')`)

	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	in := &deletePoolInput{Name: "burst", ForceEvict: true}
	if _, err := s.deletePool(ctx, in); err != nil {
		t.Fatal(err)
	}

	draining, _, _ := drainState(t, s, context.Background(), "h-other", "")
	if draining {
		t.Error("deletePool cordoned a host outside the deleted pool")
	}
	draining, _, _ = drainState(t, s, context.Background(), "h-static", "")
	if draining {
		t.Error("deletePool cordoned a static host in the deleted pool")
	}
}

// A plain drain on a host already draining for outdated binaries adds its
// own cause ("manual") alongside "outdated" rather than clobbering it:
// both coexist in drain_causes, and state_reason is free to show
// whichever drain called last (display text, not a marker). Once
// "manual" is present the reaper leaves the host alone (the operator
// owns it now: see TestOutdatedAndManualDrainCoexistAcrossAnUndrain for
// the exit-skipped and undrain-order coverage); this test only checks
// that the earlier cause was not clobbered.
func TestPlainDrainKeepsAnEarlierDrainReason(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)

	// First, drained for outdated binaries (as drainIfOutdated does).
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := s.drainHosts(ctx, tx, outdatedBinariesReason, causeOutdated, "", "id = $1", "h1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, causes, _, _ := hostState(t, s, ctx, "h1"); !slices.Contains(causes, causeOutdated) {
		t.Fatalf("after the outdated-binaries drain: drain_causes = %v, want it to contain %q", causes, causeOutdated)
	}

	// Then a plain drain (an operator's) on the same host: must add its
	// own cause, not remove the earlier one.
	ctx = context.WithValue(ctx, principalKey, Principal{Operator: true, Scopes: []string{"admin", "operator"}})
	if _, err := s.drainHost(ctx, &drainHostInput{HostPath: HostPath{ID: "h1"}}); err != nil {
		t.Fatal(err)
	}
	_, _, reason, causes, _, _ := hostState(t, s, ctx, "h1")
	if !slices.Contains(causes, causeOutdated) || !slices.Contains(causes, causeManual) {
		t.Fatalf("after a plain drain on top: drain_causes = %v, want both %q and %q", causes, causeOutdated, causeManual)
	}
	if reason != "drain requested" {
		t.Fatalf("after a plain drain on top: state_reason = %q, want the latest drain's display text", reason)
	}
}
