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

func TestAllocateCostHoursNonnegativeAndConserved(t *testing.T) {
	from := t0
	to := from.Add(4 * time.Hour)
	amount := mustRat("0.000000002")
	var parts []string
	sum := new(big.Rat)
	allocateCostHours(from, to, amount, func(_ time.Time, part *big.Rat) {
		if part.Sign() < 0 {
			t.Fatalf("negative hourly allocation: %s", part)
		}
		parts = append(parts, moneyString(part))
		sum.Add(sum, part)
	})
	if len(parts) != 4 || strings.Join(parts, ",") != "0.000000001,0,0.000000001,0" || moneyString(sum) != moneyString(amount) {
		t.Fatalf("hourly allocations %v sum to %s, want %s", parts, sum, amount)
	}
}

func TestHostHourOnceRoundedAndMissingRateRetry(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at, terminated_at, capacity)
		VALUES ('h-round','h-round','p','terminated',$1,$2,'{"cpus":2,"memory":2}')`, hour, hour.Add(time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-round',$1,$2,0.000000001,'USD',2,2,'static')`, hour, hour.Add(30*time.Minute))
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-round'`, nil, &next)
	if !next.Equal(hour.Add(time.Hour)) {
		t.Fatalf("missing rate cursor %s, want next hour", next)
	}
	var pendingRows int
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'h-round' AND run_id IS NULL`, nil, &pendingRows)
	if pendingRows != 1 {
		t.Fatalf("missing-rate hour wrote %d priced partial rows, want 1", pendingRows)
	}
	var pendingGaps int
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-round' AND reason = 'rate_pending'`, nil, &pendingGaps)
	if pendingGaps != 1 {
		t.Fatalf("missing-rate hour has %d pending intervals, want 1", pendingGaps)
	}
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-round',$1,$2,0.000000001,'USD',2,2,'static')`, hour.Add(30*time.Minute), hour.Add(time.Hour))
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
		VALUES ('p-round','t1','r1','h-round',1,'exited','{"cpus":1,"memory":1}',$1,$2)`, hour, hour.Add(time.Hour))
	execSQL(t, s, ctx, `UPDATE cost_host_hour_gaps SET retry_at = now() - interval '1 second' WHERE host_id = 'h-round'`)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var allocated, idle, billed string
	systemScan(t, s, `SELECT trim_scale(allocated)::text, trim_scale(unallocated)::text,
		trim_scale(allocated + unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-round' AND run_id IS NULL`, nil, &allocated, &idle, &billed)
	if allocated != "0.000000001" || idle != "0" || billed != "0.000000001" {
		t.Fatalf("once-rounded host hour: allocated %s idle %s total %s", allocated, idle, billed)
	}
}

func TestHostHourRefreshWaitsForPlacementEnd(t *testing.T) {
	s, _ := costFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at, terminated_at)
		VALUES ('h-end','h-end','p','terminated',$1,$2)`, hour, hour.Add(time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-end',$1,4,'USD',4,400,'static')`, hour)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at)
		VALUES ('p-end','t1','r1','h-end',1,'running','{"cpus":2,"memory":200}',$1)`, hour)

	locked, release := make(chan struct{}), make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := lockCostHost(ctx, tx, "h-end"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE placements SET state = 'exited', ended_at = $1 WHERE id = 'p-end'`, hour.Add(30*time.Minute)); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-ended:
		t.Fatalf("placement end before refresh: %v", err)
	}
	refreshed, done := make(chan error, 1), make(chan struct{})
	go func() {
		refreshed <- s.updateHostHours(ctx)
		close(done)
	}()
	waitErr := waitLocked(s, 1, done)
	blocked := true
	select {
	case <-done:
		blocked = false
	default:
	}
	close(release)
	endErr := <-ended
	refreshErr := <-refreshed
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if !blocked {
		t.Fatal("host-hour refresh completed before the placement end committed")
	}
	if endErr != nil || refreshErr != nil {
		t.Fatalf("placement end: %v; host-hour refresh: %v", endErr, refreshErr)
	}
	var allocated, idle string
	systemScan(t, s, `SELECT trim_scale(allocated)::text, trim_scale(unallocated)::text
		FROM cost_hourly WHERE host_id = 'h-end' AND hour = $1 AND run_id IS NULL AND currency = 'USD'`,
		[]any{hour}, &allocated, &idle)
	if allocated != "1" || idle != "3" {
		t.Fatalf("committed half-hour placement: allocated %s idle %s, want 1 and 3", allocated, idle)
	}
}

func TestHostHoursSkipUnpricedStaticHourAndPriceLaterHour(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	priceAt := hour.Add(time.Hour)
	end := priceAt.Add(time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, registered_at, terminated_at)
		VALUES ('h-static-gap', 'h-static-gap', 'p', 'terminated', $1, $2)`, hour, end)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-static-gap', $1, $2, 6, 'USD', 1, 1, 'static')`, priceAt, end)
	for i := 0; i < 2; i++ {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-static-gap'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("static gap blocked later priced hour: cursor %s, want %s", next, end)
	}
	var count int
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'h-static-gap' AND hour = $1 AND run_id IS NULL`, []any{hour}, &count)
	if count != 0 {
		t.Fatalf("unpriced static hour wrote %d rows", count)
	}
	var missingFrom, missingTo time.Time
	var reason, status string
	systemScan(t, s, `SELECT missing_from, missing_to, reason, status FROM cost_host_hour_gaps
		WHERE host_id = 'h-static-gap' AND hour = $1`, []any{hour}, &missingFrom, &missingTo, &reason, &status)
	if !missingFrom.Equal(hour) || !missingTo.Equal(priceAt) || reason != "static_unpriced" || status != "incomplete" {
		t.Fatalf("static incomplete interval %s to %s: %s, %s", missingFrom, missingTo, reason, status)
	}
	var idle string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-static-gap' AND hour = $1 AND run_id IS NULL`, []any{priceAt}, &idle)
	if idle != "6" {
		t.Fatalf("later static hour idle %s, want 6", idle)
	}
}

func TestHostHoursSkipTerminatedProviderProvisioningGap(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	registered := hour.Add(30 * time.Minute)
	end := hour.Add(2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, provision_requested_at, registered_at, terminated_at)
		VALUES ('h-provider-gap', 'h-provider-gap', 'p', 'terminated', $1, $2, $3)`, hour, registered, end)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-provider-gap', $1, $2, 8, 'USD', 1, 1, 'aws-pricing')`, registered, end)
	for i := 0; i < 2; i++ {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-provider-gap'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("provisioning gap blocked later priced hour: cursor %s, want %s", next, end)
	}
	var partial, later string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-provider-gap' AND hour = $1 AND run_id IS NULL`, []any{hour}, &partial)
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-provider-gap' AND hour = $1 AND run_id IS NULL`, []any{hour.Add(time.Hour)}, &later)
	if partial != "4" || later != "8" {
		t.Fatalf("provider hours idle %s, %s; want 4, 8", partial, later)
	}
	var missingFrom, missingTo time.Time
	var reason, status string
	systemScan(t, s, `SELECT missing_from, missing_to, reason, status FROM cost_host_hour_gaps
		WHERE host_id = 'h-provider-gap' AND hour = $1`, []any{hour}, &missingFrom, &missingTo, &reason, &status)
	if !missingFrom.Equal(hour) || !missingTo.Equal(registered) || reason != "provider_pre_registration" || status != "incomplete" {
		t.Fatalf("provider incomplete interval %s to %s: %s, %s", missingFrom, missingTo, reason, status)
	}
}

func TestHostHoursRetryProviderGapAfterRegistration(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	end := hour.Add(2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, provision_requested_at, registered_at, terminated_at)
		VALUES ('h-provider-retry', 'h-provider-retry', 'p', 'terminated', $1, $1, $2)`, hour, end)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-provider-retry', $1, $2, 8, 'USD', 1, 1, 'aws-pricing')`, hour.Add(time.Hour), end)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-provider-retry'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("recoverable provider gap blocked cursor at %s", next)
	}
	var gaps int
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-provider-retry' AND reason = 'rate_pending'`, nil, &gaps)
	if gaps != 1 {
		t.Fatalf("recoverable provider gap recorded %d pending intervals, want 1", gaps)
	}
	var later string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-provider-retry' AND hour = $1 AND run_id IS NULL`, []any{hour.Add(time.Hour)}, &later)
	if later != "8" {
		t.Fatalf("later priced hour stayed behind pending gap: %s, want 8", later)
	}
	var missingFrom, missingTo time.Time
	var status string
	var retryAt time.Time
	systemScan(t, s, `SELECT missing_from, missing_to, status, retry_at FROM cost_host_hour_gaps
		WHERE host_id = 'h-provider-retry' AND hour = $1`, []any{hour}, &missingFrom, &missingTo, &status, &retryAt)
	if !missingFrom.Equal(hour) || !missingTo.Equal(hour.Add(time.Hour)) || status != "incomplete" || !retryAt.After(time.Now()) {
		t.Fatalf("pending gap %s to %s: %s, retry %s", missingFrom, missingTo, status, retryAt)
	}
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-provider-retry', $1, $2, 4, 'USD', 1, 1, 'aws-pricing')`, hour, hour.Add(time.Hour))
	execSQL(t, s, ctx, `UPDATE cost_host_refresh SET retry_at = NULL WHERE host_id = 'h-provider-retry'`)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE cost_host_hour_gaps SET retry_at = now() - interval '1 second' WHERE host_id = 'h-provider-retry'`)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-provider-retry'`, nil, &gaps)
	if gaps != 0 {
		t.Fatalf("recovered provider hour retained %d missing intervals", gaps)
	}
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-provider-retry'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("recovered provider gap blocked later hour: cursor %s, want %s", next, end)
	}
	var total string
	systemScan(t, s, `SELECT trim_scale(sum(unallocated))::text FROM cost_hourly
		WHERE host_id = 'h-provider-retry' AND run_id IS NULL`, nil, &total)
	if total != "12" {
		t.Fatalf("recovered provider hours total %s, want 12", total)
	}
}

func TestHostHoursRetryPartiallyPricedProviderAfterRegistration(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	registered := hour.Add(15 * time.Minute)
	priceAt := hour.Add(30 * time.Minute)
	end := hour.Add(2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, provision_requested_at, registered_at, terminated_at)
		VALUES ('h-provider-partial', 'h-provider-partial', 'p', 'terminated', $1, $2, $3)`, hour, registered, end)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-provider-partial', $1, $2, 8, 'USD', 1, 1, 'aws-pricing')`, priceAt, end)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-provider-partial'`, nil, &next)
	if !next.Equal(hour.Add(time.Hour)) {
		t.Fatalf("partially priced provider gap blocked cursor at %s", next)
	}
	var gaps, rows int
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-provider-partial' AND reason = 'rate_pending'`, nil, &gaps)
	systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE host_id = 'h-provider-partial' AND run_id IS NULL`, nil, &rows)
	if gaps != 1 || rows != 1 {
		t.Fatalf("incomplete hour wrote %d pending gaps and %d priced rows", gaps, rows)
	}
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-provider-partial', $1, $2, 4, 'USD', 1, 1, 'aws-pricing')`, registered, priceAt)
	execSQL(t, s, ctx, `UPDATE cost_host_refresh SET retry_at = NULL WHERE host_id = 'h-provider-partial'`)
	for i := 0; i < 2; i++ {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, s, ctx, `UPDATE cost_host_hour_gaps SET retry_at = now() - interval '1 second'
		WHERE host_id = 'h-provider-partial' AND reason = 'rate_pending'`)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-provider-partial'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("recovered partial hour blocked later priced hour: cursor %s", next)
	}
	var missingFrom, missingTo time.Time
	var reason string
	systemScan(t, s, `SELECT missing_from, missing_to, reason FROM cost_host_hour_gaps
		WHERE host_id = 'h-provider-partial' AND hour = $1`, []any{hour}, &missingFrom, &missingTo, &reason)
	if !missingFrom.Equal(hour) || !missingTo.Equal(registered) || reason != "provider_pre_registration" {
		t.Fatalf("recovered hour incomplete interval %s to %s: %s", missingFrom, missingTo, reason)
	}
	var first, later string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-provider-partial' AND hour = $1 AND run_id IS NULL`, []any{hour}, &first)
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-provider-partial' AND hour = $1 AND run_id IS NULL`, []any{hour.Add(time.Hour)}, &later)
	if first != "5" || later != "8" {
		t.Fatalf("recovered priced hours %s and %s, want 5 and 8", first, later)
	}
}

func TestHostHoursPendingRetryDoesNotStarveForwardCursor(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	end := hour.Add(2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, provision_requested_at, registered_at, terminated_at)
		VALUES ('h-pending-forward', 'h-pending-forward', 'p', 'terminated', $1, $1, $2)`, hour, end)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h-pending-forward', $1, $2, 8, 'USD', 1, 1, 'aws-pricing')`, hour.Add(time.Hour), end)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE cost_host_hour_gaps SET retry_at = now() - interval '1 second'
		WHERE host_id = 'h-pending-forward' AND reason = 'rate_pending'`)
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	systemScan(t, s, `SELECT next_hour FROM cost_host_refresh WHERE host_id = 'h-pending-forward'`, nil, &next)
	if !next.Equal(end) {
		t.Fatalf("due pending hour starved later priced hour: cursor %s", next)
	}
	var idle string
	systemScan(t, s, `SELECT trim_scale(unallocated)::text FROM cost_hourly
		WHERE host_id = 'h-pending-forward' AND hour = $1 AND run_id IS NULL`, []any{hour.Add(time.Hour)}, &idle)
	if idle != "8" {
		t.Fatalf("later priced hour idle %s, want 8", idle)
	}
}

func TestHostHoursPendingGapExpiresWithRetention(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 1
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, provision_requested_at, registered_at, terminated_at)
		VALUES ('h-expiring-gap', 'h-expiring-gap', 'p', 'terminated', $1, $1, $2)`, hour, hour.Add(time.Hour))
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-expiring-gap' AND reason = 'rate_pending'`, nil, &count)
	if count != 1 {
		t.Fatalf("pending intervals before retention: %d", count)
	}
	s.cfg.Costs.Hourly = time.Hour
	if err := s.updateHostHours(ctx); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT count(*) FROM cost_host_hour_gaps WHERE host_id = 'h-expiring-gap'`, nil, &count)
	if count != 0 {
		t.Fatalf("expired pending intervals: %d", count)
	}
}

func TestPluginHourlyCumulativeRoundingInPostgres(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	from := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	line := costReport{Family: "ai", Item: "m", Currency: "USD", Amount: "0.000000002", From: from, To: from.Add(4 * time.Hour)}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return replacePluginHours(ctx, tx, "t1", "r1", "plugin", []costReport{line}, s.cfg.Costs.Hourly)
	}); err != nil {
		t.Fatal(err)
	}
	var amounts []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT trim_scale(amount)::text FROM cost_hourly
			WHERE run_id = 'r1' AND source = 'plugin' ORDER BY hour`)
		if err != nil {
			return err
		}
		amounts, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(amounts, ","); got != "0.000000001,0,0.000000001,0" {
		t.Fatalf("stored hourly amounts: %s", got)
	}
}

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
