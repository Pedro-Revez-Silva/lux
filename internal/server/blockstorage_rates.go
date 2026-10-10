package server

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// A provider host's block-storage rate (docs/costs.md, section 3) is one
// period over its whole billed window: its volumes exist from the launch
// request to termination and their size is fixed, so per_hour is the sum of
// their hourly list prices. Capacity is the host's, as for its compute
// periods, so placements share it alike. No borrowing between hosts: a host
// whose volumes are not known, or not priced, has no period (missing).

// bsHost is a provider host without a block-storage period whose volumes
// are known.
type bsHost struct {
	ID, Provider, Region string
	Volumes              []HostVolume
}

// blockStorageHosts are the hosts refreshBlockStorage prices: only those
// with capacity to share (as openBlockStorageRate needs), so a launch that
// never registered is not selected and locked on every pass; a live one is
// selected once it registers.
func (s *Server) blockStorageHosts(ctx context.Context) ([]bsHost, error) {
	var out []bsHost
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, p.provider, coalesce(h.launch_template->>'region', ''), h.volumes
			FROM hosts h JOIN pools p ON p.id = h.pool_id
			WHERE h.volumes IS NOT NULL AND jsonb_array_length(h.volumes) > 0
				AND h.provision_requested_at IS NOT NULL AND p.provider <> 'static'
				AND NOT EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = h.id AND r.family = 'block-storage')
				AND (coalesce((h.capacity->>'cpus')::float8, 0) > 0 OR coalesce((h.capacity->>'memory')::int8, 0) > 0
					OR EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = h.id AND r.family = 'compute'
						AND (r.cap_cpus > 0 OR r.cap_memory > 0)))
			ORDER BY h.id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[bsHost])
		return err
	})
	return out, err
}

// refreshBlockStorage opens the block-storage period of every host whose
// volumes became known, fetching each (provider, region, type)'s prices
// once per pass. A host with a type it cannot price stays without one.
func (s *Server) refreshBlockStorage(ctx context.Context) {
	hosts, err := s.blockStorageHosts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("costs: list block storage hosts", "err", err)
		}
		return
	}
	type key struct{ provider, region, kind string }
	type result struct {
		price BlockStoragePrice
		err   error
	}
	prices := map[key]result{}
	for _, h := range hosts {
		got := map[string]BlockStoragePrice{}
		var failed error
		for _, v := range h.Volumes {
			k := key{h.Provider, h.Region, v.Type}
			r, ok := prices[k]
			if !ok {
				r.price, r.err = s.blockStoragePrice(ctx, h.Provider, h.Region, v.Type)
				prices[k] = r
			}
			if r.err != nil {
				failed = r.err
				break
			}
			got[v.Type] = r.price
		}
		if failed == nil {
			failed = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				// The cost-host lock keeps a concurrent updateHostHours from
				// overwriting openBlockStorageRate's rewound cursor.
				runs, err := lockBlockStorageHost(ctx, tx, h.ID)
				if err != nil {
					return err
				}
				opened, err := openBlockStorageRate(ctx, tx, h.ID, got, h.Provider+"-ebs-pricing", s.cfg.Costs.Hourly)
				if err != nil || !opened {
					return err
				}
				// Runs already final on a host live when its volumes became
				// known gain their block storage as a backfill's do.
				skipped, err := requeueBlockStorageRuns(ctx, tx, runs)
				if len(skipped) > 0 {
					s.log.Warn("costs: block storage not added to Runs final without compute snapshots", "host", h.ID, "runs", skipped)
				}
				return err
			})
		}
		if failed != nil && ctx.Err() == nil {
			s.log.Warn("costs: block storage price", "host", h.ID, "err", failed)
		}
	}
}

// blockStorageHourly is volumes' summed hourly price, exact, in their one
// currency.
func blockStorageHourly(volumes []HostVolume, prices map[string]BlockStoragePrice) (*big.Rat, string, error) {
	total := new(big.Rat)
	currency := ""
	for _, v := range volumes {
		p, ok := prices[v.Type]
		if !ok {
			return nil, "", fmt.Errorf("no price for volume type %q", v.Type)
		}
		if currency != "" && p.Currency != currency {
			return nil, "", fmt.Errorf("volumes priced in %s and %s", currency, p.Currency)
		}
		currency = p.Currency
		h, err := volumeHourly(v, p)
		if err != nil {
			return nil, "", err
		}
		total.Add(total, h)
	}
	return total, currency, nil
}

// openBlockStorageRate writes hostID's block-storage period from its
// recorded volumes and the given unit prices, once: a host that already
// has one, has no known volumes, or no capacity to share (it never
// registered) gets none. It reports whether it wrote one. The host's
// host-hour cursor moves back to the period's start (within retention) so
// its unallocated rows are rebuilt with the new family. The caller holds
// the host's cost-host lock (lockBlockStorageHost).
func openBlockStorageRate(ctx context.Context, tx pgx.Tx, hostID string, prices map[string]BlockStoragePrice, source string, retention time.Duration) (bool, error) {
	var volumes *[]HostVolume
	var from time.Time
	var to *time.Time
	var cpus float64
	var memory int64
	err := tx.QueryRow(ctx, `SELECT h.volumes, h.provision_requested_at, h.terminated_at,
			coalesce(nullif(coalesce((h.capacity->>'cpus')::float8, 0), 0), c.cap_cpus, 0),
			coalesce(nullif(coalesce((h.capacity->>'memory')::int8, 0), 0), c.cap_memory, 0)
		FROM hosts h LEFT JOIN LATERAL (SELECT cap_cpus, cap_memory FROM host_rates r
			WHERE r.host_id = h.id AND r.family = 'compute' AND (r.cap_cpus > 0 OR r.cap_memory > 0)
			ORDER BY r.valid_from DESC LIMIT 1) c ON true
		WHERE h.id = $1 AND h.provision_requested_at IS NOT NULL FOR UPDATE OF h`, hostID).
		Scan(&volumes, &from, &to, &cpus, &memory)
	if err == pgx.ErrNoRows || (err == nil && (volumes == nil || len(*volumes) == 0 || (cpus <= 0 && memory <= 0))) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_rates WHERE host_id = $1 AND family = 'block-storage')`, hostID).Scan(&exists); err != nil || exists {
		return false, err
	}
	if to != nil && !to.After(from) {
		return false, nil
	}
	hourly, currency, err := blockStorageHourly(*volumes, prices)
	if err != nil {
		return false, err
	}
	used := map[string]BlockStoragePrice{}
	for _, v := range *volumes {
		used[v.Type] = prices[v.Type]
	}
	details := map[string]any{"volumes": *volumes, "prices": used, "hoursPerMonth": hoursPerMonth}
	if _, err := tx.Exec(ctx, `INSERT INTO host_rates (host_id, family, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source, details)
		VALUES ($1, 'block-storage', $2, $3, $4, $5, $6, $7, $8, $9)`,
		hostID, from, to, moneyString(hourly), currency, cpus, memory, source, details); err != nil {
		return false, err
	}
	return true, rewindHostHours(ctx, tx, hostID, from, retention)
}

// rewindHostHours moves hostID's host-hour cursor back to from's hour, or
// the oldest retained hour, so the refresh rebuilds those hours.
func rewindHostHours(ctx context.Context, tx pgx.Tx, hostID string, from time.Time, retention time.Duration) error {
	oldest := time.Now().UTC().Add(-retention).Truncate(time.Hour).Add(time.Hour)
	start := maxTime(from.UTC().Truncate(time.Hour), oldest)
	_, err := tx.Exec(ctx, `UPDATE cost_host_refresh SET next_hour = least(next_hour, $2), retry_at = NULL WHERE host_id = $1`, hostID, start)
	return err
}
