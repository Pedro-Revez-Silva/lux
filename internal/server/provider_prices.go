package server

import (
	"context"
	"fmt"
	"slices"
	"sync"
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

// Cursors avoid replaying long contiguous history on each tick. Entries for
// inactive servers and hosts expire, and the map is capped between refreshes.
var spotGapCursors sync.Map // spotGapKey -> spotGapCursor

type spotGapCursor struct {
	at   time.Time
	used time.Time
}

type spotGapKey struct {
	server *Server
	host   string
}

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

// refreshPrices runs independently of cost draining and provisioning. A
// provider failure leaves gaps for the next pass. Spot requests are shared by
// zone/type; terminated hosts remain eligible while compute is incomplete.
func (s *Server) refreshPrices(ctx context.Context) {
	if !s.cfg.Costs.Enabled || !s.cfg.Costs.ComputeEC2 {
		return
	}
	hosts, err := s.pricedHosts(ctx)
	if err != nil {
		s.log.Warn("costs: list priced hosts", "err", err)
		return
	}
	cursors := 0
	spotGapCursors.Range(func(k, v any) bool {
		if time.Since(v.(spotGapCursor).used) > time.Hour || cursors >= 4096 {
			spotGapCursors.Delete(k)
		} else {
			cursors++
		}
		return true
	})
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
	const spotWindow = 24 * time.Hour
	type spotWindowRequest struct {
		from, to time.Time
	}
	for key, hs := range groups {
		p := s.cfg.Costs.Prices[key.provider]
		if p == nil {
			continue
		}
		now := time.Now()
		var windows []spotWindowRequest
		recentWindows := map[string]spotWindowRequest{}
		for _, h := range hs {
			from := h.From
			if h.Registered != nil {
				from = *h.Registered
			}
			until := now
			if h.To != nil && h.To.Before(until) {
				until = *h.To
			}
			if !from.Before(until) {
				continue
			}
			gapFrom, gapTo, err := s.spotGap(ctx, h.ID, from, until)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("costs: spot gap", "host", h.ID, "err", err)
				}
				continue
			}
			recent := maxTime(from, until.Add(-spotWindow))
			if h.To != nil && recent.Before(until) {
				recentWindows[h.ID] = spotWindowRequest{recent, until}
			}
			if (h.To == nil || !gapFrom.Before(gapTo)) && recent.Before(until) {
				windows = append(windows, spotWindowRequest{recent, until})
			}
			if gapFrom.Before(gapTo) && (h.To != nil || gapFrom.Before(recent)) {
				windows = append(windows, spotWindowRequest{gapFrom, minTime(gapTo, gapFrom.Add(spotWindow))})
			}
		}
		slices.SortFunc(windows, func(a, b spotWindowRequest) int { return a.from.Compare(b.from) })
		merged := windows[:0]
		for _, w := range windows {
			if len(merged) > 0 && !w.from.After(merged[len(merged)-1].to) {
				merged[len(merged)-1].to = maxTime(merged[len(merged)-1].to, w.to)
			} else {
				merged = append(merged, w)
			}
		}
		covered := map[string]time.Time{}
		for id, recent := range recentWindows {
			covered[id] = recent.from
		}
		for _, w := range merged {
			for start := w.from; start.Before(w.to); {
				end := minTime(w.to, start.Add(spotWindow))
				history, err := p.SpotHistory(ctx, key.zone, key.kind, start, end)
				if err != nil {
					if ctx.Err() == nil {
						s.log.Warn("costs: spot history", "zone", key.zone, "type", key.kind, "err", err)
					}
				} else {
					slices.SortFunc(history, func(a, b SpotRate) int { return a.At.Compare(b.At) })
					for _, h := range hs {
						if err := s.applySpotHistory(ctx, h, history, start, end, now); err != nil {
							if ctx.Err() == nil {
								s.log.Warn("costs: spot periods", "host", h.ID, "err", err)
							}
							continue
						}
						if recent, ok := recentWindows[h.ID]; ok && !start.After(covered[h.ID]) && end.After(covered[h.ID]) &&
							len(history) > 0 && !history[0].At.After(covered[h.ID]) {
							covered[h.ID] = end
							if !end.Before(recent.to) {
								if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
									_, err := tx.Exec(ctx, `INSERT INTO spot_history_refresh (host_id, checked_at) VALUES ($1, $2)
										ON CONFLICT (host_id) DO UPDATE SET checked_at = greatest(spot_history_refresh.checked_at, EXCLUDED.checked_at)`, h.ID, now)
									return err
								}); err != nil && ctx.Err() == nil {
									s.log.Warn("costs: spot refresh checkpoint", "host", h.ID, "err", err)
								}
							}
						}
					}
				}
				start = end
			}
		}
	}
}

// spotGap checks at most 128 adjacent periods via the host/valid_from index.
// An uncovered interval is retried; contiguous history advances the cursor.
func (s *Server) spotGap(ctx context.Context, host string, from, until time.Time) (time.Time, time.Time, error) {
	key := spotGapKey{s, host}
	cursor := from
	if saved, ok := spotGapCursors.Load(key); ok {
		cursor = maxTime(cursor, saved.(spotGapCursor).at)
	}
	gapFrom, gapTo := until, until
	var advance time.Time
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var preceding *time.Time
		var precedingFrom time.Time
		err := tx.QueryRow(ctx, `SELECT valid_from, valid_to FROM host_rates
    WHERE host_id=$1 AND valid_from <= $2 ORDER BY valid_from DESC LIMIT 1`, host, cursor).Scan(&precedingFrom, &preceding)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil {
			if preceding == nil {
				return nil
			}
			cursor = maxTime(cursor, *preceding)
			advance = precedingFrom
		}
		lookup := cursor.Add(-time.Nanosecond)
		if !advance.IsZero() {
			lookup = advance
		}
		rows, err := tx.Query(ctx, `SELECT valid_from, valid_to FROM host_rates
    WHERE host_id=$1 AND valid_from > $2 AND valid_from < $3 ORDER BY valid_from LIMIT 128`, host, lookup, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var start time.Time
			var end *time.Time
			if err := rows.Scan(&start, &end); err != nil {
				return err
			}
			if cursor.Before(start) {
				gapFrom, gapTo = cursor, start
				return nil
			}
			if end == nil {
				advance = start
				return nil
			}
			cursor = maxTime(cursor, *end)
			advance = start
			count++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if count < 128 && cursor.Before(until) {
			gapFrom, gapTo = cursor, until
		}
		return nil
	})
	if err == nil && !advance.IsZero() {
		spotGapCursors.Store(key, spotGapCursor{advance, time.Now()})
	}
	return gapFrom, gapTo, err
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

// applySpotHistory changes only the open period, or inserts uncovered gaps.
// Every closed row's price, capacity and bounds are immutable.
func (s *Server) applySpotHistory(ctx context.Context, h pricedHost, history []SpotRate, windowFrom, windowTo, now time.Time) error {
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
		until := minTime(now, windowTo)
		if ended != nil && ended.Before(until) {
			until = *ended
		}
		from = maxTime(from, windowFrom)
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
    FROM host_rates WHERE host_id=$1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
    ORDER BY valid_from`, h.ID, from, until)
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
		periodIndex := 0
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
				for periodIndex < len(periods) && periods[periodIndex].to != nil && !periods[periodIndex].to.After(cursor) {
					periodIndex++
				}
				if periodIndex < len(periods) {
					v := &periods[periodIndex]
					if !v.from.After(cursor) {
						covering = v
					} else {
						next = minTime(next, v.from)
					}
				}
				if covering != nil {
					limit := end
					if covering.to != nil {
						limit = minTime(limit, *covering.to)
					}
					if covering.to == nil && cursor.Equal(covering.from) {
						cursor = limit
						continue
					}
					if covering.to == nil && (covering.rate != r.HourlyRate || covering.cpus != cpus || covering.memory != memory) {
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
				// Only a live host's current edge is open. A bounded historical
				// response cannot establish a price beyond its queried boundary.
				var stop *time.Time
				if next.Before(end) {
					stop = &next
				} else if (i+1 < len(history) && !history[i+1].At.After(until)) ||
					(ended != nil && until.Before(*ended)) ||
					(ended == nil && until.Before(now)) {
					stop = &end
				}
				if _, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id,valid_from,valid_to,per_hour,currency,cap_cpus,cap_memory,source)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, h.ID, cursor, stop, r.PerHour, r.Currency, cpus, memory, h.Provider+"-spot-history"); err != nil {
					return err
				}
				cursor = next
			}
		}
		return nil
	})
}
