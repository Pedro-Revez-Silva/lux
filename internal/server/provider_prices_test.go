package server

import (
	"context"
	"errors"
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
	if len(first) != 1 || first[0].perHour != "0.1" || !first[0].open || !first[0].from.Equal(from) {
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
