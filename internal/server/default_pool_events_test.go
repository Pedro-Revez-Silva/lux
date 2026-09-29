package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// isDefaultChanges is each pool's isDefault changes in its events, oldest
// first, as "old→new" (a created pool's old is null).
func isDefaultChanges(t *testing.T, s *Server) map[string][]string {
	t.Helper()
	rows := queryOne[[][]string](t, s, `SELECT coalesce(array_agg(ARRAY[p.name,
			coalesce(e.data->'changes'->'isDefault'->>'old', 'null') || '→' || (e.data->'changes'->'isDefault'->>'new')] ORDER BY e.id), '{}')
		FROM pool_events e JOIN pools p ON p.id = e.pool_id WHERE e.data->'changes' ? 'isDefault'`)
	got := map[string][]string{}
	for _, r := range rows {
		got[r[0]] = append(got[r[0]], r[1])
	}
	return got
}

// Moving the mark is a config_changed on each pool it moves between; a
// removal of the default says it lost the mark in its pool.retired.
func TestDefaultMarkMovesAreRecorded(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static", MaxHosts: 2})
	mustPut(t, s, "t1", Pool{Name: "b", IsDefault: mark(true)})
	// Setting a pool without isDefault, or marking the default again, moves nothing.
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static", MaxHosts: 3})
	mustPut(t, s, "t1", Pool{Name: "b", IsDefault: mark(true)})
	want := map[string][]string{"a": {"null→true", "true→false"}, "b": {"null→false", "false→true"}}
	if got := isDefaultChanges(t, s); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("isDefault changes %v, want %v", got, want)
	}
	// The marker-only put changed nothing else: its event names isDefault alone.
	marked := queryOne[map[string]any](t, s, `SELECT e.data->'changes' FROM pool_events e JOIN pools p ON p.id = e.pool_id
		WHERE p.name = 'b' AND NOT (e.data->>'created')::bool AND e.data->'changes' ? 'isDefault'`)
	if len(marked) != 1 {
		t.Fatalf("marking b recorded %v, want isDefault only", marked)
	}

	if _, err := s.deletePool(tenantCtx("t1"), &deletePoolInput{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	retired := events(t, s, evRetired)
	if len(retired) != 1 || fmt.Sprint(retired[0].Data["changes"].(map[string]any)["isDefault"]) != fmt.Sprint(map[string]any{"old": true, "new": false}) {
		t.Fatalf("retired events %+v, want b losing its mark", retired)
	}
}

// A mark is written with its events or not at all: an event refused
// leaves the mark where it was, on either pool it would move between.
func TestDefaultMarkIsAtomicWithItsEvents(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static"})
	refuseEvent(t, s, evConfigChanged)
	for _, pl := range []Pool{{Name: "b", IsDefault: mark(true)}, {Name: "b", Provider: "static", MaxHosts: 2, IsDefault: mark(true)}} {
		if _, err := putPoolAs(t, s, "t1", pl); err == nil {
			t.Fatalf("put %+v succeeded with its event refused", pl)
		}
		if got := defaultPools(t, s); got["t1"] != "a" {
			t.Fatalf("defaults %v after a refused mark, want a", got)
		}
	}
}

// A mark waits, under the owner's default lock, for a write of that pool
// in flight (ChangePool's pool-name lock), and records its change from
// what that write committed.
func TestDefaultMarkWaitsForAPoolWriteInFlight(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	// b's first write, without a mark (putPool without isDefault).
	create := holdTx(ctx, s, func(tx pgx.Tx) error {
		return SavePool(ctx, tx, "t1", "b", nil, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, max_hosts) VALUES ('pb', 't1', 'b', 'static', 4)`)
			return err
		})
	})
	if !create.settle(t, ctx, s) {
		t.Fatal("the create waited on nothing")
	}
	marked := holdTx(ctx, s, func(tx pgx.Tx) error { return SetDefaultPool(ctx, tx, "t1", "b", true) })
	marked.waitsOnAdvisory(t, ctx, s, "pool-name:t1/b")
	create.finish(t, ctx)
	marked.finish(t, ctx)
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("defaults %v, want b", got)
	}
	want := map[string][]string{"a": {"null→true", "true→false"}, "b": {"null→false", "false→true"}}
	if got := isDefaultChanges(t, s); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("isDefault changes %v, want %v", got, want)
	}
}

// A removal of the default waits for a mark in flight at the owner's
// default lock, before it locks the pool the mark is moving off, and then
// retires a pool the mark has already left.
func TestPoolRemovalWaitsForAMarkInFlight(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "a", Provider: "static", IsDefault: mark(true)})
	mustPut(t, s, "t1", Pool{Name: "b", Provider: "static"})
	marked := holdTx(ctx, s, func(tx pgx.Tx) error { return SetDefaultPool(ctx, tx, "t1", "b", true) })
	if !marked.settle(t, ctx, s) {
		t.Fatal("the mark waited on nothing")
	}
	removed := make(chan error, 1)
	go func() {
		_, err := s.deletePool(asTenant(ctx, "t1"), &deletePoolInput{Name: "a"})
		removed <- err
	}()
	waitBlockedBy(t, s, ctx, marked.backend, removed)
	var onDefaultLock bool
	systemScan(t, s, `WITH k AS (SELECT hashtextextended('default-pool:t1', 0) AS key)
		SELECT EXISTS (SELECT 1 FROM pg_locks l, k WHERE NOT l.granted AND l.locktype = 'advisory'
			AND l.classid = ((k.key >> 32) & 4294967295)::oid AND l.objid = (k.key & 4294967295)::oid)`, nil, &onDefaultLock)
	if !onDefaultLock {
		t.Fatal("the removal is blocked, but not on the default-pool lock")
	}
	marked.finish(t, ctx)
	if err := await(t, ctx, removed, 1)[0]; err != nil {
		t.Fatalf("removal: %v", err)
	}
	if got := defaultPools(t, s); got["t1"] != "b" {
		t.Fatalf("defaults %v, want b", got)
	}
	retired := events(t, s, evRetired)
	if len(retired) != 1 || retired[0].Data["changes"].(map[string]any)["isDefault"] != nil {
		t.Fatalf("retired %+v: a had already lost its mark to b", retired)
	}
	want := map[string][]string{"a": {"null→true", "true→false"}, "b": {"null→false", "false→true"}}
	if got := isDefaultChanges(t, s); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("isDefault changes %v, want %v", got, want)
	}
}

// Marks of the default racing creates, removals and restores of the same
// pools (putPool, deletePool, luxd admin's SavePool): none deadlocks or
// fails but for a mark of a pool removed meanwhile (404); the owner ends
// with at most one default, never a retired one; and each pool's events,
// replayed, end at its mark: every transition, the pool that lost the
// mark included, was recorded once, in order.
func TestDefaultMarksRaceCreateRetireRestore(t *testing.T) {
	s := testServer(t)
	slowDeadlockCheck(t, s)
	ctx := testDeadline(t)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	names := []string{"a", "b", "c"}
	for round := range 15 {
		ops := []func() error{
			func() error {
				_, err := s.putPool(asTenant(ctx, "t1"), poolIn(Pool{Name: "a", IsDefault: mark(true)}))
				return err
			},
			func() error {
				_, err := s.putPool(asTenant(ctx, "t1"), poolIn(Pool{Name: "b", IsDefault: mark(true)}))
				return err
			},
			func() error {
				_, err := s.putPool(asTenant(ctx, "t1"), poolIn(Pool{Name: "c", Provider: "static", MaxHosts: round, IsDefault: mark(round%2 == 0)}))
				return err
			},
			func() error { _, err := s.deletePool(asTenant(ctx, "t1"), &deletePoolInput{Name: "a"}); return err },
			func() error { _, err := s.deletePool(asTenant(ctx, "t1"), &deletePoolInput{Name: "c"}); return err },
			func() error {
				_, err := s.putPool(asTenant(ctx, "t1"), poolIn(Pool{Name: "a", Provider: "static", MaxHosts: round}))
				return err
			},
			func() error {
				_, err := s.putPool(asTenant(ctx, "t1"), poolIn(Pool{Name: "b", Provider: "static", MaxHosts: round + 1}))
				return err
			},
		}
		done := make(chan error, len(ops))
		for _, op := range ops {
			go func() { done <- op() }()
		}
		for _, err := range await(t, ctx, done, len(ops)) {
			var he *HTTPError
			if err != nil && !(errors.As(err, &he) && he.Status == http.StatusNotFound) {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		var retiredMarked int
		systemScan(t, s, `SELECT count(*) FROM pools WHERE retired AND is_default`, nil, &retiredMarked)
		if retiredMarked != 0 {
			t.Fatalf("round %d: a retired pool is marked", round)
		}
		defaultPools(t, s)
		changes := isDefaultChanges(t, s)
		for _, n := range names {
			var isDefault bool
			var exists bool
			systemScan(t, s, `SELECT count(*) > 0, coalesce(bool_or(is_default), false) FROM pools WHERE tenant_id = 't1' AND name = $1`, []any{n}, &exists, &isDefault)
			if !exists {
				continue
			}
			prev := "null"
			for _, c := range changes[n] {
				old, new, _ := strings.Cut(c, "→")
				if old != prev {
					t.Fatalf("round %d: %s's isDefault events %v do not chain", round, n, changes[n])
				}
				prev = new
			}
			if prev != fmt.Sprint(isDefault) {
				t.Fatalf("round %d: %s's isDefault events %v end at %s, the pool is %v", round, n, changes[n], prev, isDefault)
			}
		}
	}
}
