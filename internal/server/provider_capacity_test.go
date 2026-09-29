package server

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func TestProviderHelloCapacitySplitsRunCost(t *testing.T) {
	s, _ := costFixture(t)
	s.cfg.Costs = CostsConfig{Enabled: true, ComputeEC2: true}
	ctx := context.Background()
	from := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)
	insertProviderHost(t, s, "t1", "burst", "ec2", "capacity-host", MarketOnDemand, from)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('capacity-host', $1, 3600, 'USD', 4, $2, 'ec2-pricing')`, from, 16*gib)
	placeRun(t, s, "t1", "capacity-run", StateRunning, "capacity-host", placementWindow{
		From: from, CPUs: 2, Memory: 4 * gib,
	})
	var before hostCompute
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		before, err = loadHostCompute(ctx, tx, "capacity-host", from, from.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	baseline, err := computeCost(before)
	if err != nil || len(baseline.Placements) != 1 || baseline.Placements[0].Amounts["USD"] == nil ||
		baseline.Placements[0].Amounts["USD"].Cmp(big.NewRat(1800, 1)) != 0 {
		t.Fatalf("cost before hello: %+v, %v", baseline.Placements, err)
	}
	tok := testHostToken(t, s, ctx)
	tenant := "t1"
	tok.TenantID = &tenant
	tok.PoolID = new("pool-t1-ec2-burst")
	hello := proto.Hello{Name: "capacity-host", ProviderID: "i-capacity-host", ProtocolVersion: proto.Version,
		Capacity: proto.Capacity{CPUs: 8, Memory: 32 * gib}}
	if _, err := s.registerHost(ctx, tok, hello); err != nil {
		t.Fatal(err)
	}
	var boundary, oldFrom time.Time
	var oldTo *time.Time
	var oldPrice, newPrice, oldSource, newSource, currency string
	var oldCPUs, newCPUs float64
	var oldMemory, newMemory int64
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT valid_from, valid_to, trim_scale(per_hour)::text, currency, cap_cpus, cap_memory, source
			FROM host_rates WHERE host_id = 'capacity-host' ORDER BY valid_from`)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("missing old rate")
		}
		if err := rows.Scan(&oldFrom, &oldTo, &oldPrice, &currency, &oldCPUs, &oldMemory, &oldSource); err != nil {
			return err
		}
		if !rows.Next() {
			t.Fatal("missing new rate")
		}
		var to *time.Time
		if err := rows.Scan(&boundary, &to, &newPrice, &currency, &newCPUs, &newMemory, &newSource); err != nil {
			return err
		}
		if to != nil || rows.Next() {
			t.Fatalf("unexpected extra or closed new rate: %v", to)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if oldTo == nil || !oldTo.Equal(boundary) || !oldFrom.Equal(from) || oldPrice != "3600" || newPrice != oldPrice ||
		currency != "USD" || oldSource != "ec2-pricing" || newSource != oldSource || oldCPUs != 4 || newCPUs != 8 ||
		oldMemory != 16*gib || newMemory != 32*gib {
		t.Fatalf("incorrect capacity boundary: from=%v to=%v boundary=%v prices=%s/%s sources=%s/%s capacity=%v/%v %v/%v",
			oldFrom, oldTo, boundary, oldPrice, newPrice, oldSource, newSource, oldCPUs, newCPUs, oldMemory, newMemory)
	}
	// A closed placement can span the boundary without racing the live cost clock.
	end := boundary.Add(time.Hour)
	execSQL(t, s, ctx, `UPDATE placements SET ended_at = $1, state = 'exited' WHERE run_id = 'capacity-run'`, end)
	var in hostCompute
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		in, err = loadHostCompute(ctx, tx, "capacity-host", from, end)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	want := new(big.Rat).SetFrac(big.NewInt(boundary.Sub(from).Nanoseconds()), big.NewInt(int64(time.Hour)))
	want.Mul(want, big.NewRat(1800, 1))
	want.Add(want, big.NewRat(900, 1))
	if len(result.Placements) != 1 || result.Placements[0].Amounts["USD"] == nil ||
		result.Placements[0].Amounts["USD"].Cmp(want) != 0 {
		t.Fatalf("run cost across capacity boundary: %+v, want %s USD", result.Placements, want)
	}
	if _, err := s.registerHost(ctx, tok, hello); err != nil {
		t.Fatal(err)
	}
	if rates := providerRates(t, s, "capacity-host"); len(rates) != 2 || !rates[0].to.Equal(boundary) {
		t.Fatalf("repeat hello modified closed rates: %+v", rates)
	}
}

func TestProviderFirstHelloUsesOnlyPreRegistrationCache(t *testing.T) {
	p := &fakePriceProvider{}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	tok := testHostToken(t, s, ctx)
	tenant := "t1"
	tok.TenantID = &tenant
	tok.PoolID = new("pool-t1-ec2-burst")
	insertProviderHost(t, s, "t1", "burst", "ec2", "cached", MarketOnDemand, time.Now().Add(-time.Hour))
	for _, tc := range []struct {
		id, market string
	}{
		{"cached", MarketOnDemand},
		{"late", MarketOnDemand},
		{"spot", MarketSpot},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if tc.id != "cached" {
				execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at,
					instance_type, market, launch_template) VALUES ($1, 't1', $1, 'pool-t1-ec2-burst', 'provisioning', 'i-' || $1::text,
					now() - interval '1 hour', 'm7i.large', $2, '{"region":"us-east-1"}')`, tc.id, tc.market)
			}
			execSQL(t, s, ctx, `UPDATE hosts SET registered_at = NULL WHERE id = $1`, tc.id)
			fetched := time.Now().Add(-time.Minute)
			if tc.id == "late" {
				fetched = time.Now().Add(time.Minute)
			}
			execSQL(t, s, ctx, `INSERT INTO price_cache (provider, region, instance_type, os, per_hour, currency, fetched_at)
					VALUES ('ec2', 'us-east-1', 'm7i.large', 'Linux', 0.4, 'USD', $1)
					ON CONFLICT (provider, region, instance_type, os) DO UPDATE SET fetched_at = EXCLUDED.fetched_at`, fetched)
			_, err := s.registerHost(ctx, tok, proto.Hello{Name: tc.id, ProviderID: "i-" + tc.id,
				ProtocolVersion: proto.Version, Capacity: proto.Capacity{CPUs: 4, Memory: 16 * gib}})
			if err != nil {
				t.Fatal(err)
			}
			rates := providerRates(t, s, tc.id)
			if tc.id == "cached" {
				var registered time.Time
				systemScan(t, s, `SELECT registered_at FROM hosts WHERE id = $1`, []any{tc.id}, &registered)
				if len(rates) != 1 || !rates[0].open || !rates[0].from.Equal(registered) || rates[0].perHour != "0.4" {
					t.Fatalf("cached first hello rates: %+v, registered %v", rates, registered)
				}
			} else if len(rates) != 0 {
				t.Fatalf("unknown price acquired a rate: %+v", rates)
			}
		})
	}
	if p.onDemandCalls != 0 || len(p.spotCalls) != 0 {
		t.Fatalf("hello called provider: on-demand %d, spot %d", p.onDemandCalls, len(p.spotCalls))
	}
}
