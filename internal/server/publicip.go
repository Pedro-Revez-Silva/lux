package server

import (
	"context"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// A host's public IPv4 addresses (docs/costs.md, section 2, "Public
// IPv4") cost a flat hourly charge each while attached, used or not, like
// its disk: one 'public-ip' host_rates period per address count, shared
// between its placements by their share. A provider host's count is
// hosts.network.publicIPv4, its charge the region's cached list price.

type publicIPHost struct {
	ID, Provider, Region string
}

// publicIPHosts are provider hosts with addresses and capacity whose
// public-ip period has never opened.
func (s *Server) publicIPHosts(ctx context.Context) ([]publicIPHost, error) {
	var out []publicIPHost
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, p.provider, coalesce(h.launch_template->>'region', '')
			FROM hosts h JOIN pools p ON p.id = h.pool_id
			WHERE h.provision_requested_at IS NOT NULL AND p.provider <> 'static' AND h.registered_at IS NOT NULL
				AND coalesce((h.network->>'publicIPv4')::int, 0) > 0
				AND NOT EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = h.id AND r.family = 'public-ip')
			ORDER BY h.id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[publicIPHost])
		return err
	})
	return out, err
}

// refreshPublicIP opens the first public-ip period of every provider host
// whose addresses became known, at its region's price (fetched once per
// pass). Later count changes are syncPublicIPRate's, from the provider
// check that records them.
func (s *Server) refreshPublicIP(ctx context.Context) {
	hosts, err := s.publicIPHosts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("costs: list public IPv4 hosts", "err", err)
		}
		return
	}
	type key struct{ provider, region string }
	// A region's price, nil after a failed fetch (logged once).
	prices := map[key]*DataTransferPrices{}
	for _, h := range hosts {
		k := key{h.Provider, h.Region}
		price, ok := prices[k]
		if !ok {
			p, err := s.dataTransferPrice(ctx, h.Provider, h.Region)
			if err == nil {
				price = &p
			} else if ctx.Err() == nil {
				s.log.Warn("costs: public IPv4 price", "provider", h.Provider, "region", h.Region, "err", err)
			}
			prices[k] = price
		}
		if price != nil {
			s.openPublicIP(ctx, h, *price)
		}
	}
}

// openPublicIP opens a provider host's first public-ip period at price
// with what opening a block-storage period takes: the host's Runs and
// cost-host lock first (lockBlockStorageHost), then, if it opened, the
// host's hours and Runs from its start rebuilt (requeueHostRunsSince).
func (s *Server) openPublicIP(ctx context.Context, h publicIPHost, price DataTransferPrices) {
	if price.PublicIPv4PerHour == "" {
		return
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		runs, err := lockBlockStorageHost(ctx, tx, h.ID)
		if err != nil {
			return err
		}
		first, err := syncPublicIPRate(ctx, tx, h.ID, price.PublicIPv4PerHour, price.Currency, h.Provider+"-vpc-pricing")
		if err != nil || first == nil {
			return err
		}
		return s.requeueHostRunsSince(ctx, tx, h.ID, familyPublicIP, runs, *first)
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("costs: public IPv4 period", "host", h.ID, "err", err)
	}
}

// syncPublicIPRate keeps hostID's public-ip periods in step with its
// address count (hosts.network.publicIPv4) at perAddress an hour. The
// first period, while the host has none, opens when its addresses became
// known (hosts.public_ip_from; its registration when they were known by
// then): it returns that start, and only a caller holding
// lockBlockStorageHost's locks may open it (openPublicIP).
// A count or price that differs from the open period's closes it and
// opens another at the same instant (none for zero addresses). A host with
// no recorded network, or no capacity to share, is left alone.
func syncPublicIPRate(ctx context.Context, tx pgx.Tx, hostID, perAddress, currency, source string) (*time.Time, error) {
	var count *int
	var registered, terminated *time.Time
	var cpus float64
	var memory int64
	err := tx.QueryRow(ctx, `SELECT (h.network->>'publicIPv4')::int, coalesce(h.public_ip_from, h.registered_at), h.terminated_at,
			coalesce(nullif(coalesce((h.capacity->>'cpus')::float8, 0), 0), c.cap_cpus, 0),
			coalesce(nullif(coalesce((h.capacity->>'memory')::int8, 0), 0), c.cap_memory, 0)
		FROM hosts h LEFT JOIN LATERAL (SELECT cap_cpus, cap_memory FROM host_rates r
			WHERE r.host_id = h.id AND r.family = 'compute' AND (r.cap_cpus > 0 OR r.cap_memory > 0)
			ORDER BY r.valid_from DESC LIMIT 1) c ON true
		WHERE h.id = $1`, hostID).Scan(&count, &registered, &terminated, &cpus, &memory)
	if err == pgx.ErrNoRows || (err == nil && (count == nil || registered == nil || (cpus <= 0 && memory <= 0))) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hourly := new(big.Rat).Mul(mustRat(perAddress), big.NewRat(int64(*count), 1))
	perHour := moneyString(hourly)
	details := map[string]any{"addresses": *count, "perAddressHour": trimmedDecimal(perAddress)}
	var known bool
	var openPerHour, openCurrency *string
	var openAddresses *int
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_rates WHERE host_id = $1 AND family = 'public-ip'),
			o.per_hour, o.currency, o.addresses
		FROM (SELECT 1) one LEFT JOIN (SELECT trim_scale(per_hour)::text AS per_hour, currency, (details->>'addresses')::int AS addresses
			FROM host_rates WHERE host_id = $1 AND family = 'public-ip' AND valid_to IS NULL) o ON true`, hostID).
		Scan(&known, &openPerHour, &openCurrency, &openAddresses); err != nil {
		return nil, err
	}
	openRate := openPerHour != nil
	if !known {
		if *count == 0 || (terminated != nil && !terminated.After(*registered)) {
			return nil, nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_rates (host_id, family, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source, details)
			VALUES ($1, 'public-ip', $2, $3, $4, $5, $6, $7, $8, $9)`,
			hostID, registered, terminated, perHour, currency, cpus, memory, source, details); err != nil {
			return nil, err
		}
		return registered, nil
	}
	if terminated != nil {
		return nil, nil
	}
	same := openRate && *openPerHour == perHour && *openCurrency == currency && openAddresses != nil && *openAddresses == *count
	if same || (!openRate && *count == 0) {
		return nil, nil
	}
	// The open period (if any) closes when the new one opens. The instant
	// is taken under the caller's lock (clock_timestamp(), not now()).
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_rates SET valid_to = $2
		WHERE host_id = $1 AND family = 'public-ip' AND valid_to IS NULL AND valid_from < $2`, hostID, at); err != nil {
		return nil, err
	}
	if *count == 0 {
		return nil, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO host_rates (host_id, family, valid_from, per_hour, currency, cap_cpus, cap_memory, source, details)
		VALUES ($1, 'public-ip', $2, $3, $4, $5, $6, $7, $8)`, hostID, at, perHour, currency, cpus, memory, source, details)
	return nil, err
}
