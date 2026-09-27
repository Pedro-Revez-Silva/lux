package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	From                                     time.Time
	To                                       *time.Time
	Registered                               *time.Time
	CPUs                                     float64
	Memory                                   int64
}

func (s *Server) pricedHosts(ctx context.Context) ([]pricedHost, error) {
	var out []pricedHost
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, p.provider, coalesce(h.launch_template->>'region',''), coalesce(h.instance_type,''),
    coalesce(h.zone,''), coalesce(h.market,''), h.provision_requested_at, h.terminated_at, h.registered_at,
    coalesce((h.capacity->>'cpus')::float8,0), coalesce((h.capacity->>'memory')::int8,0)
    FROM hosts h JOIN pools p ON p.name = h.pool AND p.tenant_id IS NOT DISTINCT FROM h.tenant_id
    WHERE h.provision_requested_at IS NOT NULL AND h.provider_id IS NOT NULL AND p.provider <> 'static'
      AND (h.terminated_at IS NULL OR EXISTS (SELECT 1 FROM cost_sources cs
        JOIN placements pl ON pl.run_id = cs.run_id WHERE pl.host_id = h.id AND cs.source = 'compute' AND cs.status <> 'final'))
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

// priceLaunchedHost primes the on-demand cache at launch, before the runner
// has advertised capacity. A failed lookup does not hold up provisioning.
func (s *Server) priceLaunchedHost(ctx context.Context, provider string, template []byte, launched Launched) {
	if !s.cfg.Costs.Enabled || !s.cfg.Costs.ComputeEC2 || launched.Market != MarketOnDemand || launched.InstanceType == "" {
		return
	}
	var info struct {
		Region string `json:"region"`
	}
	if err := json.Unmarshal(template, &info); err != nil || info.Region == "" {
		return
	}
	if _, err := s.onDemandPrice(ctx, provider, info.Region, launched.InstanceType); err != nil && ctx.Err() == nil {
		s.log.Warn("costs: price at launch", "provider", provider, "type", launched.InstanceType, "err", err)
	}
}

// refreshPrices runs before a cost drain, outside its Run/queue locks. A
// provider failure leaves gaps and the next tick retries. Spot requests are
// shared by zone/type; a terminated host is retained while gaps may be filled.
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
		if h.Market != MarketOnDemand || h.To != nil {
			continue
		}
		rate, err := s.onDemandPrice(ctx, h.Provider, h.Region, h.Type)
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
		from, to := hs[0].From, time.Now()
		for _, h := range hs {
			if h.From.Before(from) {
				from = h.From
			}
		}
		history, err := p.SpotHistory(ctx, key.zone, key.kind, from, to)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("costs: spot history", "zone", key.zone, "type", key.kind, "err", err)
			}
			continue
		}
		slices.SortFunc(history, func(a, b SpotRate) int { return a.At.Compare(b.At) })
		for _, h := range hs {
			if err := s.applySpotHistory(ctx, h, history, to); err != nil && ctx.Err() == nil {
				s.log.Warn("costs: spot periods", "host", h.ID, "err", err)
			}
		}
	}
}

func (s *Server) applyCurrentPrice(ctx context.Context, h pricedHost, rate HourlyRate, source string) error {
	if h.CPUs <= 0 && h.Memory <= 0 {
		return nil
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var from time.Time
		var cpus float64
		var memory int64
		var registered *time.Time
		if err := tx.QueryRow(ctx, `SELECT provision_requested_at, registered_at, coalesce((capacity->>'cpus')::float8,0),
    coalesce((capacity->>'memory')::int8,0) FROM hosts WHERE id=$1 AND terminated_at IS NULL FOR UPDATE`, h.ID).
			Scan(&from, &registered, &cpus, &memory); err != nil {
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
		}
		_, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,per_hour,currency,cap_cpus,cap_memory,source)
    VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (host_id,valid_from) DO NOTHING`, h.ID, from, rate.PerHour, rate.Currency, cpus, memory, source)
		return err
	})
}

// applySpotHistory changes only the open period, or inserts uncovered gaps.
// Every closed row's price, capacity and bounds are immutable.
func (s *Server) applySpotHistory(ctx context.Context, h pricedHost, history []SpotRate, now time.Time) error {
	if len(history) == 0 {
		return nil
	}
	for i, r := range history {
		if err := validPrice(r.PerHour, r.Currency); err != nil || r.Currency == "" {
			return fmt.Errorf("invalid spot rate %q %q: %v", r.PerHour, r.Currency, err)
		}
		history[i].PerHour = moneyString(mustRat(r.PerHour))
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var from time.Time
		var ended, registered *time.Time
		var cpus float64
		var memory int64
		if err := tx.QueryRow(ctx, `SELECT provision_requested_at,terminated_at,registered_at,
    coalesce((capacity->>'cpus')::float8,0),coalesce((capacity->>'memory')::int8,0)
    FROM hosts WHERE id=$1 FOR UPDATE`, h.ID).Scan(&from, &ended, &registered, &cpus, &memory); err != nil {
			return err
		}
		if cpus <= 0 && memory <= 0 {
			return nil
		}
		if registered != nil {
			from = *registered
		}
		until := now
		if ended != nil && ended.Before(until) {
			until = *ended
		}
		if !from.Before(until) {
			return nil
		}
		type period struct {
			from   time.Time
			to     *time.Time
			rate   HourlyRate
			cpus   float64
			memory int64
		}
		rows, err := tx.Query(ctx, `SELECT valid_from,valid_to,trim_scale(per_hour)::text,currency,cap_cpus,cap_memory
    FROM host_rates WHERE host_id=$1 ORDER BY valid_from`, h.ID)
		if err != nil {
			return err
		}
		var periods []period
		for rows.Next() {
			var v period
			if err = rows.Scan(&v.from, &v.to, &v.rate.PerHour, &v.rate.Currency, &v.cpus, &v.memory); err != nil {
				break
			}
			periods = append(periods, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for i, r := range history {
			start := maxTime(from, r.At)
			end := until
			if i+1 < len(history) {
				end = minTime(end, history[i+1].At)
			}
			if !start.Before(end) {
				continue
			}
			// A history response lacking a price at/before the window start must
			// not be extrapolated backwards: leave that part missing.
			cursor := start
			for cursor.Before(end) {
				var covering *period
				next := end
				for j := range periods {
					v := &periods[j]
					if !v.from.After(cursor) && (v.to == nil || v.to.After(cursor)) {
						covering = v
						break
					}
					if v.from.After(cursor) && v.from.Before(next) {
						next = v.from
					}
				}
				if covering != nil {
					limit := end
					if covering.to != nil {
						limit = minTime(limit, *covering.to)
					}
					if covering.to == nil && (covering.rate != r.HourlyRate || covering.cpus != cpus || covering.memory != memory) && cursor.After(covering.from) {
						if _, err = tx.Exec(ctx, `UPDATE host_rates SET valid_to=$2 WHERE host_id=$1 AND valid_from=$3 AND valid_to IS NULL`, h.ID, cursor, covering.from); err != nil {
							return err
						}
						v := cursor
						covering.to = &v
						continue
					}
					cursor = limit
					continue
				}
				// A segment ending at now is open for a live host, so the next tick
				// can close it when it discovers a change. Historical gaps stay closed.
				var stop *time.Time
				if next.Before(end) {
					stop = &next
				} else if end.Before(now) || i+1 < len(history) || ended != nil {
					stop = &end
				}
				if _, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,valid_to,per_hour,currency,cap_cpus,cap_memory,source)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, h.ID, cursor, stop, r.PerHour, r.Currency, cpus, memory, h.Provider+"-spot-history"); err != nil {
					return err
				}
				periods = append(periods, period{cursor, stop, r.HourlyRate, cpus, memory})
				cursor = next
			}
		}
		return nil
	})
}
