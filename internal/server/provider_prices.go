package server

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
)

// HourlyRate and SpotRate are provider-neutral prices. A provider does not
// know about lux hosts, tenants, transactions or the cost queue.
type HourlyRate struct{ PerHour, Currency string }
type SpotRate struct {
	At time.Time
	HourlyRate
}
type PriceProvider interface {
	OnDemand(context.Context, string, string) (HourlyRate, error)                          // region, instance type
	SpotHistory(context.Context, string, string, time.Time, time.Time) ([]SpotRate, error) // zone, type, from, to
}

const DefaultPricesRefresh = 24 * time.Hour

type pricedHost struct {
	ID, Provider, Region, Type, Zone, Market string
	CPUs                                     float64
	Memory                                   int64
}

func (s *Server) pricedHosts(ctx context.Context) ([]pricedHost, error) {
	var out []pricedHost
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, p.provider, coalesce(h.launch_template->>'region',''), coalesce(h.instance_type,''),
    coalesce(h.zone,''), coalesce(h.market,''),
    coalesce((h.capacity->>'cpus')::float8,0), coalesce((h.capacity->>'memory')::int8,0)
    FROM hosts h JOIN pools p ON p.id = h.pool_id
    WHERE h.provision_requested_at IS NOT NULL AND h.provider_id IS NOT NULL AND p.provider <> 'static'
      AND (h.terminated_at IS NULL OR (
       NOT EXISTS (SELECT 1 FROM host_rates hr WHERE hr.host_id = h.id)
       AND EXISTS (SELECT 1 FROM placements pl
         JOIN cost_sources source ON source.run_id = pl.run_id AND source.source = 'compute' AND source.status = 'incomplete'
         LEFT JOIN cost_placement_snapshots snap ON snap.placement_id = pl.id
         WHERE pl.host_id = h.id AND pl.ended_at IS NOT NULL
           AND (snap.placement_id IS NULL OR snap.amount IS NULL))))
    ORDER BY h.id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pricedHost])
		return err
	})
	return out, err
}

func (s *Server) cachedPrice(ctx context.Context, provider, region, kind string) (HourlyRate, bool, error) {
	var r HourlyRate
	var fetched time.Time
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT trim_scale(per_hour)::text, currency, fetched_at FROM price_cache
    WHERE provider=$1 AND region=$2 AND instance_type=$3 AND os='Linux'`, provider, region, kind).
			Scan(&r.PerHour, &r.Currency, &fetched)
	})
	if err == pgx.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, time.Since(fetched) < s.cfg.Costs.PricesRefresh, nil
}

// onDemandPrice uses a cached price even if stale when the API is unavailable;
// a failed refresh must not turn a previously priced host into a rate gap.
func (s *Server) onDemandPrice(ctx context.Context, provider, region, kind string) (HourlyRate, error) {
	cached, fresh, err := s.cachedPrice(ctx, provider, region, kind)
	if err != nil || fresh {
		return cached, err
	}
	p := s.cfg.Costs.Prices[provider]
	if p == nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return cached, fmt.Errorf("no price provider for %s", provider)
	}
	rate, err := p.OnDemand(ctx, region, kind)
	if err != nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return rate, err
	}
	if err := validPrice(rate.PerHour, rate.Currency); err != nil || rate.Currency == "" {
		return HourlyRate{}, fmt.Errorf("invalid %s on-demand price %q %q: %v", provider, rate.PerHour, rate.Currency, err)
	}
	rate.PerHour = moneyString(mustRat(rate.PerHour))
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO price_cache (provider,region,instance_type,os,per_hour,currency,fetched_at)
    VALUES ($1,$2,$3,'Linux',$4,$5,clock_timestamp()) ON CONFLICT (provider,region,instance_type,os)
    DO UPDATE SET per_hour=EXCLUDED.per_hour,currency=EXCLUDED.currency,fetched_at=EXCLUDED.fetched_at`, provider, region, kind, rate.PerHour, rate.Currency)
		return err
	})
	return rate, err
}

// refreshPrices runs independently of cost draining and provisioning. Spot
// requests are shared by zone/type; terminated hosts are retried only while
// incomplete ended placements still need a rate.
func (s *Server) refreshPrices(ctx context.Context) {
	if !s.cfg.Costs.Enabled || !s.cfg.Costs.ComputeEC2 {
		return
	}
	hosts, err := s.pricedHosts(ctx)
	if err != nil {
		s.log.Warn("costs: list priced hosts", "err", err)
		return
	}
	type spotKey struct{ provider, zone, kind string }
	groups := map[spotKey][]pricedHost{}
	type demandKey struct{ provider, region, kind string }
	type demandResult struct {
		rate HourlyRate
		err  error
	}
	demand := map[demandKey]demandResult{}
	for _, h := range hosts {
		if h.Type == "" || h.Region == "" {
			continue
		}
		if h.Market == MarketSpot {
			if h.Zone != "" {
				k := spotKey{h.Provider, h.Zone, h.Type}
				groups[k] = append(groups[k], h)
			}
			continue
		}
		if h.Market != MarketOnDemand {
			continue
		}
		key := demandKey{h.Provider, h.Region, h.Type}
		result, ok := demand[key]
		if !ok {
			result.rate, result.err = s.onDemandPrice(ctx, h.Provider, h.Region, h.Type)
			demand[key] = result
		}
		rate, err := result.rate, result.err
		if err == nil {
			err = s.applyCurrentPrice(ctx, h, rate, h.Provider+"-pricing")
		}
		if err != nil && ctx.Err() == nil {
			s.log.Warn("costs: on-demand price", "host", h.ID, "err", err)
		}
	}
	for key, hs := range groups {
		p := s.cfg.Costs.Prices[key.provider]
		if p == nil {
			continue
		}
		now := time.Now()
		history, err := p.SpotHistory(ctx, key.zone, key.kind, now.Add(-DefaultPricesRefresh), now)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("costs: spot price", "zone", key.zone, "type", key.kind, "err", err)
			}
			continue
		}
		if len(history) == 0 {
			continue
		}
		var latest *SpotRate
		for i := range history {
			r := &history[i]
			if err := validPrice(r.PerHour, r.Currency); err != nil || r.Currency == "" || r.At.After(now) || r.At.IsZero() {
				continue
			}
			if latest == nil || r.At.After(latest.At) {
				latest = r
			}
		}
		if latest == nil {
			s.log.Warn("costs: no valid spot price", "zone", key.zone, "type", key.kind)
			continue
		}
		price := *latest
		price.PerHour = moneyString(mustRat(price.PerHour))
		for _, h := range hs {
			if err := s.applySpotPrice(ctx, h, price); err != nil && ctx.Err() == nil {
				s.log.Warn("costs: apply spot price", "host", h.ID, "err", err)
			}
		}
	}
}

// applySpotPrice retains the last known rate until a newer observation arrives.
// A price observed after registration does not backfill an unknown earlier rate.
func (s *Server) applySpotPrice(ctx context.Context, h pricedHost, price SpotRate) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var from time.Time
		var registered, ended *time.Time
		var cpus float64
		var memory int64
		if err := tx.QueryRow(ctx, `SELECT provision_requested_at, registered_at, terminated_at,
    coalesce((capacity->>'cpus')::float8,0), coalesce((capacity->>'memory')::int8,0)
    FROM hosts WHERE id=$1 FOR UPDATE`, h.ID).Scan(&from, &registered, &ended, &cpus, &memory); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		if cpus <= 0 && memory <= 0 {
			return nil
		}
		if ended != nil {
			// Recovery uses one observed estimate only for a host with no rate history.
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_rates WHERE host_id=$1)`, h.ID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if registered != nil {
				from = *registered
			}
			if !from.Before(*ended) {
				return nil
			}
			_, err := tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,valid_to,per_hour,currency,cap_cpus,cap_memory,source)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (host_id,valid_from) DO NOTHING`,
				h.ID, from, ended, price.PerHour, price.Currency, cpus, memory, h.Provider+"-spot-history")
			return err
		}
		if registered != nil {
			from = *registered
		}
		var oldFrom time.Time
		var oldRate, oldCurrency string
		var oldCPUs float64
		var oldMemory int64
		err := tx.QueryRow(ctx, `SELECT valid_from,trim_scale(per_hour)::text,currency,cap_cpus,cap_memory
    FROM host_rates WHERE host_id=$1 AND valid_to IS NULL`, h.ID).
			Scan(&oldFrom, &oldRate, &oldCurrency, &oldCPUs, &oldMemory)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil {
			if oldRate == price.PerHour && oldCurrency == price.Currency && oldCPUs == cpus && oldMemory == memory {
				return nil
			}
			from = time.Now()
			if !from.After(oldFrom) {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE host_rates SET valid_to=$2 WHERE host_id=$1 AND valid_to IS NULL`, h.ID, from); err != nil {
				return err
			}
		} else {
			from = maxTime(from, price.At)
			if from.After(time.Now()) {
				return nil
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,per_hour,currency,cap_cpus,cap_memory,source)
    VALUES ($1,$2,$3,$4,$5,$6,$7)`, h.ID, from, price.PerHour, price.Currency, cpus, memory, h.Provider+"-spot-history")
		return err
	})
}

func (s *Server) applyCurrentPrice(ctx context.Context, h pricedHost, rate HourlyRate, source string) error {
	if h.CPUs <= 0 && h.Memory <= 0 {
		return nil
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var from time.Time
		var cpus float64
		var memory int64
		var registered, ended *time.Time
		if err := tx.QueryRow(ctx, `SELECT provision_requested_at, registered_at, coalesce((capacity->>'cpus')::float8,0),
    coalesce((capacity->>'memory')::int8,0), terminated_at FROM hosts WHERE id=$1 FOR UPDATE`, h.ID).
			Scan(&from, &registered, &cpus, &memory, &ended); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		if cpus <= 0 && memory <= 0 {
			return nil
		}
		// Capacity is not known until the runner registers. Prior time is missing.
		if registered != nil {
			from = *registered
		}
		if ended != nil {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_rates WHERE host_id=$1)`, h.ID).Scan(&exists); err != nil {
				return err
			}
			if exists || !from.Before(*ended) {
				return nil
			}
			_, err := tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,valid_to,per_hour,currency,cap_cpus,cap_memory,source)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (host_id,valid_from) DO NOTHING`,
				h.ID, from, ended, rate.PerHour, rate.Currency, cpus, memory, source)
			return err
		}
		var fetched time.Time
		if err := tx.QueryRow(ctx, `SELECT fetched_at FROM price_cache
    WHERE provider=$1 AND region=$2 AND instance_type=$3 AND os='Linux'`, h.Provider, h.Region, h.Type).Scan(&fetched); err != nil {
			return err
		}
		var oldFrom time.Time
		var oldRate, oldCurrency string
		var oldCPUs float64
		var oldMemory int64
		err := tx.QueryRow(ctx, `SELECT valid_from,trim_scale(per_hour)::text,currency,cap_cpus,cap_memory FROM host_rates
    WHERE host_id=$1 AND valid_to IS NULL`, h.ID).Scan(&oldFrom, &oldRate, &oldCurrency, &oldCPUs, &oldMemory)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil {
			if ended != nil {
				return nil
			}
			if oldRate == rate.PerHour && oldCurrency == rate.Currency && oldCPUs == cpus && oldMemory == memory {
				return nil
			}
			var at time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
				return err
			}
			if !at.After(oldFrom) {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE host_rates SET valid_to=$2 WHERE host_id=$1 AND valid_to IS NULL`, h.ID, at); err != nil {
				return err
			}
			from = at
		} else {
			from = maxTime(from, fetched)
		}
		if ended != nil && !from.Before(*ended) {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,valid_to,per_hour,currency,cap_cpus,cap_memory,source)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (host_id,valid_from) DO NOTHING`, h.ID, from, ended, rate.PerHour, rate.Currency, cpus, memory, source)
		return err
	})
}
