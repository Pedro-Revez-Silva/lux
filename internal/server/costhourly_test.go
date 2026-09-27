package server

import (
	"context"
	"math/big"
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
			return replacePluginHours(ctx, tx, tenant, run, "ledger", lines)
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
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_hourly`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
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
		if err := replaceComputeHours(ctx, tx, "t1", "r1", hours); err != nil {
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
	var out costSummaryBody
	path := "/v1/costs?from=" + old.Add(-time.Hour).Format(time.RFC3339) + "&to=" + recent.Add(time.Hour).Format(time.RFC3339)
	if code := getJSON(t, s, keys["t1"], path, &out); code != 200 || len(out.Totals) != 1 || out.Totals[0].Amount != "3" {
		t.Errorf("retained after resume: %d %+v", code, out)
	}
}
