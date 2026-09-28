package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type fakePriceProvider struct {
	onDemand []fakeOnDemandAnswer
	spot     [][]SpotRate
	spotErr  []error

	onDemandCalls int
	spotCalls     []spotPriceCall
}

type fakeOnDemandAnswer struct {
	rate HourlyRate
	err  error
}

type spotPriceCall struct {
	zone, kind string
	from, to   time.Time
}

func (p *fakePriceProvider) OnDemand(_ context.Context, _, _ string) (HourlyRate, error) {
	i := p.onDemandCalls
	p.onDemandCalls++
	if i >= len(p.onDemand) {
		return HourlyRate{}, errors.New("unexpected on-demand price request")
	}
	return p.onDemand[i].rate, p.onDemand[i].err
}

func (p *fakePriceProvider) SpotHistory(_ context.Context, zone, kind string, from, to time.Time) ([]SpotRate, error) {
	i := len(p.spotCalls)
	p.spotCalls = append(p.spotCalls, spotPriceCall{zone: zone, kind: kind, from: from, to: to})
	if i >= len(p.spot) {
		return nil, errors.New("unexpected spot price request")
	}
	if i < len(p.spotErr) && p.spotErr[i] != nil {
		return nil, p.spotErr[i]
	}
	return p.spot[i], nil
}

func providerPriceServer(t *testing.T, p PriceProvider) *Server {
	t.Helper()
	s := testServer(t)
	s.cfg.Costs = CostsConfig{
		Enabled:       true,
		ComputeEC2:    true,
		PricesRefresh: time.Hour,
		Prices:        map[string]PriceProvider{"ec2": p},
	}
	return s
}

func insertProviderHost(t *testing.T, s *Server, tenant, pool, provider, id, market string, from time.Time) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider)
		VALUES ($1, $2, $3, $4)`, "pool-"+tenant+"-"+provider+"-"+pool, tenant, pool, provider)
	execSQL(t, s, ctx, `INSERT INTO hosts
		(id, tenant_id, name, pool, state, provider_id, provision_requested_at, registered_at,
		 instance_type, market, zone, launch_template, capacity)
		VALUES ($1, $2, $1, $3, 'ready', 'i-' || $1::text, $4, $4, 'm7i.large', $5, 'us-east-1a',
		        '{"region":"us-east-1"}', jsonb_build_object('cpus', 4, 'memory', $6::int8))`,
		id, tenant, pool, from, market, int64(16)<<30)
}

type storedProviderRate struct {
	from, to        time.Time
	open            bool
	perHour, source string
}

func providerRates(t *testing.T, s *Server, host string) []storedProviderRate {
	t.Helper()
	ctx := context.Background()
	var out []storedProviderRate
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT valid_from, coalesce(valid_to, valid_from), valid_to IS NULL,
			trim_scale(per_hour)::text, source
			FROM host_rates WHERE host_id = $1 ORDER BY valid_from`, host)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rate storedProviderRate
			if err := rows.Scan(&rate.from, &rate.to, &rate.open, &rate.perHour, &rate.source); err != nil {
				return err
			}
			out = append(out, rate)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func cachedProviderRate(t *testing.T, s *Server) string {
	t.Helper()
	var price string
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT trim_scale(per_hour)::text FROM price_cache
			WHERE provider = 'ec2' AND region = 'us-east-1' AND instance_type = 'm7i.large' AND os = 'Linux'`).Scan(&price)
	}); err != nil {
		t.Fatal(err)
	}
	return price
}

func TestProviderPricesOnDemandCacheRefreshAndRetry(t *testing.T) {
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{
		{rate: HourlyRate{PerHour: "0.10", Currency: "USD"}},
		{err: errors.New("pricing temporarily unavailable")},
		{rate: HourlyRate{PerHour: "0.20", Currency: "USD"}},
	}}
	s := providerPriceServer(t, p)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "od", MarketOnDemand, from)

	s.refreshPrices(context.Background())
	if p.onDemandCalls != 1 || cachedProviderRate(t, s) != "0.1" {
		t.Fatalf("first refresh: calls=%d cached=%q", p.onDemandCalls, cachedProviderRate(t, s))
	}
	first := providerRates(t, s, "od")
	if len(first) != 1 || first[0].perHour != "0.1" || !first[0].open || !first[0].from.After(from) {
		t.Fatalf("first on-demand period: %+v", first)
	}

	s.refreshPrices(context.Background())
	if p.onDemandCalls != 1 {
		t.Fatalf("fresh cached refresh made %d provider calls", p.onDemandCalls)
	}
	execSQL(t, s, context.Background(), `UPDATE price_cache SET fetched_at = clock_timestamp() - interval '2 hours'`)
	s.refreshPrices(context.Background())
	if p.onDemandCalls != 2 || cachedProviderRate(t, s) != "0.1" {
		t.Fatalf("failed stale refresh: calls=%d cached=%q", p.onDemandCalls, cachedProviderRate(t, s))
	}
	failed := providerRates(t, s, "od")
	if len(failed) != 1 || failed[0].perHour != "0.1" || !failed[0].open {
		t.Fatalf("failed stale refresh changed the usable rate: %+v", failed)
	}
	s.refreshPrices(context.Background())
	if p.onDemandCalls != 3 || cachedProviderRate(t, s) != "0.2" {
		t.Fatalf("retry: calls=%d cached=%q", p.onDemandCalls, cachedProviderRate(t, s))
	}
	after := providerRates(t, s, "od")
	if len(after) != 2 || after[0].perHour != "0.1" || after[0].open || after[1].perHour != "0.2" || !after[1].open || !after[0].to.Equal(after[1].from) {
		t.Fatalf("successful refresh did not close then replace the period: %+v", after)
	}
}

func TestProviderPricesRespectPoolTenantBoundary(t *testing.T) {
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{{rate: HourlyRate{PerHour: "0.42", Currency: "USD"}}}}
	s := providerPriceServer(t, p)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "t1-host", MarketOnDemand, from)
	insertProviderHost(t, s, "t2", "burst", "static", "t2-host", MarketOnDemand, from)
	s.refreshPrices(context.Background())
	if p.onDemandCalls != 1 {
		t.Fatalf("provider calls = %d, want only t1's ec2 host priced", p.onDemandCalls)
	}
	if got := providerRates(t, s, "t1-host"); len(got) != 1 || got[0].perHour != "0.42" {
		t.Errorf("t1 provider rates = %+v", got)
	}
	if got := providerRates(t, s, "t2-host"); len(got) != 0 {
		t.Errorf("t2 static pool acquired t1's provider price: %+v", got)
	}
}

func TestProviderPricesSpotLatestRateAndRetry(t *testing.T) {
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	old := from.Add(time.Hour)
	latest := from.Add(2 * time.Hour)
	p := &fakePriceProvider{
		spot: [][]SpotRate{
			{{At: latest, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}},
				{At: old, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
			nil,
			{{At: latest, HourlyRate: HourlyRate{PerHour: "0.3", Currency: "USD"}}},
		},
		spotErr: []error{nil, errors.New("spot unavailable")},
	}
	s := providerPriceServer(t, p)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "spot", "ec2", "spot-a", MarketSpot, from)
	execSQL(t, s, context.Background(), `INSERT INTO hosts
		(id, tenant_id, name, pool, state, provider_id, provision_requested_at, registered_at,
		 instance_type, market, zone, launch_template, capacity)
		SELECT 'spot-b', tenant_id, 'spot-b', pool, state, 'i-spot-b', provision_requested_at, registered_at,
		 instance_type, market, zone, launch_template, capacity FROM hosts WHERE id = 'spot-a'`)
	s.refreshPrices(context.Background())
	if len(p.spotCalls) != 1 || p.onDemandCalls != 0 || p.spotCalls[0].zone != "us-east-1a" || p.spotCalls[0].kind != "m7i.large" {
		t.Fatalf("spot request was not shared by zone/type: %+v", p.spotCalls)
	}
	for _, id := range []string{"spot-a", "spot-b"} {
		got := providerRates(t, s, id)
		if len(got) != 1 || got[0].perHour != "0.2" || !got[0].from.Equal(latest) || !got[0].open {
			t.Fatalf("%s latest spot rate = %+v", id, got)
		}
	}
	s.refreshPrices(context.Background())
	if got := providerRates(t, s, "spot-a"); len(got) != 1 || !got[0].open {
		t.Fatalf("failed provider request removed spot rate: %+v", got)
	}
	s.refreshPrices(context.Background())
	got := providerRates(t, s, "spot-a")
	if len(got) != 2 || got[0].open || got[0].perHour != "0.2" || !got[0].to.Equal(got[1].from) || got[1].perHour != "0.3" || !got[1].open {
		t.Fatalf("new spot price did not replace current rate: %+v", got)
	}
}

func TestProviderPricesSpotDoesNotUseOnDemandFallback(t *testing.T) {
	p := &fakePriceProvider{spot: [][]SpotRate{nil}, onDemand: []fakeOnDemandAnswer{{rate: HourlyRate{PerHour: "0.5", Currency: "USD"}}}}
	s := providerPriceServer(t, p)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "spot", "ec2", "spot-empty", MarketSpot, time.Now().Add(-time.Hour))
	s.refreshPrices(context.Background())
	if got := providerRates(t, s, "spot-empty"); len(got) != 0 || p.onDemandCalls != 0 {
		t.Fatalf("missing spot price used on-demand fallback: rates=%+v onDemandCalls=%d", got, p.onDemandCalls)
	}
}

func TestProviderPricesRecoverTerminatedIncompleteHosts(t *testing.T) {
	for _, tc := range []struct {
		market, price, amount, item string
	}{
		{MarketSpot, "0.2", "0.15", "m7i.large:spot"},
		{MarketOnDemand, "0.4", "0.3", "m7i.large"},
	} {
		t.Run(tc.market, func(t *testing.T) {
			from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
			end := from.Add(2 * time.Hour)
			p := &fakePriceProvider{
				onDemand: []fakeOnDemandAnswer{
					{err: errors.New("pricing temporarily unavailable")},
					{rate: HourlyRate{PerHour: "0.4", Currency: "USD"}},
				},
				spot: [][]SpotRate{
					nil,
					{{At: from.Add(time.Hour), HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}},
						{At: from.Add(90 * time.Minute), HourlyRate: HourlyRate{PerHour: "bad", Currency: "USD"}}},
				},
				spotErr: []error{errors.New("spot temporarily unavailable")},
			}
			s, keys := costFixture(t)
			s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute,
				Batch: DefaultCostsBatch, PricesRefresh: time.Hour,
				Prices: map[string]PriceProvider{"ec2": p}}
			insertProviderHost(t, s, "t1", "burst", "ec2", "recover", tc.market, from)
			placeRun(t, s, "t1", "pending-run", StateRunning, "recover", placementWindow{
				From: from.Add(30 * time.Minute), To: &end, CPUs: 2, Memory: 4 * gib,
			})
			s.refreshPrices(context.Background())
			if got := providerRates(t, s, "recover"); len(got) != 0 {
				t.Fatalf("failed live lookup wrote rates: %+v", got)
			}
			finish(t, s, "t1", "pending-run", StateSucceeded)
			drain(t, s)
			code, cost := getCost(t, s, keys["t1"], "pending-run")
			if code != http.StatusOK || cost.Status != "incomplete" || cost.Final || len(cost.Lines) != 0 ||
				len(cost.Sources) != 1 || cost.Sources[0].NextAt == nil || pending(t, s, "pending-run") != "" {
				t.Fatalf("unpriced terminal run: HTTP %d, cost %+v", code, cost)
			}
			execSQL(t, s, context.Background(), `UPDATE hosts SET state='terminated', terminated_at=$1 WHERE id='recover'`, end)
			s.refreshPrices(context.Background())
			rates := providerRates(t, s, "recover")
			if len(rates) != 1 || rates[0].open || rates[0].perHour != tc.price || !rates[0].from.Equal(from) || !rates[0].to.Equal(end) {
				t.Fatalf("recovered host window: %+v", rates)
			}
			if tc.market == MarketSpot {
				if len(p.spotCalls) != 2 || p.onDemandCalls != 0 {
					t.Fatalf("spot recovery used wrong market: spot=%d on-demand=%d", len(p.spotCalls), p.onDemandCalls)
				}
			} else if p.onDemandCalls != 2 || len(p.spotCalls) != 0 {
				t.Fatalf("on-demand recovery used wrong market: on-demand=%d spot=%d", p.onDemandCalls, len(p.spotCalls))
			}
			execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'pending-run'`)
			if _, err := s.costTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := pending(t, s, "pending-run"); got == "" {
				t.Fatal("terminal incomplete run was not queued for recovery")
			}
			drain(t, s)
			code, cost = getCost(t, s, keys["t1"], "pending-run")
			if code != http.StatusOK || cost.Status != "final" || !cost.Final || len(cost.Lines) != 1 ||
				cost.Lines[0].Item != tc.item || cost.Lines[0].Amount != tc.amount || !cost.Lines[0].Final ||
				len(cost.Totals) != 1 || cost.Totals[0].Amount != tc.amount ||
				len(cost.Sources) != 1 || cost.Sources[0].Status != "final" || cost.Sources[0].NextAt != nil {
				t.Fatalf("recovered terminal estimate: HTTP %d, cost %+v", code, cost)
			}
			var rate, amount string
			var finalized bool
			systemScan(t, s, `SELECT trim_scale(per_hour)::text, trim_scale(amount)::text, finalized
				FROM cost_placement_snapshots WHERE placement_id = 'p-pending-run'`, nil, &rate, &amount, &finalized)
			if rate != tc.price || amount != tc.amount || !finalized {
				t.Fatalf("placement snapshot: rate=%s amount=%s finalized=%v", rate, amount, finalized)
			}
			s.refreshPrices(context.Background())
			if got := providerRates(t, s, "recover"); len(got) != 1 {
				t.Fatalf("repeated refresh changed recovered period: %+v", got)
			}
		})
	}
}

func TestProviderPricesRecoveryRequiresPendingUnpricedPlacement(t *testing.T) {
	p := &fakePriceProvider{spot: [][]SpotRate{{{At: time.Now().Add(-time.Hour), HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}}}}
	s, _ := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, PricesRefresh: time.Hour,
		Prices: map[string]PriceProvider{"ec2": p}}
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	insertProviderHost(t, s, "t1", "burst", "ec2", "idle", MarketSpot, from)
	insertProviderHost(t, s, "t1", "other", "ec2", "not-pending", MarketSpot, from)
	end := from.Add(2 * time.Hour)
	placeRun(t, s, "t1", "unqueued-run", StateRunning, "not-pending", placementWindow{
		From: from.Add(time.Minute), To: &end, CPUs: 2, Memory: 4 * gib,
	})
	execSQL(t, s, context.Background(), `UPDATE hosts SET state='terminated', terminated_at=$1 WHERE id IN ('idle','not-pending')`, end)
	s.refreshPrices(context.Background())
	if len(p.spotCalls) != 0 || len(providerRates(t, s, "idle")) != 0 || len(providerRates(t, s, "not-pending")) != 0 {
		t.Fatalf("unneeded recovery: calls=%d", len(p.spotCalls))
	}
}

func TestProviderPricesOnDemandLatePriceResolvesIncompletePlacement(t *testing.T) {
	from := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	registered := from.Add(time.Hour)
	end := registered.Add(time.Hour)
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{
		{err: errors.New("pricing temporarily unavailable")},
		{rate: HourlyRate{PerHour: "0.40", Currency: "USD"}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		PricesRefresh: time.Hour, Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "od-cost", MarketOnDemand, from)
	execSQL(t, s, context.Background(), `UPDATE hosts SET registered_at = $1 WHERE id = 'od-cost'`, registered)
	placeRun(t, s, "t1", "od-run", StateRunning, "od-cost", placementWindow{
		From: from.Add(30 * time.Minute), To: &end, CPUs: 2, Memory: 4 * gib,
	})
	finish(t, s, "t1", "od-run", StateSucceeded)
	if got := pending(t, s, "od-run"); got == "" {
		t.Fatal("finishing on-demand run did not queue its cost")
	}
	s.refreshPrices(context.Background())
	drain(t, s)
	code, c := getCost(t, s, keys["t1"], "od-run")
	if code != http.StatusOK || c.Status != "incomplete" || len(c.Lines) != 0 ||
		len(c.Sources) != 1 || c.Sources[0].NextAt == nil {
		t.Fatalf("on-demand lookup failed: HTTP %d, cost %+v", code, c)
	}
	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'od-run'`)
	if _, err := s.costTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	code, c = getCost(t, s, keys["t1"], "od-run")
	if code != http.StatusOK || c.Status != "final" || !c.Final || len(c.Lines) != 1 ||
		len(c.Sources) != 1 || c.Sources[0].NextAt != nil || len(c.Totals) != 1 {
		t.Fatalf("latest available on-demand rate must resolve the incomplete placement: HTTP %d, cost %+v", code, c)
	}
	if rates := providerRates(t, s, "od-cost"); len(rates) != 1 || !rates[0].from.After(end) {
		t.Fatalf("late on-demand period must start after the unpriced placement: %+v", rates)
	}
	if p.onDemandCalls != 2 {
		t.Errorf("on-demand lookups = %d, want failed fetch and retry", p.onDemandCalls)
	}
}
