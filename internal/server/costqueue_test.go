package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// pending is a Run's cost_pending row as "reason claimed_by", or "" when it
// has none.
func pending(t *testing.T, s *Server, runID string) string {
	t.Helper()
	ctx := context.Background()
	var out string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reason || ' ' || coalesce(claimed_by, '-') FROM cost_pending WHERE run_id = $1`, runID).Scan(&out)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return out
}

// systemScan scans q's one row into dest, in the system scope.
func systemScan(t *testing.T, s *Server, q string, args []any, dest ...any) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(dest...)
	}); err != nil {
		t.Fatal(err)
	}
}

// Leaving running queues the Run's costs in the state change's own
// transaction, whatever the scope (an API stop runs as the tenant); a live
// state doesn't. A rolled-back change queues nothing, and several changes
// merge into one row.
func TestSetRunStateQueuesCosts(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		state string
		scope store.Scope
		want  string
	}{
		{StateStarting, store.System(), ""},
		{StateRunning, store.System(), ""},
		{StateStopping, store.Tenant("t1"), "state:stopping -"},
		{StateStopped, store.System(), "state:stopped -"},
		{StateLost, store.System(), "state:lost -"},
		{StateSucceeded, store.Tenant("t1"), "state:succeeded -"},
		{StateFailed, store.System(), "state:failed -"},
		{StateCancelled, store.Tenant("t1"), "state:cancelled -"},
	} {
		execSQL(t, s, ctx, `DELETE FROM cost_pending`)
		if err := s.db.Tx(ctx, c.scope, func(tx pgx.Tx) error {
			return setRunState(ctx, tx, "t1", "r1", c.state, "", 1)
		}); err != nil {
			t.Fatal(err)
		}
		if got := pending(t, s, "r1"); got != c.want {
			t.Errorf("%s: queued %q, want %q", c.state, got, c.want)
		}
	}

	// Rolled back: nothing queued.
	execSQL(t, s, ctx, `DELETE FROM cost_pending`)
	rollback := errors.New("rollback")
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := setRunState(ctx, tx, "t1", "r1", StateFailed, "", 1); err != nil {
			return err
		}
		if got := pending(t, s, "r1"); got != "" {
			t.Errorf("visible before commit: %q", got)
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got := pending(t, s, "r1"); got != "" {
		t.Errorf("rolled back, yet queued %q", got)
	}

	// Several changes, one row: the earliest due_at stays, the last reason
	// wins, and a claim is freed (its result was read before the change).
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
		VALUES ('r1', now() - interval '1 hour', 'tick', 'other', now() + interval '1 minute')`)
	for _, st := range []string{StateStopping, StateStopped, StateCancelled} {
		if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
			return setRunState(ctx, tx, "t1", "r1", st, "", 1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var early bool
	systemScan(t, s, `SELECT count(*), bool_and(due_at < now() - interval '59 minutes') FROM cost_pending`, nil, &n, &early)
	if got := pending(t, s, "r1"); n != 1 || !early || got != "state:cancelled -" {
		t.Errorf("merged into %d rows (earliest kept %v): %q", n, early, got)
	}

	// A tenant's scope queues only its own Runs, and never reads the queue.
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return enqueueCost(ctx, tx, "r2", "state:stopped")
	}); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, s, "r2"); got != "" {
		t.Errorf("t1 queued t2's run: %q", got)
	}
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_pending`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("t1 reads %d queue rows (%v), want 0", n, err)
	}
}

// peer is another luxd on s's database, named id in claims.
func peer(s *Server, id string) *Server {
	p := New(s.cfg, s.db, nil, s.log)
	p.id = id
	return p
}

// ticks counts cost_ticks rows.
func ticks(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	systemScan(t, s, `SELECT count(*) FROM cost_ticks`, nil, &n)
	return n
}

// Two luxd on one database, ticking at once: one tick per bucket. The
// winner queues every live Run and every Run whose compute is owed
// another attempt, and forgets ticks older than a day.
func TestCostTickOnePerBucket(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Every = time.Hour
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO cost_ticks (tick_at) VALUES (now() - interval '25 hours'), (now() - interval '23 hours')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('stopped', 't1', '{}', 'stopped'), ('owed', 't1', '{}', 'failed'), ('later', 't1', '{}', 'failed'),
		('done', 't1', '{}', 'succeeded'), ('stopping', 't2', '{}', 'stopping'),
		('scheduled', 't1', '{}', 'scheduled'), ('starting', 't1', '{}', 'starting')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('owed', 't1', 'compute', 'incomplete', now() - interval '1 second'),
		('later', 't1', 'compute', 'incomplete', now() + interval '1 hour'),
		('done', 't1', 'compute', 'final', NULL)`)
	// Already queued earlier: the tick keeps the earlier due_at.
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r2', now() - interval '1 hour', 'state:stopping')`)

	b := peer(s, "luxd-b")
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv := s
			if i%2 == 1 {
				srv = b
			}
			won, err := srv.costTick(ctx)
			if err != nil {
				t.Error(err)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d ticks in one bucket, want 1", wins.Load())
	}
	if n := ticks(t, s); n != 2 {
		t.Errorf("%d tick rows, want this bucket's and the one under a day old", n)
	}
	for run, want := range map[string]string{"r1": "tick -", "r2": "state:stopping -", "stopping": "tick -", "owed": "tick -",
		"scheduled": "tick -", "starting": "tick -", "stopped": "", "later": "", "done": ""} {
		if got := pending(t, s, run); got != want {
			t.Errorf("%s: queued %q, want %q", run, got, want)
		}
	}
	var early bool
	if systemScan(t, s, `SELECT due_at < now() - interval '59 minutes' FROM cost_pending WHERE run_id = 'r2'`, nil, &early); !early {
		t.Error("r2's earlier due_at not kept")
	}
}

func TestCostTickBackfillsTerminalComputeInBatches(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('historical-a','t1','{}','succeeded'), ('historical-b','t2','{}','failed'),
		('historical-c','t1','{}','cancelled'), ('historical-stopped','t1','{}','stopped')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES ('historical-c','t1','compute','final')`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	if pending(t, s, "historical-a") == "" || pending(t, s, "historical-b") != "" || pending(t, s, "historical-stopped") != "" || pending(t, s, "historical-c") != "" {
		t.Fatal("first tick should queue only the first missing terminal compute source")
	}
	execSQL(t, s, ctx, `DELETE FROM cost_ticks`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	if pending(t, s, "historical-b") == "" {
		t.Fatal("next tick did not advance past an already queued Run")
	}
}

func TestCostTickDiscoversHistoricalPluginSources(t *testing.T) {
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		out := make([]any, 0, len(req.Runs))
		for _, run := range req.Runs {
			out = append(out, map[string]any{"runId": run.RunID, "status": "ok", "final": true, "lines": []any{pluginLine()}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": out})
	})
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'lost' WHERE id = 'r2'`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('old-t1', 't1', '{}', 'failed'), ('old-t2', 't2', '{}', 'cancelled'),
		('existing', 't1', '{}', 'succeeded'), ('quiet', 't1', '{}', 'stopped')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('old-t1', 't1', 'compute', 'final', NULL), ('old-t2', 't2', 'compute', 'final', NULL),
		('existing', 't1', 'ledger', 'final', NULL), ('quiet', 't1', 'ledger', 'ok', NULL),
		('quiet', 't1', 'removed', 'incomplete', now() - interval '1 second'),
		('existing', 't1', 'removed', 'incomplete', now() - interval '1 second')`)
	if _, cost := getCost(t, s, keys["t1"], "old-t1"); cost.Final || cost.Status != "incomplete" {
		t.Fatalf("missing configured source appeared final before backfill: %+v", cost)
	}
	if won, err := s.costTick(ctx); err != nil || !won {
		t.Fatalf("tick: won %v, %v", won, err)
	}
	for run, want := range map[string]string{"old-t1": "tick -", "old-t2": "tick -", "existing": "tick -", "quiet": "", "r1": "tick -", "r2": "tick -"} {
		if got := pending(t, s, run); got != want {
			t.Errorf("%s: queued %q, want %q", run, got, want)
		}
	}
	drain(t, s)
	for _, c := range []struct{ id, key string }{{"old-t1", keys["t1"]}, {"old-t2", keys["t2"]}, {"r1", keys["t1"]}, {"r2", keys["t2"]}} {
		code, cost := getCost(t, s, c.key, c.id)
		if code != http.StatusOK || len(cost.Lines) != 1 || cost.Lines[0].Source != "ledger" || cost.Sources[len(cost.Sources)-1].Source != "ledger" {
			t.Errorf("%s: %d, %+v", c.id, code, cost)
		}
	}
	if code, _ := getCost(t, s, keys["t2"], "old-t1"); code != http.StatusNotFound {
		t.Errorf("t2 read t1's historical costs: %d", code)
	}
	if won, err := s.costTick(ctx); err != nil || won {
		t.Errorf("second tick: won %v, %v", won, err)
	}
	if got := pending(t, s, "old-t1"); got != "" {
		t.Errorf("settled historical run queued again: %q", got)
	}
	execSQL(t, s, ctx, `DELETE FROM cost_ticks`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2", "quiet"} {
		if got := pending(t, s, id); got != "" {
			t.Errorf("%s with existing source queued again: %q", id, got)
		}
	}
}

func TestPollDueCostSourcesBoundedWithoutRewritingClaims(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Batch = 2
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id IN ('r1', 'r2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('r3', 't1', '{}', 'failed'), ('r4', 't2', '{}', 'failed'), ('r5', 't2', '{}', 'failed')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('r1', 't1', 'compute', 'incomplete', now() - interval '1 hour'),
		('r2', 't2', 'compute', 'incomplete', now() - interval '1 hour'),
		('r3', 't1', 'compute', 'incomplete', now() - interval '1 hour'),
		('r4', 't2', 'compute', 'incomplete', now() - interval '1 hour'),
		('r5', 't2', 'compute', 'incomplete', now() - interval '1 hour')`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
		VALUES ('r1', now() - interval '1 hour', 'state:failed', 'peer', now() + interval '1 hour')`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason)
		VALUES ('r2', now() + interval '1 hour', 'retry')`)
	for pass, want := range []int{4, 5, 5} {
		if err := s.pollDueCostSources(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		systemScan(t, s, `SELECT count(*) FROM cost_pending`, nil, &count)
		if count != want {
			t.Fatalf("pass %d queued %d, want %d", pass, count, want)
		}
		if got := pending(t, s, "r1"); got != "state:failed peer" {
			t.Fatalf("claimed row changed: %q", got)
		}
		var delayed bool
		systemScan(t, s, `SELECT due_at > now() + interval '59 minutes' FROM cost_pending WHERE run_id = 'r2'`, nil, &delayed)
		if !delayed {
			t.Fatal("released retry was made due early")
		}
	}
	// A new retry due later is eligible despite the already-queued population.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r6', 't1', '{}', 'failed')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at)
		VALUES ('r6', 't1', 'compute', 'incomplete', now() - interval '1 second')`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, s, "r6"); got != "retry -" {
		t.Errorf("newly due retry: %q", got)
	}
}

func TestCostTickBoundedRetriesWithConcurrentPoll(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Batch = 3
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "ledger"}}
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id IN ('r1', 'r2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state)
		SELECT 'backlog-' || lpad(n::text, 4, '0'), 't1', '{}', 'failed' FROM generate_series(1, 1200) n`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at)
		SELECT id, tenant_id, 'ledger', 'incomplete', now() - interval '1 hour'
		FROM runs WHERE id LIKE 'backlog-%'`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
		VALUES ('backlog-0001', now() - interval '1 hour', 'retry', 'peer', now() + interval '1 hour'),
			('backlog-0002', now() + interval '1 hour', 'retry', NULL, NULL)`)
	// Existing plugin sources keep the backlog out of missing-source discovery.
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status)
		VALUES ('r1', 't1', 'ledger', 'final'), ('r2', 't2', 'ledger', 'final')`)
	type queueRow struct {
		due, claim             time.Time
		reason, owner, version string
	}
	readRow := func(id string) queueRow {
		t.Helper()
		var row queueRow
		var claim *time.Time
		var owner *string
		systemScan(t, s, `SELECT due_at, reason, claimed_by, claimed_until, xmin::text FROM cost_pending WHERE run_id = $1`, []any{id}, &row.due, &row.reason, &owner, &claim, &row.version)
		if owner != nil {
			row.owner = *owner
		}
		if claim != nil {
			row.claim = *claim
		}
		return row
	}
	claimed, delayed := readRow("backlog-0001"), readRow("backlog-0002")
	b := peer(s, "luxd-b")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, run := range []func() error{
		func() error { _, err := s.costTick(ctx); return err },
		func() error { return b.pollDueCostSources(ctx) },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := run(); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var queued int
	systemScan(t, s, `SELECT count(*) FROM cost_pending WHERE run_id LIKE 'backlog-%'`, nil, &queued)
	if queued < 3 || queued > 8 {
		t.Errorf("queued %d backlog rows, want between 3 and 8 after tick and poll", queued)
	}
	for id, before := range map[string]queueRow{"backlog-0001": claimed, "backlog-0002": delayed} {
		if after := readRow(id); after != before {
			t.Errorf("%s changed from %+v to %+v", id, before, after)
		}
	}
	// The next drain poll discovers more retries without another tick.
	if err := b.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	var after int
	systemScan(t, s, `SELECT count(*) FROM cost_pending WHERE run_id LIKE 'backlog-%'`, nil, &after)
	if after != queued+3 {
		t.Errorf("next poll queued %d more backlog rows, want 3", after-queued)
	}
}

func TestCostTickBatchesHistoricalMissingSources(t *testing.T) {
	s, _, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		out := make([]any, 0, len(req.Runs))
		for _, run := range req.Runs {
			out = append(out, map[string]any{"runId": run.RunID, "status": "ok", "final": true, "lines": []any{pluginLine()}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": out})
	})
	s.cfg.Costs.Batch = 3
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id IN ('r1', 'r2')`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
		VALUES ('r1', now() - interval '1 hour', 'state:failed', 'peer', now() + interval '1 hour'),
			('r2', now() + interval '1 hour', 'retry', NULL, NULL)`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state)
		SELECT 'historical-' || n, 't1', '{}',
			(ARRAY['succeeded', 'failed', 'cancelled', 'stopped', 'lost'])[(n - 1) % 5 + 1]
		FROM generate_series(1, 20) n`)
	readQueue := func(id string) (string, string) {
		t.Helper()
		var row, version string
		systemScan(t, s, `SELECT row_to_json(p)::text, xmin::text FROM cost_pending p WHERE run_id = $1`, []any{id}, &row, &version)
		return row, version
	}
	claimed, claimedVersion := readQueue("r1")
	delayed, delayedVersion := readQueue("r2")
	for cycle := 0; cycle < 7; cycle++ {
		if won, err := s.costTick(ctx); err != nil || !won {
			t.Fatalf("cycle %d tick: won %v, %v", cycle, won, err)
		}
		if err := s.pollDueCostSources(ctx); err != nil {
			t.Fatal(err)
		}
		var queued int
		systemScan(t, s, `SELECT count(*) FROM cost_pending WHERE run_id LIKE 'historical-%'`, nil, &queued)
		if want := min(3, 20-cycle*3); queued != want {
			t.Fatalf("cycle %d queued %d historical Runs, want %d", cycle, queued, want)
		}
		if row, version := readQueue("r1"); row != claimed || version != claimedVersion {
			t.Fatalf("cycle %d changed claimed row", cycle)
		}
		if row, version := readQueue("r2"); row != delayed || version != delayedVersion {
			t.Fatalf("cycle %d changed delayed row", cycle)
		}
		drain(t, s)
		var sources, remaining int
		systemScan(t, s, `SELECT count(*) FROM cost_sources WHERE run_id LIKE 'historical-%' AND source = 'ledger'`, nil, &sources)
		systemScan(t, s, `SELECT count(*) FROM cost_pending WHERE run_id LIKE 'historical-%'`, nil, &remaining)
		if want := min(20, (cycle+1)*3); sources != want || remaining != 0 {
			t.Fatalf("cycle %d: %d sources, %d pending; want %d sources and no pending", cycle, sources, remaining, want)
		}
		execSQL(t, s, ctx, `DELETE FROM cost_ticks`)
	}
	for _, state := range []string{"succeeded", "failed", "cancelled", "stopped", "lost"} {
		var n int
		systemScan(t, s, `SELECT count(*) FROM runs r JOIN cost_sources c ON c.run_id = r.id
			WHERE r.id LIKE 'historical-%' AND r.state = $1 AND c.source = 'ledger' AND c.status IN ('final', 'ok')`, []any{state}, &n)
		if n != 4 {
			t.Errorf("%s: %d backfilled sources, want 4", state, n)
		}
	}
	if won, err := s.costTick(ctx); err != nil || !won {
		t.Fatalf("post-backfill tick: won %v, %v", won, err)
	}
	var queued int
	systemScan(t, s, `SELECT count(*) FROM cost_pending WHERE run_id LIKE 'historical-%'`, nil, &queued)
	if queued != 0 {
		t.Errorf("post-backfill tick queued %d historical Runs", queued)
	}
}

func TestCostLoopPollsDueSourcesBetweenTicks(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Enabled = true
	s.cfg.Costs.Every = time.Hour
	s.cfg.Costs.DrainEvery = 20 * time.Millisecond
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id IN ('r1', 'r2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('due', 't1', '{}', 'failed'), ('removed', 't2', '{}', 'stopped')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at)
		VALUES ('due-host', 't1', 'due-host', 'ready', now() - interval '1 hour')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at)
		VALUES ('due-placement', 't1', 'due', 'due-host', 1, 'running', '{}', now() - interval '1 hour')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('due', 't1', 'compute', 'incomplete', now() + interval '200 milliseconds'),
		('removed', 't2', 'old-plugin', 'incomplete', now() - interval '1 second')`)
	// Consume this bucket before starting the loop, so only drain-cadence
	// polling can enqueue the near-due source.
	if won, err := s.costTick(ctx); err != nil || !won {
		t.Fatalf("initial tick: won %v, %v", won, err)
	}
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.costLoop(lctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	var attempted bool
	for time.Now().Before(deadline) {
		systemScan(t, s, `SELECT attempts > 0 FROM cost_sources WHERE run_id = 'due' AND source = 'compute'`, nil, &attempted)
		if attempted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !attempted {
		t.Error("near-due compute source was not retried before the next tick")
	}
	if got := pending(t, s, "removed"); got != "" {
		t.Errorf("removed plugin queued: %q", got)
	}
}

func TestPollDueCostSourcesConfiguredPluginsOnly(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "ledger"}}
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id IN ('r1', 'r2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('plugin-due', 't1', '{}', 'failed'), ('plugin-later', 't2', '{}', 'cancelled'),
		('removed-only', 't2', '{}', 'stopped'), ('final-only', 't1', '{}', 'succeeded')`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('plugin-due', 't1', 'ledger', 'incomplete', now() - interval '1 second'),
		('plugin-later', 't2', 'ledger', 'incomplete', now() + interval '1 hour'),
		('removed-only', 't2', 'removed', 'incomplete', now() - interval '1 second'),
		('final-only', 't1', 'ledger', 'final', now() - interval '1 second')`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]string{"plugin-due": "retry -", "plugin-later": "", "removed-only": "", "final-only": ""} {
		if got := pending(t, s, run); got != want {
			t.Errorf("%s: queued %q, want %q", run, got, want)
		}
	}
	// Polling does not postpone an earlier state-triggered queue entry.
	execSQL(t, s, ctx, `UPDATE cost_pending SET due_at = now() - interval '1 hour', reason = 'state:failed' WHERE run_id = 'plugin-due'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	var kept bool
	systemScan(t, s, `SELECT reason = 'state:failed' AND due_at < now() - interval '59 minutes'
		FROM cost_pending WHERE run_id = 'plugin-due'`, nil, &kept)
	if !kept {
		t.Error("poll replaced an earlier state-triggered queue entry")
	}
}

// A due Run is claimed by one luxd; another takes it only once the claim
// has expired. Rows not yet due are left.
func TestCostClaims(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	b := peer(s, "luxd-b")
	claim := func(srv *Server) []string {
		t.Helper()
		runs, err := srv.claimCosts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}
	for _, c := range []struct {
		name         string
		due, claimed string // SQL; claimed "": no claim
		want         bool
	}{
		{"due, unclaimed", "now()", "", true},
		{"not due yet", "now() + interval '1 minute'", "", false},
		{"claim expired", "now() - interval '5 minutes'", "now() - interval '1 second'", true},
		{"claim held", "now() - interval '5 minutes'", "now() + interval '1 minute'", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			execSQL(t, s, ctx, `DELETE FROM cost_pending`)
			execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason, claimed_by, claimed_until)
				VALUES ('r1', `+c.due+`, 'tick', CASE WHEN $1 THEN 'luxd-a' END, `+cmp.Or(c.claimed, "NULL")+`)`, c.claimed != "")
			got := claim(b)
			if (len(got) == 1) != c.want {
				t.Fatalf("claimed %v, want %v", got, c.want)
			}
			if c.want {
				if p := pending(t, s, "r1"); p != "tick luxd-b" {
					t.Errorf("row %q after the claim", p)
				}
				if again := claim(s); len(again) != 0 {
					t.Errorf("claimed twice: %v", again)
				}
			}
		})
	}
}

// Not enabled: no tick and no drain. Enabled: the loop does both.
func TestCostLoopEnabled(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r2', now(), 'state:stopped')`)

	s.cfg.Costs.Enabled = false
	s.cfg.Costs.DrainEvery = 10 * time.Millisecond
	lctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	s.costLoop(lctx)
	cancel()
	if n, p := ticks(t, s), pending(t, s, "r2"); n != 0 || p != "state:stopped -" {
		t.Fatalf("disabled: %d ticks, r2 queued %q", n, p)
	}

	s.cfg.Costs.Enabled = true
	lctx, cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.costLoop(lctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for pending(t, s, "r2") != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n, p := ticks(t, s), pending(t, s, "r2"); n != 1 || p != "" {
		t.Errorf("enabled: %d ticks, r2 still queued %q", n, p)
	}
}

// With drain_every an hour, the loop still drains at once while more is
// due (a full batch), and when a Run event wakes it.
func TestCostLoopWakes(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, Every: time.Hour, DrainEvery: time.Hour, Batch: 1}
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r3', 't1', '{}', 'running'), ('r4', 't1', '{}', 'stopped')`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r4', now(), 'state:stopped')`)
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	queued := func() int {
		var n int
		systemScan(t, s, `SELECT count(*) FROM cost_pending`, nil, &n)
		return n
	}
	until := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("not %s within 10s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// r4, and r1, r2 and r3 (queued by the tick): four batches of one.
	wg.Add(1)
	go func() { defer wg.Done(); s.costLoop(lctx) }()
	until("drained batch after batch", func() bool { return queued() == 0 })

	wg.Add(1)
	go func() { defer wg.Done(); s.listenLoop(lctx) }()
	until("listening", func() bool {
		var n int
		systemScan(t, s, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND query = 'LISTEN lux_events' AND state = 'idle'`, nil, &n)
		return n == 1
	})
	finish(t, s, "t1", "r3", StateStopped)
	until("woken by the state change", func() bool { return queued() == 0 })
}

// drain drains s's queue until nothing is due.
func drain(t *testing.T, s *Server) {
	t.Helper()
	for {
		n, err := s.drainCosts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// finish moves a Run to state through setRunState, as luxd does.
func finish(t *testing.T, s *Server, tenantID, runID, state string) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return setRunState(ctx, tx, tenantID, runID, state, "", 1)
	}); err != nil {
		t.Fatal(err)
	}
}

// resume resumes t1's Run through requestResume, in t1's scope, as the API
// does.
func resume(t *testing.T, s *Server, runID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return s.requestResume(ctx, tx, "t1", runID, nil, "resume")
	}); err != nil {
		t.Fatal(err)
	}
}

// computeView is a Run's compute lines as "item amount currency final",
// and its compute source as "status", read through the API with key.
func computeView(t *testing.T, s *Server, key, runID string) (lines []string, source string) {
	t.Helper()
	code, c := getCost(t, s, key, runID)
	if code != http.StatusOK {
		t.Fatalf("GET cost of %s: %d", runID, code)
	}
	for _, l := range c.Lines {
		if l.Source == "compute" {
			lines = append(lines, fmt.Sprintf("%s %s %s %v", l.Item, l.Amount, l.Currency, l.Final))
		}
	}
	for _, src := range c.Sources {
		if src.Source == "compute" {
			source = src.Status
		}
	}
	return lines, source
}

// costHosts: on tenant t1, "static" (8 CPUs, 32 GiB, $0.40/h from 10:00),
// "unpriced" (no price at all) and "late" (priced only from 10:30), all
// static hosts that registered at 10:00; and "ec2", launched at 10:00 as a
// spot m7i.2xlarge, with no rate from 10:20 to 10:40.
func costHosts(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at) VALUES
		('static', 't1', 'static', 'ready', $1), ('unpriced', 't1', 'unpriced', 'ready', $1), ('late', 't1', 'late', 'ready', $1)`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, registered_at, instance_type, market, zone)
		VALUES ('ec2', 't1', 'ec2', 'ready', $1, $1, 'm7i.2xlarge', 'spot', 'eu-west-1a')`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('static', $1, NULL, 0.40, 'USD', 8, $4, 'static'),
		('late', $2, NULL, 0.40, 'USD', 8, $4, 'static'),
		('ec2', $1, $3, 0.40, 'USD', 8, $4, 'aws-spot-history'),
		('ec2', $5, NULL, 0.40, 'USD', 8, $4, 'aws-spot-history')`, at("10:00"), at("10:30"), at("10:20"), 32*gib, at("10:40"))
}

// placeRun adds tenant's Run id, in state, with one placement on host.
func placeRun(t *testing.T, s *Server, tenantID, id, state, host string, p placementWindow) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, $2, '{}', $3)`, id, tenantID, state)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ($1, $2, $3, $4, 1, CASE WHEN $7::timestamptz IS NULL THEN 'running' ELSE 'exited' END,
			jsonb_build_object('cpus', $5::float8, 'memory', $6::int8), $8, $7)`,
		"p-"+id, tenantID, id, host, p.CPUs, p.Memory, p.To, p.From)
}

// The worked example (docs/costs.md, section 2) through the queue: each
// Run's line has its exact amount, an estimate until the Run is terminal.
// Finishing replaces the lines (never adds) and makes them final; resuming
// makes them estimates again.
func TestDrainComputeWorkedExample(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	for _, p := range workedExample {
		placeRun(t, s, "t1", p.RunID, StateStopping, "static", p)
	}
	for _, id := range []string{"A", "B", "C"} {
		finish(t, s, "t1", id, StateStopped)
	}
	drain(t, s)
	for run, want := range map[string]string{"A": "static 0.05 USD false", "B": "static 0.15 USD false", "C": "static 0.05 USD false"} {
		lines, src := computeView(t, s, keys["t1"], run)
		if fmt.Sprint(lines) != "["+want+"]" || src != "ok" {
			t.Errorf("%s: %v, source %q; want %s, ok", run, lines, src, want)
		}
		if p := pending(t, s, run); p != "" {
			t.Errorf("%s still queued: %q", run, p)
		}
	}
	_, c := getCost(t, s, keys["t1"], "B")
	d := c.Lines[0].Details["placements"].([]any)[0].(map[string]any)
	if len(c.Lines[0].Details["placements"].([]any)) != 1 || d["hostId"] != "static" || d["amount"] != "0.15" ||
		d["ratePerHour"] != "0.400000000" || d["share"] != 0.5 || d["cpus"] != 1.0 || d["epoch"] != 1.0 || c.Lines[0].Details["missingRate"] != nil {
		t.Errorf("B's details: %v", c.Lines[0].Details)
	}
	if !c.Lines[0].From.Equal(at("10:15")) || !c.Lines[0].To.Equal(at("11:00")) || c.Lines[0].Family != "compute" {
		t.Errorf("B's line: %+v", c.Lines[0])
	}

	// B finishes: one line still, now final, as is the source.
	finish(t, s, "t1", "B", StateSucceeded)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "B"); fmt.Sprint(lines) != "[static 0.15 USD true]" || src != "final" {
		t.Errorf("B finished: %v, source %q", lines, src)
	}
	if _, c := getCost(t, s, keys["t1"], "B"); c.Status != "final" || totals(c.Totals) != "/USD=0.15(f0.15,e0) " {
		t.Errorf("B finished: %s %s", c.Status, totals(c.Totals))
	}
	// A stopped Run is evaluated, never final.
	if lines, src := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" {
		t.Errorf("A stopped: %v, source %q", lines, src)
	}

	// Another tenant never reads these lines, and its own Run shows none.
	if code, _ := getCost(t, s, keys["t2"], "B"); code != http.StatusNotFound {
		t.Errorf("t2 reading t1's run: %d, want 404", code)
	}
	if lines, _ := computeView(t, s, keys["t2"], "r2"); len(lines) != 0 {
		t.Errorf("t2's run shows %v", lines)
	}

	// C failed (final) and resumed: its costs are an estimate again.
	finish(t, s, "t1", "C", StateFailed)
	drain(t, s)
	resume(t, s, "C")
	if lines, src := computeView(t, s, keys["t1"], "C"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" {
		t.Errorf("C resumed: %v, source %q", lines, src)
	}
}

// A missing rate leaves a placement unpriced until a retry finds an observation.
// A later observation covers the whole placement, and a finalized snapshot is
// not repriced by subsequent changes to the host's rate history.
func TestDrainComputeMissingRateRecovery(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	whole := place("", 2, 8, "10:00", "11:00")
	placeRun(t, s, "t1", "unpriced", StateRunning, "unpriced", whole)
	finish(t, s, "t1", "unpriced", StateSucceeded)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "unpriced"); len(lines) != 0 || src != "incomplete" {
		t.Fatalf("missing rate: %v, source %q", lines, src)
	}
	if row, next := sourceRow(t, s, "unpriced"); row != "incomplete 1 t t" || next <= 0 {
		t.Fatalf("missing rate retry: %s, next in %s", row, next)
	}
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('unpriced', $1, 0.40, 'USD', 8, $2, 'static')`, at("10:30"), 32*gib)
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'unpriced'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "unpriced"); fmt.Sprint(lines) != "[static 0.1 USD true]" || src != "final" {
		t.Errorf("recovered rate: %v, source %q", lines, src)
	}
	if row, _ := sourceRow(t, s, "unpriced"); row != "final 0 f f" {
		t.Errorf("recovered source: %s", row)
	}
}

// A spot placement freezes its first complete observation when it ends;
// later spot history does not reopen or reprice its compute source.
func TestDrainComputeSpotSnapshotFinal(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	placeRun(t, s, "t1", "spot", StateRunning, "ec2", place("", 2, 8, "10:00", "10:20"))
	finish(t, s, "t1", "spot", StateFailed)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "spot"); fmt.Sprint(lines) != "[m7i.2xlarge:spot 0.033333333 USD true]" || src != "final" {
		t.Fatalf("initial spot snapshot: %v, source %q", lines, src)
	}
	var before string
	systemScan(t, s, `SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-spot'`, nil, &before)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('ec2', $1, $2, 0.80, 'USD', 8, $3, 'aws-spot-history')`, at("10:10"), at("10:15"), 32*gib)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('spot', 'retry')`)
	drain(t, peer(s, "restarted"))
	var after string
	systemScan(t, s, `SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-spot'`, nil, &after)
	if after != before {
		t.Errorf("spot snapshot repriced from %s to %s", before, after)
	}
	if lines, src := computeView(t, s, keys["t1"], "spot"); fmt.Sprint(lines) != "[m7i.2xlarge:spot 0.033333333 USD true]" || src != "final" {
		t.Errorf("later spot history: %v, source %q", lines, src)
	}
}

// claimed is one drain's claim and read, not yet written.
type claimed struct {
	runs  []string
	evals map[string]*computeEval
	now   time.Time
}

// claimOne claims s's one due Run and evaluates it, as a drain does before
// it writes.
func claimOne(t *testing.T, s *Server) claimed {
	t.Helper()
	ctx := context.Background()
	runs, err := s.claimCosts(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("claimed %v (%v)", runs, err)
	}
	c := claimed{runs: runs}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		c.evals, c.now, err = loadComputeRuns(ctx, tx, runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// write writes c's result, as the drain goes on to.
func (c claimed) write(s *Server) error {
	ctx := context.Background()
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.writeCosts(ctx, tx, c.runs, c.evals, c.now)
	})
}

// A state change while a luxd works a Run frees its claim: that luxd's
// result, read before the change, is not written, and the Run is due again.
func TestDrainComputeStaleClaim(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateRunning, "static", workedExample[0])
	finish(t, s, "t1", "A", StateStopping)
	c := claimOne(t, s)
	finish(t, s, "t1", "A", StateSucceeded)
	if err := c.write(s); err != nil {
		t.Fatal(err)
	}
	if lines, src := computeView(t, s, keys["t1"], "A"); len(lines) != 0 || src != "" || pending(t, s, "A") != "state:succeeded -" {
		t.Fatalf("stale result written: %v %q, queued %q", lines, src, pending(t, s, "A"))
	}
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.05 USD true]" || src != "final" {
		t.Errorf("A: %v, source %q", lines, src)
	}

	// A resume while a luxd works the Run: its result, read while the Run
	// was finished, would make it final again.
	placeRun(t, s, "t1", "B", StateRunning, "static", workedExample[1])
	finish(t, s, "t1", "B", StateFailed)
	c = claimOne(t, s)
	if !c.evals["B"].final() {
		t.Fatalf("B evaluated as not final: %+v", c.evals["B"])
	}
	resume(t, s, "B")
	if err := c.write(s); err != nil {
		t.Fatal(err)
	}
	if lines, src := computeView(t, s, keys["t1"], "B"); len(lines) != 0 || src != "" || pending(t, s, "B") != "state:resuming -" {
		t.Errorf("stale result written after a resume: %v %q, queued %q", lines, src, pending(t, s, "B"))
	}
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "B"); fmt.Sprint(lines) != "[static 0.15 USD false]" || src != "ok" {
		t.Errorf("B resumed: %v, source %q", lines, src)
	}
}

// waitLocked waits until n of s's sessions wait on a lock, or until done
// is closed.
func waitLocked(s *Server, n int, done <-chan struct{}) error {
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got)
		}); err != nil {
			return err
		}
		if got >= n {
			return nil
		}
		select {
		case <-done:
			return nil
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d sessions waiting on a lock, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A state change holds its Run's row (as a runner report, a stop or the
// reaper does) while a drainer writes that Run: the drainer takes the Run
// before its queue row, as the state change does, so it waits, finds its
// claim freed, and writes nothing. Neither deadlocks.
func TestDrainComputeLockOrder(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateRunning, "static", workedExample[0])
	finish(t, s, "t1", "A", StateStopping)
	c := claimOne(t, s)
	ctx := context.Background()

	locked, wrote := make(chan struct{}), make(chan error, 1)
	change := make(chan error, 1)
	go func() {
		change <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'A' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			if err := waitLocked(s, 1, nil); err != nil { // the drainer, on A
				return err
			}
			return setRunState(ctx, tx, "t1", "A", StateSucceeded, "", 1)
		})
	}()
	<-locked
	go func() { wrote <- c.write(s) }()
	if err := <-change; err != nil {
		t.Errorf("state change: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Errorf("drainer: %v", err)
	}
	if lines, _ := computeView(t, s, keys["t1"], "A"); len(lines) != 0 || pending(t, s, "A") != "state:succeeded -" {
		t.Errorf("stale result written: %v, queued %q", lines, pending(t, s, "A"))
	}
}

// A state change holds a live Run's row while the tick runs: the tick
// skips that Run (its state change queues it) rather than wait on it while
// holding the queue row the state change needs next. Neither deadlocks.
func TestCostTickLockOrder(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Every = time.Hour
	ctx := context.Background()
	locked, ticked := make(chan struct{}), make(chan struct{})
	change := make(chan error, 1)
	go func() {
		change <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'r1' FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			// The tick, waiting on r1 or done with it.
			if err := waitLocked(s, 1, ticked); err != nil {
				return err
			}
			return setRunState(ctx, tx, "t1", "r1", StateStopping, "", 1)
		})
	}()
	<-locked
	won, err := s.costTick(ctx)
	close(ticked)
	if err != nil || !won {
		t.Errorf("tick: won %v, %v", won, err)
	}
	if err := <-change; err != nil {
		t.Errorf("state change: %v", err)
	}
	for run, want := range map[string]string{"r1": "state:stopping -", "r2": "tick -"} {
		if got := pending(t, s, run); got != want {
			t.Errorf("%s: queued %q, want %q", run, got, want)
		}
	}
}

// An uncommitted placement insert holds the host lock while a different Run's
// final evaluation waits. After commit, A's frozen share includes B.
func TestComputeHostPlacementInsertSerializes(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateRunning, "static", workedExample[0])
	execSQL(t, s, context.Background(), `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('B', 't1', '{}', 'running')`)
	finish(t, s, "t1", "A", StateSucceeded)
	c := claimOne(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	locked := make(chan struct{})
	release := make(chan struct{})
	inserted := make(chan error, 1)
	go func() {
		inserted <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'B' FOR UPDATE`); err != nil {
				return err
			}
			if err := lockCostHost(ctx, tx, "static"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
				VALUES ('race-b', 't1', 'B', 'static', 1, 'exited', '{"cpus": 8, "memory": 34359738368}', $1, $2)`, at("10:00"), at("10:30"))
			if err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	written := make(chan error, 1)
	done := make(chan struct{})
	go func() { written <- c.write(s); close(done) }()
	if err := waitLocked(s, 1, done); err != nil {
		close(release)
		<-inserted
		t.Fatal(err)
	}
	close(release)
	if err := <-inserted; err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if lines, source := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.04 USD true]" || source != "final" {
		t.Errorf("serialized final snapshot: %v, %s", lines, source)
	}
}

// Ending B's overlapping placement while A is being finalized must be
// visible to A's frozen occupancy calculation.
func TestComputeHostPlacementEndSerializes(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	placeRun(t, s, "t1", "A", StateRunning, "static", workedExample[0])
	placeRun(t, s, "t1", "B", StateRunning, "static", placementWindow{From: at("10:00"), CPUs: 8, Memory: 32 * gib})
	finish(t, s, "t1", "A", StateSucceeded)
	c := claimOne(t, s)
	locked, release := make(chan struct{}), make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'B' FOR UPDATE`); err != nil {
				return err
			}
			if err := lockCostHost(ctx, tx, "static"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE placements SET state = 'exited', ended_at = $1 WHERE id = 'p-B'`, at("10:30"))
			if err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	written := make(chan error, 1)
	done := make(chan struct{})
	go func() { written <- c.write(s); close(done) }()
	if err := waitLocked(s, 1, done); err != nil {
		close(release)
		<-ended
		t.Fatal(err)
	}
	close(release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if lines, source := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.04 USD true]" || source != "final" {
		t.Errorf("serialized end: %v, %s", lines, source)
	}
}

// A holds the Run and cost-host locks while B discovers a stale host. B
// must wait for the Run before taking the host row, so A can refresh the
// heartbeat and end the placement without a lock cycle.
func TestReapHostsRunBeforeHostRow(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "reap-race", StateRunning, "static", placementWindow{From: at("10:00"), CPUs: 2, Memory: 8 * gib})
	execSQL(t, s, context.Background(), `UPDATE hosts SET last_heartbeat = now() - interval '1 day' WHERE id = 'static'`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type heldLock struct {
		pid int
		err error
	}
	held := make(chan heldLock, 1)
	release := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = 'reap-race' FOR UPDATE`); err != nil {
				held <- heldLock{err: err}
				return err
			}
			if err := lockCostHost(ctx, tx, "static"); err != nil {
				held <- heldLock{err: err}
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				held <- heldLock{err: err}
				return err
			}
			held <- heldLock{pid: pid}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if _, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = now() WHERE id = 'static'`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE placements SET state = 'exited', ended_at = $1 WHERE id = 'p-reap-race'`, at("10:30")); err != nil {
				return err
			}
			return setRunState(ctx, tx, "t1", "reap-race", StateSucceeded, "", 1)
		})
	}()
	lock := <-held
	if lock.err != nil {
		<-aDone
		t.Fatal(lock.err)
	}
	bDone := make(chan error, 1)
	go func() { bDone <- s.reapHosts(ctx) }()

	// Observe B blocked by A, not merely scheduled but not yet running.
	waitErr := func() error {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting bool
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
					  AND $1 = ANY(pg_blocking_pids(pid)))`, lock.pid).Scan(&waiting)
			})
			if err != nil {
				return err
			}
			if waiting {
				return nil
			}
			select {
			case err := <-bDone:
				return fmt.Errorf("reaper finished before blocking: %v", err)
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}()
	close(release)
	aErr := <-aDone
	bErr := <-bDone
	if waitErr != nil || aErr != nil || bErr != nil {
		t.Fatalf("wait for reaper: %v; placement transaction: %v; reaper: %v", waitErr, aErr, bErr)
	}
	var hostState, placementState, runState string
	systemScan(t, s, `SELECT h.state, p.state, r.state FROM hosts h
		JOIN placements p ON p.host_id = h.id JOIN runs r ON r.id = p.run_id
		WHERE p.id = 'p-reap-race'`, nil, &hostState, &placementState, &runState)
	if hostState != "ready" || placementState != "exited" || runState != StateSucceeded {
		t.Fatalf("reaper discarded refreshed host or placement: host %s, placement %s, run %s", hostState, placementState, runState)
	}
	drain(t, s)
	lines, source := computeView(t, s, keys["t1"], "reap-race")
	if fmt.Sprint(lines) != "[static 0.05 USD true]" || source != "final" {
		t.Fatalf("final compute: %v, source %q", lines, source)
	}
	execSQL(t, s, context.Background(), `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('static', $1, $2, 0.80, 'USD', 8, $3, 'static')`, at("10:10"), at("10:20"), 32*gib)
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('reap-race', 'retry')`)
	drain(t, s)
	if after, src := computeView(t, s, keys["t1"], "reap-race"); fmt.Sprint(after) != fmt.Sprint(lines) || src != "final" {
		t.Errorf("final compute changed: %v, source %q (was %v)", after, src, lines)
	}
}

// The tick is tried at the start of the next bucket (not a whole
// costs.every after the last try, which drifts across buckets), and a tick
// that failed is tried again within its bucket.
func TestNextCostTick(t *testing.T) {
	every := 2 * time.Minute
	day := func(clock string) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-09-27T"+clock+"Z")
		return t
	}
	for _, now := range []string{"10:00:00", "10:00:01", "10:01:59.9"} {
		if got := nextCostTick(day(now), every); !got.Equal(day("10:02:01")) {
			t.Errorf("after %s: %s, want 10:02:01", now, got.Format("15:04:05.0"))
		}
	}
	// Buckets are counted from the Unix epoch, as cost_ticks counts them.
	if got := nextCostTick(day("10:00:00"), 7*time.Second); !got.Equal(day("10:00:04.7")) {
		t.Errorf("every 7s: %s", got.Format("15:04:05.0"))
	}

	s, _ := costFixture(t)
	s.cfg.Costs.Every = every
	last := time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.tryCostTick(ctx, last); !got.Equal(last) {
		t.Errorf("failed tick: next at %s, want %s (unchanged)", got, last)
	}
	before := nextCostTick(time.Now(), every)
	got := s.tryCostTick(context.Background(), last)
	if after := nextCostTick(time.Now(), every); !got.Equal(before) && !got.Equal(after) {
		t.Errorf("tick: next at %s, want %s", got, before)
	}
}

// ownerExec runs q on s's database as its owner (the admin LUX_TEST_PG
// user), for what lux_app may not do: triggers, grants.
func ownerExec(t *testing.T, s *Server, q string, args ...any) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(os.Getenv("LUX_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = s.db.Pool.Config().ConnConfig.Database
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}

// retried reports whether runID's queue row was given back after a failure:
// unclaimed, reason retry, due about one costs.every from now.
func retried(t *testing.T, s *Server, runID string) bool {
	t.Helper()
	ctx := context.Background()
	var ok bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT claimed_by IS NULL AND claimed_until IS NULL AND reason = 'retry'
				AND due_at BETWEEN now() + $2::interval - interval '10 seconds' AND now() + $2::interval
			FROM cost_pending WHERE run_id = $1`, runID, interval(s.cfg.Costs.Every)).Scan(&ok)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return ok
}

// One Run whose write fails fails its chunk: those Runs are given back to
// be tried a tick later, and the next chunks are still written.
func TestDrainComputeFailedChunk(t *testing.T) {
	s, keys := costFixture(t)
	s.cfg.Costs.Every = 2 * time.Minute
	costHosts(t, s)
	defer func(n int) { costChunk = n }(costChunk)
	costChunk = 2
	for _, p := range workedExample {
		placeRun(t, s, "t1", p.RunID, StateStopping, "static", p)
		finish(t, s, "t1", p.RunID, StateStopped)
	}
	ownerExec(t, s, `CREATE FUNCTION fail_a() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'no writes for A'; END $$;
		CREATE TRIGGER fail_a BEFORE INSERT OR UPDATE ON cost_sources FOR EACH ROW
			WHEN (NEW.run_id = 'A') EXECUTE FUNCTION fail_a()`)
	n, err := s.drainCosts(context.Background())
	if n != 3 || err == nil || !strings.Contains(err.Error(), "no writes for A") {
		t.Fatalf("drain: %d claimed, %v", n, err)
	}
	// A and B (its chunk) are given back; C (the next chunk) is written.
	for _, run := range []string{"A", "B"} {
		if lines, _ := computeView(t, s, keys["t1"], run); len(lines) != 0 || !retried(t, s, run) {
			t.Errorf("%s: lines %v, queued %q, want none and a retry a tick later", run, lines, pending(t, s, run))
		}
	}
	if lines, src := computeView(t, s, keys["t1"], "C"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" || pending(t, s, "C") != "" {
		t.Errorf("C: %v, source %q, queued %q", lines, src, pending(t, s, "C"))
	}
}

// A read that fails gives back every claimed Run, a tick later.
func TestDrainComputeFailedRead(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Every = 2 * time.Minute
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateStopping, "static", workedExample[0])
	finish(t, s, "t1", "A", StateStopped)
	ownerExec(t, s, `REVOKE SELECT ON placements FROM lux_app`)
	if n, err := s.drainCosts(context.Background()); n != 1 || err == nil {
		t.Fatalf("drain: %d claimed, %v", n, err)
	}
	if !retried(t, s, "A") {
		t.Errorf("A queued %q, want a retry a tick later", pending(t, s, "A"))
	}
}

// sourceRow is a Run's compute source as "status attempts next_at-set
// last_error-set" (the flags t or f), and next_at - now, read in the
// system scope (last_error is operators').
func sourceRow(t *testing.T, s *Server, runID string) (string, time.Duration) {
	t.Helper()
	var out string
	var next *time.Duration
	systemScan(t, s, `SELECT format('%s %s %s %s', status, attempts, next_at IS NOT NULL, last_error <> ''), next_at - now()
		FROM cost_sources WHERE run_id = $1 AND source = 'compute'`, []any{runID}, &out, &next)
	if next == nil {
		return out, 0
	}
	return out, *next
}

// A finalized placement keeps its price when a later overlapping rate arrives.
func TestDrainComputeFrozenHostRate(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateStopping, "static", workedExample[0])
	finish(t, s, "t1", "A", StateStopped)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" {
		t.Fatalf("A: %v, source %q", lines, src)
	}
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('static', $1, $2, 0.80, 'USD', 8, $3, 'static')`, at("10:10"), at("10:20"), 32*gib)
	finish(t, s, "t1", "A", StateStopped)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "A"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" {
		t.Errorf("frozen A: %v, source %q", lines, src)
	}
}

// Runs of two tenants drained together: each Run's lines are its own
// tenant's.
func TestDrainComputeTenants(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at) VALUES ('t2host', 't2', 't2host', 'ready', $1)`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('t2host', $1, NULL, 0.80, 'USD', 8, $2, 'static')`, at("10:00"), 32*gib)
	placeRun(t, s, "t1", "A", StateStopping, "static", workedExample[0])
	placeRun(t, s, "t2", "T", StateStopping, "t2host", place("T", 2, 8, "10:00", "10:30"))
	placeRun(t, s, "t1", "U", StateStopping, "static", workedExample[2])
	for _, r := range [][2]string{{"t1", "A"}, {"t2", "T"}, {"t1", "U"}} {
		finish(t, s, r[0], r[1], StateStopped)
	}
	if n, err := s.drainCosts(ctx); n != 3 || err != nil {
		t.Fatalf("drained %d in one batch (%v), want 3", n, err)
	}
	if lines, src := computeView(t, s, keys["t2"], "T"); fmt.Sprint(lines) != "[static 0.1 USD false]" || src != "ok" {
		t.Errorf("T, read by t2: %v, source %q", lines, src)
	}
	for run, want := range map[string]string{"A": "t1", "T": "t2", "U": "t1"} {
		var lines, sources string
		systemScan(t, s, `SELECT (SELECT string_agg(tenant_id, ',') FROM cost_lines WHERE run_id = $1),
			(SELECT string_agg(tenant_id, ',') FROM cost_sources WHERE run_id = $1)`, []any{run}, &lines, &sources)
		if lines != want || sources != want {
			t.Errorf("%s: lines of %s, source of %s; want %s", run, lines, sources, want)
		}
	}
}

// Two luxd, eight drainers each, claiming at once: no Run is claimed twice.
func TestCostClaimsConcurrent(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs.Batch = 7
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) SELECT 'c' || i, 't1', '{}', 'stopped' FROM generate_series(1, 200) i`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason) SELECT id, now(), 'tick' FROM runs WHERE id LIKE 'c%'`)
	b := peer(s, "luxd-b")
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv := s
			if i%2 == 1 {
				srv = b
			}
			for {
				runs, err := srv.claimCosts(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if len(runs) == 0 {
					return
				}
				mu.Lock()
				for _, r := range runs {
					claimed[r]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	twice := 0
	for _, n := range claimed {
		if n > 1 {
			twice++
		}
	}
	if len(claimed) != 200 || twice != 0 {
		t.Errorf("%d Runs claimed, %d of them more than once; want 200, none", len(claimed), twice)
	}
}

// A live placement: priced up to now, its "to" null, never final, not even
// once the Run is terminal while the placement is still open.
func TestDrainComputeLivePlacement(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('L', 't1', '{}', 'running')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at)
		VALUES ('p-L', 't1', 'L', 'static', 1, 'running', '{"cpus": 2, "memory": 8589934592}', now() - interval '1 hour')`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	_, c := getCost(t, s, keys["t1"], "L")
	if len(c.Lines) != 1 || c.Lines[0].Final || time.Since(c.Lines[0].To) > time.Minute || time.Since(c.Lines[0].From) < time.Hour {
		t.Fatalf("L: %+v", c.Lines)
	}
	if d := c.Lines[0].Details["placements"].([]any)[0].(map[string]any); d["to"] != nil {
		t.Errorf("L's placement ends at %v, want null", d["to"])
	}
	if c.Sources[0].Status != "ok" || c.Sources[0].NextAt != nil {
		t.Errorf("L's source: %+v", c.Sources[0])
	}

	finish(t, s, "t1", "L", StateSucceeded)
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "L"); len(lines) != 1 || lines[0][len(lines[0])-5:] != "false" || src == "final" {
		t.Errorf("L succeeded, placement open: %v, source %q; want not final", lines, src)
	}
}

// A terminal Run with an open placement retries compute with exponential
// backoff capped at an hour. Resuming clears the scheduled retry.
func TestDrainComputeBackoff(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	placeRun(t, s, "t1", "ec2", StateRunning, "ec2", placementWindow{From: time.Now().Add(-time.Hour), CPUs: 2, Memory: 8 * gib})
	finish(t, s, "t1", "ec2", StateFailed)
	drain(t, s)
	if row, next := sourceRow(t, s, "ec2"); row != "ok 1 t f" || next <= 0 {
		t.Fatalf("open placement: %s, next in %s", row, next)
	}

	execSQL(t, s, ctx, `UPDATE cost_sources SET attempts = 10 WHERE run_id = 'ec2'`)
	finish(t, s, "t1", "ec2", StateFailed)
	drain(t, s)
	if row, next := sourceRow(t, s, "ec2"); row != "ok 11 t f" || next > time.Hour || next < 59*time.Minute {
		t.Errorf("attempt 11: %s, next in %s", row, next)
	}

	execSQL(t, s, ctx, `UPDATE runs SET finished_at = now() - interval '8 days' WHERE id = 'ec2'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('ec2', 'retry')`)
	drain(t, s)
	if row, _ := sourceRow(t, s, "ec2"); row != "ok 12 t f" {
		t.Errorf("a week on: %s; want continued retry", row)
	}

	resume(t, s, "ec2")
	if row, _ := sourceRow(t, s, "ec2"); row != "ok 0 f f" {
		t.Errorf("resumed: %s; want no next attempt, attempts 0", row)
	}
	if _, src := computeView(t, s, keys["t1"], "ec2"); src != "ok" {
		t.Errorf("resumed: source %q", src)
	}
}

// A live placement refreshes its snapshot; an ended one freezes its amount
// even if rates change, while a new placement gets its own snapshot.
func TestDrainComputePlacementSnapshotsFreezeAndNewHost(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from := time.Now().UTC().Add(-2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at) VALUES
		('snap-a', 't1', 'snap-a', 'ready', $1), ('snap-b', 't1', 'snap-b', 'ready', $1)`, from)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('snap-a', $1, 0.40, 'USD', 8, $2, 'static'), ('snap-b', $1, 0.80, 'USD', 8, $2, 'static')`, from, 32*gib)
	placeRun(t, s, "t1", "snap-run", StateRunning, "snap-a", placementWindow{From: from, CPUs: 2, Memory: 8 * gib})
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('snap-run', 'tick')`)
	drain(t, s)
	var first, updated string
	var finalized bool
	systemScan(t, s, `SELECT amount::text, finalized FROM cost_placement_snapshots WHERE placement_id = 'p-snap-run'`, nil, &first, &finalized)
	if finalized {
		t.Fatal("open placement frozen")
	}
	lines, src := computeView(t, s, keys["t1"], "snap-run")
	if len(lines) != 1 || src != "ok" {
		t.Fatalf("open placement: lines %v, source %q", lines, src)
	}
	execSQL(t, s, ctx, `UPDATE host_rates SET per_hour = 0.80 WHERE host_id = 'snap-a'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('snap-run', 'tick')`)
	drain(t, s)
	systemScan(t, s, `SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-snap-run'`, nil, &updated)
	if updated == first {
		t.Fatalf("active placement did not refresh: amount %s", updated)
	}
	ended := time.Now().UTC().Add(-time.Minute)
	execSQL(t, s, ctx, `UPDATE placements SET ended_at = $1, state = 'exited' WHERE id = 'p-snap-run'`, ended)
	finish(t, s, "t1", "snap-run", StateSucceeded)
	drain(t, s)
	var frozen, rate string
	var pricedTo time.Time
	systemScan(t, s, `SELECT amount::text, per_hour::text, priced_to, finalized FROM cost_placement_snapshots WHERE placement_id = 'p-snap-run'`, nil, &frozen, &rate, &pricedTo, &finalized)
	if !finalized || !pricedTo.Equal(ended.Truncate(time.Microsecond)) || rate != "0.800000000" {
		t.Fatalf("ended snapshot: amount %s, rate %s, priced to %s, finalized %v", frozen, rate, pricedTo, finalized)
	}
	if _, src := computeView(t, s, keys["t1"], "snap-run"); src != "final" {
		t.Fatalf("ended source %q, want final", src)
	}
	execSQL(t, s, ctx, `UPDATE host_rates SET per_hour = 4 WHERE host_id = 'snap-a'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('snap-run', 'retry')`)
	drain(t, s)
	var after, afterRate string
	systemScan(t, s, `SELECT amount::text, per_hour::text FROM cost_placement_snapshots WHERE placement_id = 'p-snap-run'`, nil, &after, &afterRate)
	if after != frozen || afterRate != rate {
		t.Fatalf("late rate changed frozen snapshot: %s/%s -> %s/%s", frozen, rate, after, afterRate)
	}
	// A later placement on a different host gets its own snapshot without repricing the first.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('snap-second', 't1', 'snap-run', 'snap-b', 2, 'exited', '{"cpus":2,"memory":8589934592}', $1, $2)`, ended, time.Now().UTC())
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('snap-run', 'retry')`)
	drain(t, s)
	var second string
	systemScan(t, s, `SELECT amount::text, finalized FROM cost_placement_snapshots WHERE placement_id = 'snap-second'`, nil, &second, &finalized)
	if !finalized || second == "0.000000000" {
		t.Fatalf("second host snapshot: amount %s, finalized %v", second, finalized)
	}
	systemScan(t, s, `SELECT amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-snap-run'`, nil, &after)
	if after != frozen {
		t.Errorf("second placement changed frozen first amount %s to %s", frozen, after)
	}
	var count int
	systemScan(t, s, `SELECT count(*) FROM cost_placement_snapshots WHERE run_id = 'snap-run'`, nil, &count)
	if count != 2 {
		t.Errorf("%d snapshots, want two", count)
	}
	_, cost := getCost(t, s, keys["t1"], "snap-run")
	if len(cost.Lines) != 1 || !cost.Lines[0].Final || len(cost.Lines[0].Details["placements"].([]any)) != 2 {
		t.Errorf("two-host derived line: %+v", cost.Lines)
	}
}

// Rounded placement snapshots retain their total when split across hours,
// including when two placements share the same host-hour aggregation key.
func TestDrainComputeSnapshotHourlyRounding(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	end := from.Add(3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at)
		VALUES ('round-host', 't1', 'round-host', 'ready', $1)`, from)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('round-host', $1, 0.000000001, 'USD', 3, $2, 'static')`, from, 3*gib)
	placeRun(t, s, "t1", "round-run", StateRunning, "round-host",
		placementWindow{From: from, To: &end, CPUs: 1, Memory: gib})
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('round-second', 't1', 'round-run', 'round-host', 2, 'exited',
			'{"cpus":1,"memory":1073741824}', $1, $2)`, from, end)
	finish(t, s, "t1", "round-run", StateSucceeded)
	drain(t, s)
	var count int
	var snapshot string
	systemScan(t, s, `SELECT count(*), trim_scale(sum(amount))::text FROM cost_placement_snapshots
		WHERE run_id = 'round-run'`, nil, &count, &snapshot)
	if count != 2 || snapshot != "0.000000002" {
		t.Fatalf("%d snapshots totaling %s, want two totaling 0.000000002", count, snapshot)
	}
	lines, source := computeView(t, s, keys["t1"], "round-run")
	if fmt.Sprint(lines) != "[static 0.000000002 USD true]" || source != "final" {
		t.Errorf("lines %v, source %s", lines, source)
	}
	var total string
	systemScan(t, s, `SELECT trim_scale(sum(amount))::text FROM cost_hourly
		WHERE run_id = 'round-run' AND source = 'compute'`, nil, &total)
	if total != snapshot {
		t.Errorf("hours total %s, snapshots %s", total, snapshot)
	}
	for hour, want := range []string{"0", "0.000000002", "0"} {
		systemScan(t, s, `SELECT count(*), trim_scale(sum(amount))::text FROM cost_hourly
			WHERE run_id = 'round-run' AND source = 'compute' AND host_id = 'round-host' AND hour = $1`,
			[]any{from.Add(time.Duration(hour) * time.Hour)}, &count, &total)
		if count != 1 || total != want {
			t.Errorf("hour %d: %d rows totaling %s, want one totaling %s", hour, count, total, want)
		}
	}
}

func TestResolvePlacementRateProviderSource(t *testing.T) {
	for _, tc := range []struct {
		market, suffix string
	}{
		{"on-demand", "-pricing"},
		{"spot", "-spot-history"},
	} {
		t.Run(tc.market, func(t *testing.T) {
			s, _ := costFixture(t)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES
				('rate-pool-t1', 't1', 'burst', 'ec2'), ('rate-pool-t2', 't2', 'burst', 'ec2')`)
			execSQL(t, s, ctx, `INSERT INTO hosts
				(id, tenant_id, name, pool_id, state, provision_requested_at, instance_type, market, zone, launch_template, capacity)
				VALUES ('rate-target', 't1', 'rate-target', 'rate-pool-t1', 'ready', $1, 'm7i.large', $2,
				'us-east-1a', '{"region":"us-east-1"}', '{"cpus":4,"memory":17179869184}'),
				('rate-peer', 't2', 'rate-peer', 'rate-pool-t2', 'ready', $1, 'm7i.large', $2,
				'us-east-1a', '{"region":"us-east-1"}', '{"cpus":8,"memory":34359738368}')`, at("10:00"), tc.market)
			execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
				VALUES ('rate-peer', $1, 0.40, 'USD', 8, $3, $4),
				('rate-target', $2, 9.00, 'USD', 4, $5, $6)`, at("10:00"), at("10:30"), 32*gib, "ec2"+tc.suffix, 16*gib, "other"+tc.suffix)
			check := func(host, wantPrice, wantSource string) {
				t.Helper()
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					rate, err := resolvePlacementRate(ctx, tx, host, at("10:45"))
					if err != nil {
						return err
					}
					if rate.PerHour != wantPrice || rate.Source != wantSource || rate.CapCPUs != 4 || rate.CapMemory != 16*gib {
						t.Errorf("%s: rate %+v, want %s from %s with target capacity", host, rate, wantPrice, wantSource)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			check("rate-target", "0.400000000", "ec2"+tc.suffix)

			// Without a pool, a legacy host's own suffix-matching observation remains usable.
			execSQL(t, s, ctx, `UPDATE hosts SET pool_id = NULL WHERE id = 'rate-target'`)
			check("rate-target", "9.000000000", "other"+tc.suffix)
		})
	}
}

func TestDrainComputeSnapshotOnDemandFiltersRateSource(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from, end := at("10:00"), at("11:00")
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, registered_at, instance_type, market)
		VALUES ('snap-od', 't1', 'snap-od', 'ready', $1, $1, 'm7i.large', 'on-demand')`, from)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('snap-od', $1, $3, 0.40, 'USD', 8, $2, 'aws-pricing'),
		('snap-od', $3, NULL, 9.00, 'USD', 8, $2, 'aws-spot-history')`, from, 32*gib, from.Add(time.Minute))
	placeRun(t, s, "t1", "snap-od-run", StateRunning, "snap-od", placementWindow{From: from, To: &end, CPUs: 2, Memory: 8 * gib})
	finish(t, s, "t1", "snap-od-run", StateSucceeded)
	drain(t, s)
	lines, src := computeView(t, s, keys["t1"], "snap-od-run")
	if fmt.Sprint(lines) != "[m7i.large 0.1 USD true]" || src != "final" {
		t.Errorf("on-demand cost: %v, source %q", lines, src)
	}
	var rate, amount string
	systemScan(t, s, `SELECT per_hour::text, amount::text FROM cost_placement_snapshots WHERE placement_id = 'p-snap-od-run'`, nil, &rate, &amount)
	if rate != "0.400000000" || amount != "0.100000000" {
		t.Errorf("on-demand snapshot: rate %s, amount %s", rate, amount)
	}
}

func TestDrainComputeSnapshotMissingRateRetriesAndTenantRLS(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from, end := at("10:00"), at("11:00")
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, registered_at, instance_type, market)
		VALUES ('snap-missing', 't1', 'snap-missing', 'ready', $1, $1, 'm7i.large', 'on-demand')`, from)
	placeRun(t, s, "t1", "snap-missing-run", StateRunning, "snap-missing", placementWindow{From: from, To: &end, CPUs: 2, Memory: 8 * gib})
	finish(t, s, "t1", "snap-missing-run", StateSucceeded)
	drain(t, s)
	lines, src := computeView(t, s, keys["t1"], "snap-missing-run")
	if len(lines) != 0 || src != "incomplete" {
		t.Fatalf("missing rate: lines %v, source %q", lines, src)
	}
	if row, next := sourceRow(t, s, "snap-missing-run"); row != "incomplete 1 t t" || next <= 0 {
		t.Fatalf("missing rate retry: %s, next in %s", row, next)
	}
	var visible int
	for _, tc := range []struct {
		scope store.Scope
		want  int
	}{{store.Tenant("t1"), 0}, {store.Tenant("t2"), 0}, {store.System(), 1}} {
		if err := s.db.Tx(ctx, tc.scope, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM cost_placement_snapshots WHERE run_id = 'snap-missing-run'`).Scan(&visible)
		}); err != nil {
			t.Fatal(err)
		}
		if visible != tc.want {
			t.Errorf("scope %+v sees %d snapshots, want %d", tc.scope, visible, tc.want)
		}
	}
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('snap-missing', $1, 0.40, 'USD', 8, $2, 'aws-pricing')`, from, 32*gib)
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'snap-missing-run'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	lines, src = computeView(t, s, keys["t1"], "snap-missing-run")
	if fmt.Sprint(lines) != "[m7i.large 0.1 USD true]" || src != "final" {
		t.Errorf("recovered rate: lines %v, source %q", lines, src)
	}
	if row, _ := sourceRow(t, s, "snap-missing-run"); row != "final 0 f f" {
		t.Errorf("recovered retry: %s", row)
	}
	if code, _ := getCost(t, s, keys["t2"], "snap-missing-run"); code != http.StatusNotFound {
		t.Errorf("other tenant reads run cost: HTTP %d", code)
	}
}

// Line names: an on-demand instance is its type, a spot one type:spot, a
// static host "static"; an item priced in two currencies is one line per
// currency, suffixed. A stopped or lost Run is evaluated, never retried.
func TestDrainComputeItems(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, registered_at) VALUES ('eur', 't1', 'eur', 'ready', $1)`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, registered_at, instance_type, market, zone)
		VALUES ('od', 't1', 'od', 'ready', $1, $1, 'm7i.large', 'on-demand', 'eu-west-1b')`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('eur', $1, NULL, 0.80, 'EUR', 8, $2, 'static'), ('od', $1, NULL, 0.40, 'USD', 8, $2, 'aws-pricing')`, at("10:00"), 32*gib)
	whole := place("", 2, 8, "10:00", "11:00") // share 1/4
	placeRun(t, s, "t1", "M", StateStopping, "static", whole)
	for i, h := range []string{"eur", "od", "ec2"} {
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
			VALUES ($1, 't1', 'M', $2, $3, 'exited', '{"cpus": 2, "memory": 8589934592}', $4, $5)`,
			"p-M-"+h, h, i+2, at("10:00"), at("11:00"))
	}
	placeRun(t, s, "t1", "X", StateStopping, "static", place("", 1, 4, "10:00", "10:30"))
	finish(t, s, "t1", "M", StateStopped)
	finish(t, s, "t1", "X", StateLost)
	drain(t, s)
	want := "[m7i.2xlarge:spot 0.1 USD false m7i.large 0.1 USD false static:EUR 0.2 EUR false static:USD 0.1 USD false]"
	if lines, src := computeView(t, s, keys["t1"], "M"); fmt.Sprint(lines) != want || src != "ok" {
		t.Errorf("M: %v, source %q; want %s, ok", lines, src, want)
	}
	for _, run := range []string{"M", "X"} {
		if _, c := getCost(t, s, keys["t1"], run); len(c.Sources) != 1 || c.Sources[0].NextAt != nil {
			t.Errorf("%s (not terminal) is retried: %+v", run, c.Sources)
		}
	}
}
