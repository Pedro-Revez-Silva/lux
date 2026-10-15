package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Network transfer classification (docs/telemetry.md, "Network transfer").
// The provider check stores each provider host's network placement
// (hosts.network) and the NetworkClasses built from it and the provider's
// published prefixes (hosts.net_classes). A runner gets them in its Welcome;
// each heartbeat's ack carries their Version, and a runner holding another
// fetches GET /runner/v1/network. No provider is called on hello or launch.

// networkClasses builds a host's lists: Local its own zone's subnets, Zone
// the rest of its network's, the provider lists as published. Version is a
// hash of the rest, so it changes exactly when a list (or the catalog's
// provenance) does.
func networkClasses(n HostNetwork, p ProviderPrefixes) proto.NetworkClasses {
	nc := proto.NetworkClasses{Source: cmp.Or(p.Source, proto.NetSourceNone),
		RegionService: p.RegionService, RegionPublic: p.RegionPublic, ProviderGlobal: p.ProviderGlobal, Provider: p.Provider}
	if !p.FetchedAt.IsZero() {
		at := p.FetchedAt.UTC()
		nc.FetchedAt = &at
	}
	for _, sn := range n.VPCSubnets {
		if sn.Zone == n.Zone {
			nc.Local = append(nc.Local, sn.CIDR)
		} else if !n.SubnetsUnavailable {
			nc.Zone = append(nc.Zone, sn.CIDR)
		}
	}
	// A subnet list without the host's own (a stale cache) still makes its
	// own subnets local.
	for _, c := range n.SubnetCIDRs {
		if !slices.Contains(nc.Local, c) {
			nc.Local = append(nc.Local, c)
		}
	}
	slices.Sort(nc.Local)
	slices.Sort(nc.Zone)
	raw, _ := json.Marshal(nc)
	sum := sha256.Sum256(raw)
	nc.Version = hex.EncodeToString(sum[:12])
	return nc
}

// recordNetwork asks each provider, once per region, for the network of
// every live provider host, and stores what changed. Every pass, not only
// while unknown: an interface or address can be attached later. A failed
// prefix fetch with no copy yet still stores the host's subnets, with
// Source none (unlisted traffic is then unknown, never internet).
func (s *Server) recordNetwork(ctx context.Context) {
	type host struct {
		ID, ProviderID, Provider, Region string
		Template                         json.RawMessage
	}
	var hosts []host
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, h.provider_id, p.provider, coalesce(h.launch_template->>'region', ''),
				coalesce(h.launch_template, p.template)
			FROM hosts h JOIN pools p ON p.id = h.pool_id
			WHERE h.provider_id IS NOT NULL AND h.provision_requested_at IS NOT NULL
				AND h.state <> 'terminated' AND p.provider <> 'static'
			ORDER BY h.id`)
		if err != nil {
			return err
		}
		hosts, err = pgx.CollectRows(rows, pgx.RowToStructByPos[host])
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("network: list hosts", "err", err)
		}
		return
	}
	groups := map[[2]string][]host{}
	for _, h := range hosts {
		k := [2]string{h.Provider, h.Region}
		groups[k] = append(groups[k], h)
	}
	for _, k := range slices.SortedFunc(maps.Keys(groups), func(a, b [2]string) int {
		return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1]))
	}) {
		prov := s.cfg.Providers[k[0]]
		if prov == nil {
			continue
		}
		prefixes, err := prov.Prefixes(ctx, groups[k][0].Template)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("network: provider prefixes", "provider", k[0], "region", k[1], "err", err)
		}
		for batch := range slices.Chunk(groups[k], volumeBatch) {
			ids := make([]string, len(batch))
			for i, h := range batch {
				ids[i] = h.ProviderID
			}
			got, err := prov.Network(ctx, batch[0].Template, ids)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("network: provider", "provider", k[0], "region", k[1], "err", err)
				}
				continue
			}
			for _, h := range batch {
				n, ok := got[h.ProviderID]
				if !ok {
					continue
				}
				if err := s.storeNetworkRefresh(ctx, h.ID, n, prefixes); err != nil && ctx.Err() == nil {
					s.log.Warn("network: store", "host", h.ID, "err", err)
				}
			}
		}
	}
}

func (s *Server) storeNetworkRefresh(ctx context.Context, hostID string, n HostNetwork, prefixes ProviderPrefixes) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var old *HostNetwork
		var previous *proto.NetworkClasses
		// The stored catalog is only needed to stand in for a failed prefix fetch.
		if err := tx.QueryRow(ctx, `SELECT network, CASE WHEN $2 THEN net_classes END FROM hosts WHERE id=$1 FOR NO KEY UPDATE`,
			hostID, prefixes.Source == "").Scan(&old, &previous); err != nil {
			return err
		}
		switch {
		case n.SubnetsUnavailable:
			// Partial discovery: the last known subnets, and no catalog, so
			// unlisted traffic is unknown rather than internet.
			if old != nil {
				n.SubnetCIDRs, n.VPCSubnets = old.SubnetCIDRs, old.VPCSubnets
			}
			prefixes = ProviderPrefixes{}
		case prefixes.Source == "" && previous != nil && previous.Catalog():
			prefixes.Source, prefixes.RegionService, prefixes.RegionPublic = previous.Source, previous.RegionService, previous.RegionPublic
			prefixes.ProviderGlobal, prefixes.Provider = previous.ProviderGlobal, previous.Provider
			if previous.FetchedAt != nil {
				prefixes.FetchedAt = *previous.FetchedAt
			}
		}
		n.SubnetCIDRs, n.VPCSubnets = nonNil(n.SubnetCIDRs), nonNil(n.VPCSubnets)
		nc := networkClasses(n, prefixes)
		_, err := tx.Exec(ctx, `UPDATE hosts SET network=$2, net_classes=$3 WHERE id=$1 AND (network IS DISTINCT FROM $2::jsonb OR net_classes IS DISTINCT FROM $3::jsonb)`, hostID, n, nc)
		return err
	})
}

// hostNetwork is a host's stored classes (nil: none known, the runner
// derives Local itself) and its own traffic as last recorded.
func (s *Server) hostNetwork(ctx context.Context, tx pgx.Tx, hostID string) (*proto.NetworkClasses, map[string]proto.NetBytes, error) {
	var nc *proto.NetworkClasses
	var net map[string]proto.NetBytes
	err := tx.QueryRow(ctx, `SELECT net_classes, host_net_classes FROM hosts WHERE id = $1`, hostID).Scan(&nc, &net)
	return nc, net, err
}

// serveNetwork is GET /runner/v1/network?name=<host>: the host's classes,
// for a runner whose heartbeat ack named a version it does not hold.
func (s *Server) serveNetwork(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	var nc *proto.NetworkClasses
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM hosts WHERE token_id = $1 AND name = $2 AND state <> 'terminated'`,
			tok.ID, r.URL.Query().Get("name")).Scan(&id); err != nil {
			return err
		}
		nc, _, err = s.hostNetwork(r.Context(), tx, id)
		return err
	})
	if err != nil {
		return errf(http.StatusConflict, "hello_required", "send hello first")
	}
	if nc == nil {
		nc = &proto.NetworkClasses{Source: proto.NetSourceNone}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(nc)
}
