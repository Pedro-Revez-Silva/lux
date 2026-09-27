package server

import (
	"cmp"
	"context"
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
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), bool_and(due_at < now() - interval '59 minutes') FROM cost_pending`).Scan(&n, &early)
	}); err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_ticks`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
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
		('done', 't1', '{}', 'succeeded'), ('stopping', 't2', '{}', 'stopping')`)
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
		"stopped": "", "later": "", "done": ""} {
		if got := pending(t, s, run); got != want {
			t.Errorf("%s: queued %q, want %q", run, got, want)
		}
	}
	var early bool
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT due_at < now() - interval '59 minutes' FROM cost_pending WHERE run_id = 'r2'`).Scan(&early)
	}); err != nil || !early {
		t.Errorf("r2's earlier due_at kept: %v (%v)", early, err)
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
		d["ratePerHour"] != "0.4" || d["share"] != 0.5 || d["cpus"] != 1.0 || d["epoch"] != 1.0 || c.Lines[0].Details["missingRate"] != nil {
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
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return s.requestResume(ctx, tx, "t1", "C", nil, "resume")
	}); err != nil {
		t.Fatal(err)
	}
	if lines, src := computeView(t, s, keys["t1"], "C"); fmt.Sprint(lines) != "[static 0.05 USD false]" || src != "ok" {
		t.Errorf("C resumed: %v, source %q", lines, src)
	}
}

// A static host's time before it had a price is unbilled: no line for it,
// and it keeps nothing from being final. A provider's host with no rate
// for part of a placement is missing: the line says so, and the Run is not
// final.
func TestDrainComputeUnpricedAndMissing(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	whole := place("", 2, 8, "10:00", "11:00") // share 1/4
	placeRun(t, s, "t1", "unpriced", StateRunning, "unpriced", whole)
	placeRun(t, s, "t1", "late", StateRunning, "late", whole)
	placeRun(t, s, "t1", "ec2", StateRunning, "ec2", whole)
	for _, id := range []string{"unpriced", "late", "ec2"} {
		finish(t, s, "t1", id, StateSucceeded)
	}
	drain(t, s)
	for _, c := range []struct {
		run, lines, source string
	}{
		{"unpriced", "[]", "final"},
		{"late", "[static 0.05 USD true]", "final"},
		// 40 of its 60 minutes priced: $0.40/h × 1/4 × 2/3.
		{"ec2", "[m7i.2xlarge:spot 0.066666667 USD false]", "incomplete"},
	} {
		lines, src := computeView(t, s, keys["t1"], c.run)
		if fmt.Sprint(lines) != c.lines || src != c.source {
			t.Errorf("%s: %v, source %q; want %s, %s", c.run, lines, src, c.lines, c.source)
		}
	}
	_, c := getCost(t, s, keys["t1"], "ec2")
	d := c.Lines[0].Details["placements"].([]any)[0].(map[string]any)
	if c.Lines[0].Details["missingRate"] != true || d["missingRate"] != true || d["market"] != "spot" || d["zone"] != "eu-west-1a" {
		t.Errorf("ec2's details: %v", c.Lines[0].Details)
	}
	if c.Status != "incomplete" || len(c.Sources) != 1 || c.Sources[0].NextAt == nil {
		t.Errorf("ec2: status %s, sources %+v", c.Status, c.Sources)
	}

	// The retry comes with a tick once next_at has passed; still missing,
	// it backs off further.
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'ec2'`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	if p := pending(t, s, "ec2"); p != "tick -" {
		t.Fatalf("ec2 queued %q", p)
	}
	drain(t, s)
	var attempts int
	var next time.Duration
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT attempts, next_at - now() FROM cost_sources WHERE run_id = 'ec2'`).Scan(&attempts, &next)
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || next < 3*time.Minute || next > 4*time.Minute {
		t.Errorf("ec2 after its retry: attempt %d, next in %s", attempts, next)
	}

	// The gap filled: final.
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('ec2', $1, $2, 0.40, 'USD', 8, $3, 'aws-spot-history')`, at("10:20"), at("10:40"), 32*gib)
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'ec2'`)
	execSQL(t, s, ctx, `DELETE FROM cost_ticks`)
	if _, err := s.costTick(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if lines, src := computeView(t, s, keys["t1"], "ec2"); fmt.Sprint(lines) != "[m7i.2xlarge:spot 0.1 USD true]" || src != "final" {
		t.Errorf("ec2 gap filled: %v, source %q", lines, src)
	}
}

// A state change while a luxd works a Run frees its claim: that luxd's
// result, read before the change, is not written, and the Run is due again.
func TestDrainComputeStaleClaim(t *testing.T) {
	s, keys := costFixture(t)
	costHosts(t, s)
	placeRun(t, s, "t1", "A", StateRunning, "static", workedExample[0])
	finish(t, s, "t1", "A", StateStopping)
	ctx := context.Background()
	runs, err := s.claimCosts(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("claimed %v (%v)", runs, err)
	}
	var evals map[string]*computeEval
	var now time.Time
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		evals, now, err = evaluateCompute(ctx, tx, runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "t1", "A", StateSucceeded)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.writeCosts(ctx, tx, runs, evals, now)
	}); err != nil {
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
	if runs, err = s.claimCosts(ctx); err != nil || len(runs) != 1 {
		t.Fatalf("claimed %v (%v)", runs, err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		evals, now, err = evaluateCompute(ctx, tx, runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !evals["B"].final() {
		t.Fatalf("B evaluated as not final: %+v", evals["B"])
	}
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return s.requestResume(ctx, tx, "t1", "B", nil, "resume")
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.writeCosts(ctx, tx, runs, evals, now)
	}); err != nil {
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
	ctx := context.Background()
	runs, err := s.claimCosts(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("claimed %v (%v)", runs, err)
	}
	var evals map[string]*computeEval
	var now time.Time
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		evals, now, err = evaluateCompute(ctx, tx, runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}

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
	go func() {
		wrote <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.writeCosts(ctx, tx, runs, evals, now)
		})
	}()
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
