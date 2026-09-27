package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// fakePriceProvider gives each refresh exactly the provider answer the test
// needs, while retaining the calls so cache and retry behaviour is observable.
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

// insertProviderHost is deliberately system-scoped: provider price discovery
// crosses tenants, but must join a host to the pool in that same tenant.
func insertProviderHost(t *testing.T, s *Server, tenant, pool, provider, id, market string, from time.Time) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider)
		VALUES ($1, $2, $3, $4)`, "pool-"+tenant+"-"+provider, tenant, pool, provider)
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

// A fresh shared on-demand cache avoids another provider call. Once stale it
// is refreshed; an API failure keeps the old rate usable and is retried on the
// next pass rather than making an existing host's rate disappear.
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
		t.Fatalf("first refresh: calls=%d cached=%q, want one call and 0.1", p.onDemandCalls, cachedProviderRate(t, s))
	}
	first := providerRates(t, s, "od")
	if len(first) != 1 || first[0].perHour != "0.1" || !first[0].open || !first[0].from.After(from) {
		t.Fatalf("first on-demand period: %+v", first)
	}

	s.refreshPrices(context.Background())
	if p.onDemandCalls != 1 {
		t.Fatalf("fresh cached refresh made %d provider calls, want 1 total", p.onDemandCalls)
	}

	execSQL(t, s, context.Background(), `UPDATE price_cache SET fetched_at = clock_timestamp() - interval '2 hours'`)
	s.refreshPrices(context.Background())
	if p.onDemandCalls != 2 || cachedProviderRate(t, s) != "0.1" {
		t.Fatalf("failed stale refresh: calls=%d cached=%q, want retry with retained 0.1", p.onDemandCalls, cachedProviderRate(t, s))
	}
	failed := providerRates(t, s, "od")
	if len(failed) != 1 || failed[0].perHour != "0.1" || !failed[0].open {
		t.Fatalf("failed stale refresh changed the usable rate: %+v", failed)
	}

	s.refreshPrices(context.Background())
	if p.onDemandCalls != 3 || cachedProviderRate(t, s) != "0.2" {
		t.Fatalf("retry: calls=%d cached=%q, want third call and 0.2", p.onDemandCalls, cachedProviderRate(t, s))
	}
	after := providerRates(t, s, "od")
	if len(after) != 2 || after[0].perHour != "0.1" || after[0].open || after[1].perHour != "0.2" || !after[1].open || !after[0].to.Equal(after[1].from) {
		t.Fatalf("successful refresh did not close then replace the period: %+v", after)
	}
}

// Pools are named per tenant. A provider-priced host must be joined to its
// own tenant's pool, never to another tenant's same-named pool.
func TestProviderPricesRespectPoolTenantBoundary(t *testing.T) {
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{{rate: HourlyRate{PerHour: "0.42", Currency: "USD"}}}}
	s := providerPriceServer(t, p)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "t1-host", MarketOnDemand, from)
	// This is a provisioned row only to make a mistaken cross-tenant pool join
	// observable. Its own static pool must exclude it from provider pricing.
	insertProviderHost(t, s, "t2", "burst", "static", "t2-host", MarketOnDemand, from)

	s.refreshPrices(context.Background())
	if p.onDemandCalls != 1 {
		t.Fatalf("provider calls = %d, want only t1's ec2 host priced", p.onDemandCalls)
	}
	if got := providerRates(t, s, "t1-host"); len(got) != 1 || got[0].perHour != "0.42" {
		t.Errorf("t1 provider rates = %+v, want its on-demand rate", got)
	}
	if got := providerRates(t, s, "t2-host"); len(got) != 0 {
		t.Errorf("t2 static pool acquired t1's provider price: %+v", got)
	}
}

// Spot history begins only where the provider supplies a price. Later history
// can fill that historical gap, but cannot rewrite a closed period; its exact
// boundaries remain the provider's reported changes.
func TestProviderPricesSpotFailureRetriesWithoutChangingClosedPeriod(t *testing.T) {
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	change := from.Add(time.Hour)
	p := &fakePriceProvider{
		spot: [][]SpotRate{
			{{At: change, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
			nil,
			{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}},
				{At: change, HourlyRate: HourlyRate{PerHour: "0.9", Currency: "USD"}}},
		},
		spotErr: []error{nil, errors.New("spot history unavailable")},
	}
	s := providerPriceServer(t, p)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "spot", "ec2", "spot-retry", MarketSpot, from)
	s.refreshPrices(context.Background())
	before := providerRates(t, s, "spot-retry")
	s.refreshPrices(context.Background())
	if got := providerRates(t, s, "spot-retry"); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed fetch changed periods: before=%+v after=%+v", before, got)
	}
	s.refreshPrices(context.Background())
	got := providerRates(t, s, "spot-retry")
	if len(p.spotCalls) != 3 || len(got) != 2 || got[0].perHour != "0.1" || !got[0].from.Equal(from) || !got[0].to.Equal(change) ||
		got[1].perHour != "0.2" || !got[1].from.Equal(change) || !got[1].open {
		t.Fatalf("retry did not fill gap while preserving old period: calls=%d rates=%+v", len(p.spotCalls), got)
	}
}

func TestProviderPricesSpotPeriodsFillGapsWithoutRewritingClosedRates(t *testing.T) {
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	change1 := from.Add(time.Hour)
	change2 := change1.Add(time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{
			{At: change1, HourlyRate: HourlyRate{PerHour: "0.20", Currency: "USD"}},
			{At: change2, HourlyRate: HourlyRate{PerHour: "0.30", Currency: "USD"}},
		},
		{
			{At: from, HourlyRate: HourlyRate{PerHour: "0.10", Currency: "USD"}},
			// This differs deliberately: change1--change2 is already closed.
			{At: change1, HourlyRate: HourlyRate{PerHour: "0.99", Currency: "USD"}},
			{At: change2, HourlyRate: HourlyRate{PerHour: "0.30", Currency: "USD"}},
		},
	}}
	s := providerPriceServer(t, p)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "spot", "ec2", "spot-host", MarketSpot, from)

	s.refreshPrices(context.Background())
	first := providerRates(t, s, "spot-host")
	if len(p.spotCalls) != 1 || p.spotCalls[0].zone != "us-east-1a" || p.spotCalls[0].kind != "m7i.large" || !p.spotCalls[0].from.Equal(from) {
		t.Fatalf("spot request = %+v, want zone/type and host start", p.spotCalls)
	}
	if len(first) != 2 || first[0].perHour != "0.2" || !first[0].from.Equal(change1) || !first[0].to.Equal(change2) || first[0].open ||
		first[1].perHour != "0.3" || !first[1].from.Equal(change2) || !first[1].open {
		t.Fatalf("initial spot history should leave the leading gap: %+v", first)
	}

	s.refreshPrices(context.Background())
	got := providerRates(t, s, "spot-host")
	if len(got) != 3 {
		t.Fatalf("filled spot periods = %+v, want three", got)
	}
	if got[0].perHour != "0.1" || !got[0].from.Equal(from) || !got[0].to.Equal(change1) || got[0].open ||
		got[1].perHour != "0.2" || !got[1].from.Equal(change1) || !got[1].to.Equal(change2) || got[1].open ||
		got[2].perHour != "0.3" || !got[2].from.Equal(change2) || !got[2].open {
		t.Fatalf("spot gap fill or closed-period immutability failed: %+v", got)
	}
	for _, rate := range got {
		if rate.source != "ec2-spot-history" {
			t.Errorf("spot period source = %q, want ec2-spot-history", rate.source)
		}
	}
}

func TestProviderPricesSpotWindowBoundaryLeavesUnknownPriceMissing(t *testing.T) {
	from := time.Now().UTC().Add(-49 * time.Hour).Truncate(time.Microsecond)
	boundary := from.Add(24 * time.Hour)
	change := boundary.Add(time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: change, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
	}}
	s := providerPriceServer(t, p)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "spot", "ec2", "spot-boundary", MarketSpot, from)
	var h pricedHost
	hosts, err := s.pricedHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range hosts {
		if host.ID == "spot-boundary" {
			h = host
		}
	}
	if h.ID == "" {
		t.Fatal("spot host not found")
	}
	first, err := p.SpotHistory(context.Background(), h.Zone, h.Type, from, boundary)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applySpotHistory(context.Background(), h, first, from, boundary, time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := p.SpotHistory(context.Background(), h.Zone, h.Type, boundary, change.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applySpotHistory(context.Background(), h, second, boundary, change.Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	rates := providerRates(t, s, "spot-boundary")
	if len(p.spotCalls) != 2 || len(rates) != 2 || !rates[0].from.Equal(from) || rates[0].perHour != "0.1" ||
		rates[0].open || !rates[0].to.Equal(boundary) || !rates[1].from.Equal(change) || rates[1].perHour != "0.2" || rates[1].open {
		t.Fatalf("unknown window start must not acquire a synthetic price: calls=%+v rates=%+v", p.spotCalls, rates)
	}
	if !p.spotCalls[0].to.Equal(boundary) || !p.spotCalls[1].from.Equal(boundary) {
		t.Fatalf("history requests must meet at the 24-hour boundary: %+v", p.spotCalls)
	}
}

func TestProviderPricesSpotLateHistorySettlesRunCost(t *testing.T) {
	from := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	change := from.Add(time.Hour)
	end := change.Add(time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: change, HourlyRate: HourlyRate{PerHour: "0.20", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.10", Currency: "USD"}},
			{At: change, HourlyRate: HourlyRate{PerHour: "0.90", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.10", Currency: "USD"}},
			{At: change, HourlyRate: HourlyRate{PerHour: "0.90", Currency: "USD"}}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		PricesRefresh: time.Hour, Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "spot-cost", MarketSpot, from)
	placeRun(t, s, "t1", "spot-run", StateRunning, "spot-cost", placementWindow{
		From: from, To: &end, CPUs: 2, Memory: 4 * gib,
	})
	finish(t, s, "t1", "spot-run", StateSucceeded)
	if got := pending(t, s, "spot-run"); got == "" {
		t.Fatal("finishing spot run did not queue its cost")
	}

	s.refreshPrices(context.Background())
	drain(t, s)
	code, c := getCost(t, s, keys["t1"], "spot-run")
	if code != http.StatusOK || c.Status != "incomplete" || len(c.Lines) != 1 ||
		c.Lines[0].Item != "m7i.large:spot" || c.Lines[0].Amount != "0.1" || c.Lines[0].Final ||
		c.Lines[0].Details["missingRate"] != true || len(c.Sources) != 1 || c.Sources[0].NextAt == nil {
		t.Fatalf("spot history with leading gap: HTTP %d, cost %+v", code, c)
	}

	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'spot-run'`)
	if _, err := s.costTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	code, c = getCost(t, s, keys["t1"], "spot-run")
	if code != http.StatusOK || c.Status != "complete" || c.Final || len(c.Lines) != 1 ||
		c.Lines[0].Amount != "0.15" || c.Lines[0].Final || c.Lines[0].Details["missingRate"] != nil ||
		len(c.Sources) != 1 || c.Sources[0].NextAt == nil || totals(c.Totals) != "/USD=0.15(f0,e0.15) " {
		t.Fatalf("late history should fill gap without repricing or finalizing before settle: HTTP %d, cost %+v", code, c)
	}

	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'spot-run'`)
	execSQL(t, s, context.Background(), `DELETE FROM cost_ticks`)
	if _, err := s.costTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	code, c = getCost(t, s, keys["t1"], "spot-run")
	if code != http.StatusOK || c.Status != "complete" || c.Final || len(c.Lines) != 1 ||
		c.Lines[0].Amount != "0.15" || c.Lines[0].Final || len(c.Sources) != 1 ||
		c.Sources[0].NextAt == nil || totals(c.Totals) != "/USD=0.15(f0,e0.15) " {
		t.Fatalf("spot cost remains eligible before time-based settlement: HTTP %d, cost %+v", code, c)
	}
}

func TestProviderPricesSpotTerminatedHistoryAcrossWindows(t *testing.T) {
	from := time.Now().UTC().Add(-80 * time.Hour).Truncate(time.Microsecond)
	boundary := from.Add(24 * time.Hour)
	change := from.Add(30 * time.Hour)
	end := from.Add(49 * time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: boundary, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}},
			{At: change, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
		{{At: from.Add(48 * time.Hour), HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "spot-49h", MarketSpot, from)
	execSQL(t, s, context.Background(), `UPDATE hosts SET terminated_at = $1 WHERE id = 'spot-49h'`, end)
	placeRun(t, s, "t1", "spot-49h-run", StateRunning, "spot-49h", placementWindow{
		From: from, To: &end, CPUs: 4, Memory: 16 * gib,
	})
	finish(t, s, "t1", "spot-49h-run", StateSucceeded)
	drain(t, s)

	s.refreshPrices(context.Background())
	if calls := p.spotCalls; len(calls) != 1 || !calls[0].from.Equal(from) || !calls[0].to.Equal(boundary) {
		t.Fatalf("first bounded request: %+v", calls)
	}
	if rates := providerRates(t, s, "spot-49h"); len(rates) != 1 || rates[0].open || !rates[0].to.Equal(boundary) {
		t.Fatalf("unqueried tail must not acquire an open rate: %+v", rates)
	}
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "spot-49h-run"); c.Status != "incomplete" || c.Final {
		t.Fatalf("unqueried placement tail must remain incomplete: %+v", c)
	}
	s.refreshPrices(context.Background())
	s.refreshPrices(context.Background())
	if calls := p.spotCalls; len(calls) != 3 || !calls[1].from.Equal(boundary) || !calls[1].to.Equal(from.Add(48*time.Hour)) ||
		!calls[2].from.Equal(from.Add(48*time.Hour)) || !calls[2].to.Equal(end) {
		t.Fatalf("historical gap requests: %+v", calls)
	}
	rates := providerRates(t, s, "spot-49h")
	if len(rates) != 4 || rates[0].open || !rates[0].to.Equal(boundary) || rates[1].open || !rates[1].to.Equal(change) ||
		rates[2].open || !rates[2].to.Equal(from.Add(48*time.Hour)) || !rates[3].open || !rates[3].from.Equal(from.Add(48*time.Hour)) {
		t.Fatalf("bounded historical rates: %+v", rates)
	}
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('spot-49h-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "spot-49h-run"); c.Status != "complete" || c.Final || len(c.Lines) != 1 || c.Lines[0].Amount != "6.8" {
		t.Fatalf("49h cost after all windows checked: %+v", c)
	}
}

func TestProviderPricesSpotDelayedChangeAfterTwoCompleteAnswers(t *testing.T) {
	from := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Microsecond)
	change := from.Add(time.Hour)
	end := from.Add(2 * time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}},
			{At: change, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "spot-delayed", MarketSpot, from)
	execSQL(t, s, context.Background(), `UPDATE hosts SET terminated_at = $1 WHERE id = 'spot-delayed'`, end)
	placeRun(t, s, "t1", "spot-delayed-run", StateRunning, "spot-delayed", placementWindow{
		From: from, To: &end, CPUs: 4, Memory: 16 * gib,
	})
	finish(t, s, "t1", "spot-delayed-run", StateSucceeded)
	drain(t, s)
	for i := range 2 {
		s.refreshPrices(context.Background())
		execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('spot-delayed-run', 'retry')`)
		drain(t, s)
		if _, c := getCost(t, s, keys["t1"], "spot-delayed-run"); c.Status != "complete" || c.Final || len(c.Lines) != 1 || c.Lines[0].Amount != "0.2" {
			t.Fatalf("complete answer %d must not finalize: %+v", i+1, c)
		}
	}
	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('spot-delayed-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "spot-delayed-run"); c.Status != "complete" || c.Final || c.Lines[0].Amount != "0.3" {
		t.Fatalf("late price must update estimated cost: %+v", c)
	}
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('spot-delayed-run', 'retry')`)
	settled := claimOne(t, s)
	settled.now = time.Now().Add(costSpotSettle + time.Minute)
	if err := settled.write(s); err != nil {
		t.Fatal(err)
	}
	if _, c := getCost(t, s, keys["t1"], "spot-delayed-run"); c.Final || c.Lines[0].Amount != "0.3" {
		t.Fatalf("future threshold without refresh must not settle: %+v", c)
	}
	s.refreshPrices(context.Background())
	if len(p.spotCalls) != 4 {
		t.Fatalf("estimated host was not queried again: %+v", p.spotCalls)
	}
}

func TestProviderPricesSpotLiveHostDoesNotSettle(t *testing.T) {
	from := time.Now().UTC().Add(-50 * time.Hour).Truncate(time.Microsecond)
	end := from.Add(time.Hour)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "live-spot", MarketSpot, from)
	placeRun(t, s, "t1", "live-spot-run", StateRunning, "live-spot", placementWindow{From: from, To: &end, CPUs: 4, Memory: 16 * gib})
	finish(t, s, "t1", "live-spot-run", StateSucceeded)
	execSQL(t, s, context.Background(), `UPDATE runs SET finished_at = $1 WHERE id = 'live-spot-run'`, end)
	s.refreshPrices(context.Background())
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "live-spot-run"); c.Status != "complete" || c.Final || c.Lines[0].Amount != "0.1" || c.Sources[0].NextAt == nil {
		t.Fatalf("live host after 24h must remain estimated: %+v", c)
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET finished_at = now() - interval '8 days' WHERE id = 'live-spot-run'`)
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('live-spot-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "live-spot-run"); c.Final || c.Sources[0].NextAt == nil {
		t.Fatalf("live host must keep retrying beyond ordinary give-up: %+v", c)
	}
	execSQL(t, s, context.Background(), `UPDATE hosts SET terminated_at = now() - interval '25 hours' WHERE id = 'live-spot'`)
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('live-spot-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "live-spot-run"); c.Final {
		t.Fatalf("termination without post-threshold refresh finalized: %+v", c)
	}
	s.refreshPrices(context.Background())
	if len(p.spotCalls) < 2 {
		t.Fatalf("terminated estimated host must remain eligible for history: %+v", p.spotCalls)
	}
}

func TestProviderPricesSpotDrainBeforeFinalHistoryRefresh(t *testing.T) {
	from := time.Now().UTC().Add(-25*time.Hour - time.Second).Truncate(time.Microsecond)
	end := from.Add(time.Hour)
	change := end.Add(-10 * time.Minute)
	p := &fakePriceProvider{spot: [][]SpotRate{
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}}},
		{{At: from, HourlyRate: HourlyRate{PerHour: "0.1", Currency: "USD"}},
			{At: change, HourlyRate: HourlyRate{PerHour: "0.2", Currency: "USD"}}},
	}}
	s, keys := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true, Every: time.Minute, Batch: DefaultCostsBatch,
		Prices: map[string]PriceProvider{"ec2": p}}
	insertProviderHost(t, s, "t1", "burst", "ec2", "final-spot", MarketSpot, from)
	execSQL(t, s, context.Background(), `UPDATE hosts SET terminated_at = $1 WHERE id = 'final-spot'`, end)
	placeRun(t, s, "t1", "final-spot-run", StateRunning, "final-spot", placementWindow{From: from, To: &end, CPUs: 4, Memory: 16 * gib})
	finish(t, s, "t1", "final-spot-run", StateSucceeded)
	execSQL(t, s, context.Background(), `UPDATE runs SET finished_at = $1 WHERE id = 'final-spot-run'`, end)
	drain(t, s)
	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `UPDATE spot_history_refresh SET checked_at = $1 WHERE host_id = 'final-spot'`, end.Add(costSpotSettle-time.Second))
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('final-spot-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "final-spot-run"); c.Final || len(c.Lines) != 1 || c.Lines[0].Amount != "0.1" || c.Sources[0].NextAt == nil {
		t.Fatalf("drain before final refresh must remain estimated: %+v; calls=%+v; rates=%+v", c, p.spotCalls, providerRates(t, s, "final-spot"))
	}
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('final-spot-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "final-spot-run"); c.Final || c.Lines[0].Amount != "0.1" {
		t.Fatalf("complete price before recent overlap must remain estimated: %+v", c)
	}
	s.refreshPrices(context.Background())
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('final-spot-run', 'retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "final-spot-run"); !c.Final || c.Lines[0].Amount != "0.116666667" {
		t.Fatalf("post-threshold refresh must be consumed: %+v", c)
	}
	s.refreshPrices(context.Background())
	if len(p.spotCalls) != 2 {
		t.Fatalf("finalized host should no longer be queried: %+v", p.spotCalls)
	}
}

func TestProviderPricesOnDemandLatePriceLeavesRunGap(t *testing.T) {
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
	if code != http.StatusOK || c.Status != "incomplete" || c.Final || len(c.Lines) != 0 ||
		len(c.Sources) != 1 || c.Sources[0].NextAt == nil || len(c.Totals) != 0 {
		t.Fatalf("price fetched after placement ended must not backfill its gap: HTTP %d, cost %+v", code, c)
	}
	if rates := providerRates(t, s, "od-cost"); len(rates) != 1 || !rates[0].from.After(end) {
		t.Fatalf("late on-demand period must start after the unpriced placement: %+v", rates)
	}
	if p.onDemandCalls != 2 {
		t.Errorf("on-demand lookups = %d, want failed fetch and retry", p.onDemandCalls)
	}
}
