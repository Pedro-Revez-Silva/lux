package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Static hosts' network (docs/costs.md, section 2, "Static hosts"): no
// provider describes a static host's network or prices its transfer, so
// its operator does. hosts.network_static is the operator's catalog, the
// subnets that are local (same zone, free) and zone (billed both ways);
// with one, its NetworkClasses' Source is static, so traffic outside them
// is internet rather than unknown. hosts.transfer_prices are its per-GB
// prices and its public IPv4 hourly charge, copied from its pool's
// default when it first registers. A static host without a catalog has
// only local (its interface subnets, as the runner derives them) and
// unknown: nothing priced.

// StaticNetwork is a static host's operator-supplied catalog.
type StaticNetwork struct {
	Local []string `json:"local,omitempty" doc:"CIDRs in the host's own zone: free to reach."`
	Zone  []string `json:"zone,omitempty" doc:"CIDRs in other zones of the host's region: billed at transferPrices.zone."`
}

// HostNetworkSettings is PUT /v1/hosts/{id}/network's body and answer.
type HostNetworkSettings struct {
	Network        *StaticNetwork      `json:"network,omitempty" doc:"The catalog its runner classifies traffic by. Absent: none (only local, from the host's own subnets, and unknown)."`
	TransferPrices *DataTransferPrices `json:"transferPrices,omitempty" doc:"Per-GB transfer prices by class and direction, and publicIPv4PerHour (the host's whole public IPv4 charge per hour). Absent: nothing priced; its transfer is listed as unpriced (no-price)."`
}

// staticNetworkClasses are a static host's classes from its operator's
// catalog; nil (the runner derives Local itself) when it has none.
func staticNetworkClasses(n *StaticNetwork) *proto.NetworkClasses {
	if n == nil || (len(n.Local) == 0 && len(n.Zone) == 0) {
		return nil
	}
	nc := proto.NetworkClasses{Source: proto.NetSourceStatic, Local: n.Local, Zone: n.Zone}
	nc.Version = netClassesVersion(nc)
	return &nc
}

func validCIDRs(field string, list []string) error {
	for _, c := range list {
		if _, err := netip.ParsePrefix(strings.TrimSpace(c)); err != nil {
			return errf(http.StatusUnprocessableEntity, "invalid_network", "%s %q: not a CIDR", field, c)
		}
	}
	return nil
}

// validTransferPrices checks operator-supplied prices: a currency and
// non-negative decimals; provider keys valid region names.
func validTransferPrices(p *DataTransferPrices) error {
	if p == nil {
		return nil
	}
	if !currencyCode.MatchString(p.Currency) {
		return errf(http.StatusUnprocessableEntity, "invalid_price", "transferPrices.currency %q: want an ISO 4217 code such as USD", p.Currency)
	}
	for r := range p.Provider {
		if !proto.ValidNetworkRegion(r) {
			return errf(http.StatusUnprocessableEntity, "invalid_price", "transferPrices.provider: region %q", r)
		}
	}
	for k, v := range transferRows(*p) {
		if !costAmount.MatchString(v) || v[0] == '-' {
			return errf(http.StatusUnprocessableEntity, "invalid_price", "transferPrices %s %q: want a non-negative decimal with up to 9 fractional digits", k, v)
		}
	}
	return nil
}

type setHostNetworkInput struct {
	HostPath
	TenantQuery
	Body HostNetworkSettings
}

type hostNetworkOutput struct {
	Body struct {
		Host string `json:"host" doc:"The host's id."`
		HostNetworkSettings
	} `nameHint:"HostNetworkSet"`
}

// setHostNetwork replaces a static host's catalog and transfer prices (an
// absent field clears it). Its classes' version changes, so its runner
// fetches them at its next heartbeat; its public-ip period follows the new
// charge from now.
func (s *Server) setHostNetwork(ctx context.Context, in *setHostNetworkInput) (*hostNetworkOutput, error) {
	b := in.Body
	if b.Network != nil {
		if err := validCIDRs("network.local", b.Network.Local); err != nil {
			return nil, err
		}
		if err := validCIDRs("network.zone", b.Network.Zone); err != nil {
			return nil, err
		}
	}
	if b.TransferPrices != nil {
		t := trimmedDataTransfer(*b.TransferPrices)
		b.TransferPrices = &t
	}
	if err := validTransferPrices(b.TransferPrices); err != nil {
		return nil, err
	}
	p := principal(ctx)
	var id string
	// The permission and provider checks run before any lock, so a refused
	// request never queues behind (or holds up) the drainer; they are read
	// again under the host's row lock.
	check := func(tx pgx.Tx, lock string) error {
		var mine, provisioned bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(tenant_id = $2, false), provision_requested_at IS NOT NULL
			FROM hosts WHERE id = $1`+lock, id, p.TenantID).Scan(&mine, &provisioned); err != nil {
			return err
		}
		switch {
		case !mine && !p.Operator:
			return errf(http.StatusForbidden, "forbidden", "only operators set a platform host's network")
		case provisioned:
			return errf(http.StatusUnprocessableEntity, "invalid_network", "host %s was launched by its pool's provider, which describes and prices its network", in.ID)
		}
		return nil
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		if id, err = s.resolveHost(ctx, tx, p, in.ID, false); err != nil {
			return err
		}
		if err := check(tx, ""); err != nil {
			return err
		}
		runs, err := lockBlockStorageHost(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := check(tx, " FOR UPDATE"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET network_static = $2, transfer_prices = $3, net_classes = $4 WHERE id = $1`,
			id, b.Network, b.TransferPrices, staticNetworkClasses(b.Network)); err != nil {
			return err
		}
		first, err := syncStaticPublicIP(ctx, tx, id, false)
		if err != nil || first == nil {
			return err
		}
		return s.requeueHostRunsSince(ctx, tx, id, familyPublicIP, runs, *first)
	})
	if err != nil {
		return nil, err
	}
	out := &hostNetworkOutput{}
	out.Body.Host, out.Body.HostNetworkSettings = id, b
	return out, nil
}

// syncStaticPublicIP keeps a static host's public-ip period at its
// operator's charge (transfer_prices.publicIPv4PerHour): a changed charge
// closes the open period and opens one now; 0 or none opens none. The
// host's first period opens when it registered if it is registering (the
// charge came with it, from its pool), otherwise now: it returns that
// period's start, for the caller to rebuild the hours and Runs after it
// (requeueHostRunsSince). The caller holds the host's row.
func syncStaticPublicIP(ctx context.Context, tx pgx.Tx, hostID string, registering bool) (*time.Time, error) {
	// RETURNING's subquery sees host_rates as before the statement, so it
	// tells the host's first period from a reopened one.
	var from time.Time
	var first bool
	err := tx.QueryRow(ctx, `WITH at AS (SELECT clock_timestamp() AS at),
		want AS (SELECT h.id, (h.transfer_prices->>'publicIPv4PerHour')::numeric AS per_hour, h.transfer_prices->>'currency' AS currency,
				coalesce((h.capacity->>'cpus')::float8, 0) AS cpus, coalesce((h.capacity->>'memory')::int8, 0) AS memory, h.registered_at
			FROM hosts h WHERE h.id = $1 AND h.registered_at IS NOT NULL),
		closed AS (
			UPDATE host_rates r SET valid_to = (SELECT at FROM at) FROM want w
			WHERE r.host_id = w.id AND r.family = 'public-ip' AND r.valid_to IS NULL AND r.source = 'static'
				AND (w.per_hour IS DISTINCT FROM r.per_hour OR w.currency IS DISTINCT FROM r.currency
					OR w.cpus <> r.cap_cpus OR w.memory <> r.cap_memory)
			RETURNING r.host_id)
		INSERT INTO host_rates (host_id, family, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		SELECT w.id, 'public-ip', CASE WHEN $2 THEN w.registered_at ELSE (SELECT at FROM at) END,
			w.per_hour, w.currency, w.cpus, w.memory, 'static'
		FROM want w
		WHERE w.per_hour > 0 AND (w.cpus > 0 OR w.memory > 0)
			AND (EXISTS (SELECT 1 FROM closed)
				OR NOT EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = w.id AND r.family = 'public-ip' AND r.valid_to IS NULL))
		RETURNING valid_from, NOT EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = $1 AND r.family = 'public-ip')`,
		hostID, registering).Scan(&from, &first)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !first) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &from, nil
}

// ValidPoolTransferPrices checks a pool's default transfer prices: only a
// static pool has them (an ec2 pool's hosts are priced by the provider).
func ValidPoolTransferPrices(provider string, p *DataTransferPrices) error {
	if p == nil {
		return nil
	}
	if provider != "static" {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "transferPrices is for static pools: %s pools are priced by the provider", provider)
	}
	*p = trimmedDataTransfer(*p)
	return validTransferPrices(p)
}

// staticTransferPrices is a static host's operator-supplied transfer
// prices; nil when it has none.
func staticTransferPrices(ctx context.Context, tx pgx.Tx, hostID string) (*DataTransferPrices, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT transfer_prices FROM hosts WHERE id = $1`, hostID).Scan(&raw); err != nil || raw == nil {
		return nil, err
	}
	var p DataTransferPrices
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("host %s transfer prices: %w", hostID, err)
	}
	return &p, nil
}
