package server

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

func TestPluginHourlyReplacementAndRLS(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	write := func(tenant, run string, lines ...costReport) error {
		return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := replaceCostLines(ctx, tx, tenant, run, "ledger", lines); err != nil {
				return err
			}
			return replacePluginHours(ctx, tx, tenant, run, "ledger", lines, s.cfg.Costs.Hourly)
		})
	}
	l := costReport{Family: "ai", Item: "m", Currency: "USD", Amount: "1", From: t0.Add(30 * time.Minute), To: t0.Add(90 * time.Minute)}
	if err := write("t1", "r1", l); err != nil {
		t.Fatal(err)
	}
	if err := write("t2", "r2", costReport{Family: "ai", Item: "m", Currency: "EUR", Amount: "3", From: t0, To: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		var c costSummaryBody
		if code := getJSON(t, s, keys["t1"], costPath("&interval=hour"), &c); code != 200 {
			t.Fatalf("status %d", code)
		}
		got := ""
		for _, row := range c.Series {
			got += row.Currency + ":" + row.Amount + " "
		}
		if got != want {
			t.Errorf("hours %q, want %q", got, want)
		}
	}
	check("USD:0.5 USD:0.5 ")
	l.Amount, l.Currency, l.From, l.To = "0.000000001", "EUR", t0, t0.Add(2*time.Hour)
	if err := write("t1", "r1", l); err != nil {
		t.Fatal(err)
	}
	check("EUR:0.000000001 EUR:0 ")
	if err := write("t1", "r1"); err != nil {
		t.Fatal(err)
	}
	check("")
	if err := write("t1", "r1", costReport{Family: "ai", Item: "a", Currency: "USD", Amount: "2", From: t0, To: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := write("t2", "r2", costReport{Family: "ai", Item: "b", Currency: "EUR", Amount: "4", From: t0, To: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_hourly`).Scan(&seen)
	}); err != nil || seen != 1 {
		t.Fatalf("tenant hourly rows: %d, %v", seen, err)
	}
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount)
			VALUES ($1, 't2', 'r2', 'evil', 'ai', 'USD', 1)`, t0)
		return err
	}); err == nil {
		t.Fatal("tenant wrote another tenant's hourly cost")
	}
	// Host rows cannot be read or written in a tenant transaction.
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool, state) VALUES ('h1', 'h1', 't1', 'p', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, allocated, unallocated)
		VALUES ($1, 'compute', 'compute', 'USD', 'h1', 1, 2)`, t0)
	var n int
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_hourly`).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Fatalf("tenant saw host row: count %d, err %v", n, err)
	}
	err = s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id) VALUES ($1, 'compute', 'compute', 'USD', 'h1')`, t0.Add(time.Hour))
		return err
	})
	if err == nil {
		t.Fatal("tenant inserted host-only cost")
	}
}

func TestComputeHourlyPiecesAndHostIdle(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool, state, registered_at, capacity)
		VALUES ('h1','h1','t1','p','ready',$1,'{"cpus":4,"memory":400}')`, t0)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h1',$1,4,'USD',4,400,'static')`, t0)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('p1','t1','r1','h1',1,'exited','{"cpus":2,"memory":200}',$1,$2)`, t0.Add(30*time.Minute), t0.Add(90*time.Minute))
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		in, err := loadHostCompute(ctx, tx, "h1", t0, t0.Add(2*time.Hour))
		if err != nil {
			return err
		}
		res, err := computeCost(in)
		if err != nil {
			return err
		}
		var hours []computeHour
		for _, piece := range res.Pieces {
			if piece.Rate == nil {
				continue
			}
			forEachCostHour(piece.From, piece.To, func(hour time.Time, fraction *big.Rat) {
				for _, v := range piece.Charged {
					hours = append(hours, computeHour{hour, "h1", "USD", new(big.Rat).Mul(v, fraction)})
				}
			})
		}
		if err := replaceComputeHours(ctx, tx, "t1", "r1", hours, s.cfg.Costs.Hourly); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got costSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&interval=hour"), &got); code != 200 || len(got.Series) != 2 || got.Series[0].Amount != "1" || got.Series[1].Amount != "1" {
		t.Fatalf("compute hours: %d %+v", code, got)
	}
}

func TestHourlyCostSurvivesResumeAndRetention(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Hourly = time.Hour
	now := time.Now().UTC()
	recent := now.Truncate(time.Hour)
	old := recent.Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount)
		VALUES ($1,'t1','r1','plugin','ai','USD',2), ($2,'t1','r1','plugin','ai','USD',3)`, old, recent)
	if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return resetCostFinality(ctx, tx, "r1")
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE hour < now() - $1::interval`, interval(s.cfg.Costs.Hourly))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		lines := []costReport{
			{Family: "ai", Item: "old", Currency: "USD", Amount: "2", From: old, To: old.Add(time.Hour)},
			{Family: "ai", Item: "recent", Currency: "USD", Amount: "3", From: recent, To: recent.Add(time.Hour)},
		}
		if err := replacePluginHours(ctx, tx, "t1", "r1", "plugin", lines, s.cfg.Costs.Hourly); err != nil {
			return err
		}
		return replaceComputeHours(ctx, tx, "t1", "r1", []computeHour{{Hour: old, Host: "", Currency: "USD", Amount: big.NewRat(2, 1)}}, s.cfg.Costs.Hourly)
	}); err != nil {
		t.Fatal(err)
	}
	var expired int
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE hour = $1`, []any{old}, &expired)
	if expired != 0 {
		t.Fatalf("replacement reinserted %d expired hours", expired)
	}
	var out costSummaryBody
	path := "/v1/costs?from=" + old.Add(-time.Hour).Format(time.RFC3339) + "&to=" + recent.Add(time.Hour).Format(time.RFC3339)
	if code := getJSON(t, s, keys["t1"], path, &out); code != 200 || len(out.Totals) != 1 || out.Totals[0].Amount != "3" {
		t.Errorf("retained after resume: %d %+v", code, out)
	}
}

func TestHostHoursBackfillBoundedAndIsolated(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 2
	s.cfg.Costs.Hourly = 48 * time.Hour
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-3 * time.Hour)
	stop := now.Add(-90 * time.Minute)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at, terminated_at) VALUES
		('a-bad','bad','p','terminated',$1,$2), ('b-good','good','p','terminated',$1,$2)`, start, stop)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('a-bad',$1,$2,1,'USD',1,1,'static'), ('a-bad',$1 + interval '30 minutes',NULL,2,'USD',1,1,'static'),
		('b-good',$1,NULL,4,'USD',1,1,'static')`, start, start.Add(time.Hour))
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'r1'`)
	if won, err := s.costTick(ctx); err != nil || !won {
		t.Fatalf("malformed host interrupted tick: won %v, %v", won, err)
	}
	if reason := pending(t, s, "r1"); !strings.HasPrefix(reason, "tick ") {
		t.Fatalf("healthy run not queued: %q", reason)
	}
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'b-good' AND run_id IS NULL`, nil, &n)
	if n != 1 {
		t.Fatalf("first bounded pass wrote %d good hours, want 1", n)
	}
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'b-good' AND run_id IS NULL`, nil, &n)
	if n != 2 {
		t.Fatalf("second pass wrote %d good hours, want 2", n)
	}
	var allocated, idle string
	systemScan(t, s, `SELECT trim_scale(sum(allocated))::text, trim_scale(sum(unallocated))::text
		FROM cost_hourly WHERE host_id = 'b-good' AND run_id IS NULL`, nil, &allocated, &idle)
	if allocated != "0" || idle != "6" {
		t.Errorf("terminated host cost: allocated %s idle %s, want 0 and 6", allocated, idle)
	}
	if err := peer(s, "restart").updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'b-good' AND run_id IS NULL`, nil, &n)
	if n != 2 {
		t.Errorf("restart wrote %d rows, want 2", n)
	}
}

func TestHostHoursBackfillRespectsRetention(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 3 * time.Hour
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-12 * time.Hour)
	stop := time.Now().UTC().Add(-15 * time.Minute)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at, terminated_at)
		VALUES ('old-host','old-host','p','terminated',$1,$2)`, start, stop)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('old-host',$1,2,'USD',1,1,'static')`, start)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	var n int
	systemScan(t, s, `SELECT min(hour), count(*) FROM cost_hourly WHERE host_id = 'old-host' AND run_id IS NULL`, nil, &first, &n)
	if first.Before(now.Add(-3*time.Hour)) || n < 1 || n > 3 {
		t.Errorf("retained host hours start %s, count %d, want at most three recent hours", first, n)
	}
}

func TestHostHoursRefreshesCurrentHourWithBacklog(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 2
	s.cfg.Costs.Hourly = 48 * time.Hour
	current := time.Now().UTC().Truncate(time.Hour)
	old := current.Add(-5 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at) VALUES
		('a-old', 'a-old', 'p', 'ready', $1), ('b-old', 'b-old', 'p', 'ready', $1),
		('c-old', 'c-old', 'p', 'ready', $1), ('z-current', 'z-current', 'p', 'ready', $2)`, old, current)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('a-old', $1, 4, 'USD', 1, 1, 'static'), ('b-old', $1, 4, 'USD', 1, 1, 'static'),
		('c-old', $1, 4, 'USD', 1, 1, 'static'), ('z-current', $2, 4, 'USD', 1, 1, 'static')`, old, current)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool, allocated, unallocated)
		VALUES ($1, 'compute', 'compute', 'USD', 'z-current', 'p', 0, 0)`, current)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var amount string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'z-current' AND hour = $1 AND run_id IS NULL`, []any{current}, &amount)
	if amount == "0" {
		t.Error("current host hour stayed stale behind older host-hour backlog")
	}
	var refreshed int
	systemScan(t, s, `SELECT count(*) FROM cost_host_refresh WHERE host_id IN ('a-old', 'b-old', 'c-old')`, nil, &refreshed)
	if refreshed > s.cfg.Costs.Batch-1 {
		t.Errorf("advanced %d old host cursors, leaving no slot for current host in a batch of %d", refreshed, s.cfg.Costs.Batch)
	}
}

func TestHostHoursBatchOneAlternatesAcrossRestarts(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	current := time.Now().UTC().Truncate(time.Hour)
	old := current.Add(-4 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at) VALUES
		('a-old', 'a-old', 'p', 'ready', $1), ('b-old', 'b-old', 'p', 'ready', $1),
		('z-current', 'z-current', 'p', 'ready', $2)`, old, current)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source) VALUES
		('a-old', $1, 4, 'USD', 1, 1, 'static'), ('b-old', $1, 4, 'USD', 1, 1, 'static'),
		('z-current', $2, 4, 'USD', 1, 1, 'static')`, old, current)
	for pass := 0; pass < 4; pass++ {
		// Keep the current host eligible even after a successful refresh.
		execSQL(t, s, ctx, `UPDATE cost_host_refresh SET retry_at = NULL WHERE host_id = 'z-current'`)
		if err := peer(s, fmt.Sprintf("host-restart-%d", pass)).updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
		var oldCount int
		systemScan(t, s, `SELECT count(*) FROM cost_host_refresh WHERE host_id IN ('a-old', 'b-old')`, nil, &oldCount)
		if want := (pass + 1) / 2; oldCount != want {
			t.Fatalf("pass %d: %d backlog hosts progressed, want %d", pass, oldCount, want)
		}
		var currentAmount string
		systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
			WHERE host_id = 'z-current' AND hour = $1 AND run_id IS NULL`, []any{current}, &currentAmount)
		if currentAmount == "0" {
			t.Fatal("current host did not progress")
		}
	}
}
