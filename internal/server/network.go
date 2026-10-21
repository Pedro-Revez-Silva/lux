package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Network transfer classification (docs/telemetry.md, "Network transfer").
// The provider check stores each provider host's network placement
// (hosts.network) and the NetworkClasses built from it and the provider's
// published prefixes (hosts.net_classes); a host that registers without a
// catalog is discovered on the provisioner's next tick, not the next check.
// Until discovery stores any, a runner with CapProvisionalNet gets
// provisional classes built from its region's stored catalog
// (network_catalog). A runner gets them in its Welcome; each heartbeat's ack
// carries their Version, and a runner holding another fetches
// GET /runner/v1/network. No provider is called on hello or launch.

// networkClasses builds a host's lists: Local its own zone's subnets, Zone
// the rest of its network's, the provider lists as published. Version is a
// hash of the rest, so it changes exactly when a list (or the catalog's
// provenance) does.
func networkClasses(n HostNetwork, p ProviderPrefixes) proto.NetworkClasses {
	nc := proto.NetworkClasses{Source: cmp.Or(p.Source, proto.NetSourceNone),
		RegionService: p.RegionService, RegionPublic: p.RegionPublic, ProviderGlobal: p.ProviderGlobal,
		ProviderGlobalFree: p.ProviderGlobalFree, Provider: p.Provider}
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
	nc.Version = netClassesVersion(nc)
	return nc
}

// netClassesVersion hashes nc's lists and provenance (not its Version).
func netClassesVersion(nc proto.NetworkClasses) string {
	nc.Version = ""
	raw, _ := json.Marshal(nc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// privateRanges are a provisional catalog's Zone: before discovery, private
// traffic off the host's own subnets is most likely to its own network, and
// zone is the cautious middle between local (free) and internet.
var privateRanges = []string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// regionCatalog is the region half of p: the lists every host of one
// (provider, region) shares.
func regionCatalog(p ProviderPrefixes) proto.NetworkClasses {
	nc := networkClasses(HostNetwork{}, p)
	nc.Version = ""
	return nc
}

// provisionalLists are a new host's classes from its region's catalog
// (network_catalog), before its own network is discovered, without Version.
func provisionalLists(region proto.NetworkClasses) proto.NetworkClasses {
	region.Local, region.Zone, region.Provisional = nil, slices.Clone(privateRanges), true
	return region
}

// storeRegionCatalog keeps p as its (provider, region)'s network_catalog
// row. The version is read first, so an unchanged catalog (every pass)
// sends nothing more.
func (s *Server) storeRegionCatalog(ctx context.Context, provider, region string, p ProviderPrefixes) error {
	if p.Source == "" || p.Source == proto.NetSourceNone {
		return nil
	}
	rc := regionCatalog(p)
	version := netClassesVersion(provisionalLists(rc))
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var stored string
		err := tx.QueryRow(ctx, `SELECT version FROM network_catalog WHERE provider = $1 AND region = $2`, provider, region).Scan(&stored)
		if err == nil && stored == version || err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO network_catalog (provider, region, catalog, fetched_at, version)
			VALUES ($1, $2, $3, coalesce($4, now()), $5)
			ON CONFLICT (provider, region) DO UPDATE SET catalog = excluded.catalog, fetched_at = excluded.fetched_at, version = excluded.version
			WHERE network_catalog.version <> excluded.version`, provider, region, rc, rc.FetchedAt, version)
		return err
	})
}

// provisionalFrom are the joins and conditions that give a host provisional
// classes: a provider host with no stored classes at all, whose runner has
// CapProvisionalNet, in a (provider, region) with a network_catalog row.
// Stored classes, even Source none, are served as stored.
const provisionalFrom = ` FROM hosts h JOIN pools p ON p.id = h.pool_id
	JOIN network_catalog c ON c.provider = p.provider AND c.region = coalesce(h.launch_template->>'region', '')
	WHERE h.id = $1 AND h.provider_id IS NOT NULL AND h.provision_requested_at IS NOT NULL AND p.provider <> 'static'
		AND h.net_classes IS NULL
		AND '` + proto.CapProvisionalNet + `' = ANY(h.capabilities)`

// provisionalNetwork is hostID's provisional classes, nil if it has none.
func provisionalNetwork(ctx context.Context, tx pgx.Tx, hostID string) (*proto.NetworkClasses, error) {
	var region proto.NetworkClasses
	var version string
	err := tx.QueryRow(ctx, `SELECT c.catalog, c.version`+provisionalFrom, hostID).Scan(&region, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The stored version, so the heartbeat ack (provisionalVersion) needs
	// no catalog read and no hash.
	nc := provisionalLists(region)
	nc.Version = version
	return &nc, nil
}

// provisionalVersion is the Version provisionalNetwork would give, "" if
// none, without reading the catalog.
func provisionalVersion(ctx context.Context, tx pgx.Tx, hostID string) (string, error) {
	var version string
	err := tx.QueryRow(ctx, `SELECT c.version`+provisionalFrom, hostID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return version, err
}

// The narrow pass (recordNetwork with onlyMissing) discovers hosts that
// registered within netNewHostWindow and have no catalog, each at most once
// per netRetryEvery. Both bounds are needed. Without the spacing, a failing
// host costs a provider call on every 1s tick. Without the window, a host
// that can never get a catalog (DescribeSubnets denied gives Source none)
// costs one every 10s for life; past the window the full pass covers it.
const (
	netRetryEvery    = 10 * time.Second
	netNewHostWindow = 5 * time.Minute
)

type networkHost struct {
	ID, ProviderID, Provider, Region string
	Template                         json.RawMessage
}

// recordNetwork asks each provider, once per region, for the network of
// every live provider host, and stores what changed. Every pass, not only
// while unknown: an interface or address can be attached later. A failed
// prefix fetch with no copy yet still stores the host's subnets, with
// Source none (unlisted traffic is then unknown, never internet). Each
// region's catalog is also kept in network_catalog; the full pass fetches
// it for pools with no live host too.
//
// onlyMissing limits the pass to newly registered hosts with no catalog
// (above), so a new host's traffic is classified a tick after it registers
// rather than at the next provider check. With none, it is one query.
func (s *Server) recordNetwork(ctx context.Context, onlyMissing bool) {
	type host = networkHost
	query := `SELECT h.id, h.provider_id, p.provider, coalesce(h.launch_template->>'region', ''),
			coalesce(h.launch_template, p.template)
		FROM hosts h JOIN pools p ON p.id = h.pool_id
		WHERE h.provider_id IS NOT NULL AND h.provision_requested_at IS NOT NULL
			AND h.state <> 'terminated' AND p.provider <> 'static'`
	var args []any
	if onlyMissing {
		query += ` AND h.registered_at > now() - make_interval(secs => $1)
			AND coalesce(h.net_classes->>'source', '') IN ('', $2)`
		args = append(args, netNewHostWindow.Seconds(), proto.NetSourceNone)
	}
	var hosts []host
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query+` ORDER BY h.id`, args...)
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
	if onlyMissing {
		now := s.now()
		maps.DeleteFunc(s.netTried, func(_ string, at time.Time) bool { return now.Sub(at) >= netRetryEvery })
		hosts = slices.DeleteFunc(hosts, func(h host) bool { _, recent := s.netTried[h.ID]; return recent })
		if len(hosts) == 0 {
			return
		}
		// Pool reconciles may have outlasted the lease; the full pass's
		// caller checks it, the narrow pass only when it will call out.
		if ok, _ := s.holdProvisionLease(ctx); !ok {
			return
		}
		if s.netTried == nil {
			s.netTried = map[string]time.Time{}
		}
		for _, h := range hosts {
			s.netTried[h.ID] = now
		}
	}
	groups := map[[2]string][]host{}
	for _, h := range hosts {
		k := [2]string{h.Provider, h.Region}
		groups[k] = append(groups[k], h)
	}
	if !onlyMissing {
		s.warmRegionCatalogs(ctx, groups)
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
		if err == nil {
			if err := s.storeRegionCatalog(ctx, k[0], k[1], prefixes); err != nil && ctx.Err() == nil {
				s.log.Warn("network: store region catalog", "provider", k[0], "region", k[1], "err", err)
			}
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
				if err := s.storeNetworkRefresh(ctx, h.ID, h.Provider, h.Region, n, prefixes); err != nil && ctx.Err() == nil {
					s.log.Warn("network: store", "host", h.ID, "err", err)
				}
			}
		}
	}
}

// warmRegionCatalogs fetches Prefixes for every (provider, region) of a
// non-retired pool that covered (recordNetwork's own groups) leaves out,
// so a region's network_catalog row exists before its first host registers.
func (s *Server) warmRegionCatalogs(ctx context.Context, covered map[[2]string][]networkHost) {
	type pool struct {
		Provider, Region string
		Template         json.RawMessage
	}
	var pools []pool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (provider, coalesce(template->>'region', ''))
				provider, coalesce(template->>'region', ''), template
			FROM pools WHERE NOT retired AND provider <> 'static'
			ORDER BY provider, coalesce(template->>'region', ''), id`)
		if err != nil {
			return err
		}
		pools, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pool])
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("network: list pool regions", "err", err)
		}
		return
	}
	for _, pl := range pools {
		prov := s.cfg.Providers[pl.Provider]
		if _, ok := covered[[2]string{pl.Provider, pl.Region}]; ok || prov == nil {
			continue
		}
		prefixes, err := prov.Prefixes(ctx, pl.Template)
		if err == nil {
			err = s.storeRegionCatalog(ctx, pl.Provider, pl.Region, prefixes)
		}
		if err != nil && ctx.Err() == nil {
			s.log.Warn("network: region catalog", "provider", pl.Provider, "region", pl.Region, "err", err)
		}
	}
}

// storeNetworkRefresh writes a host's network and classes when either
// changed. A changed public IPv4 count opens a new public-ip period at the
// region's cached price; a host's first is openPublicIP's, from when its
// addresses were first recorded here (hosts.public_ip_from) if that was
// after it registered.
func (s *Server) storeNetworkRefresh(ctx context.Context, hostID, provider, region string, n HostNetwork, prefixes ProviderPrefixes) error {
	var first bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		first = false
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
			prefixes.ProviderGlobal, prefixes.ProviderGlobalFree, prefixes.Provider = previous.ProviderGlobal, previous.ProviderGlobalFree, previous.Provider
			if previous.FetchedAt != nil {
				prefixes.FetchedAt = *previous.FetchedAt
			}
		}
		n.SubnetCIDRs, n.VPCSubnets = nonNil(n.SubnetCIDRs), nonNil(n.VPCSubnets)
		nc := networkClasses(n, prefixes)
		tag, err := tx.Exec(ctx, `UPDATE hosts SET network=$2, net_classes=$3 WHERE id=$1 AND (network IS DISTINCT FROM $2::jsonb OR net_classes IS DISTINCT FROM $3::jsonb)`, hostID, n, nc)
		if err != nil || tag.RowsAffected() == 0 || (old != nil && old.PublicIPv4 == n.PublicIPv4) {
			return err
		}
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_rates WHERE host_id = $1 AND family = 'public-ip')`, hostID).Scan(&known); err != nil {
			return err
		}
		if !known {
			// Addresses appearing on a registered host (recorded late, or
			// attached after launch) are charged from now, not from its
			// registration.
			first = n.PublicIPv4 > 0
			if first && (old == nil || old.PublicIPv4 == 0) {
				_, err = tx.Exec(ctx, `UPDATE hosts SET public_ip_from = clock_timestamp() WHERE id = $1 AND registered_at IS NOT NULL`, hostID)
			}
			return err
		}
		price, _, err := cachedDataTransfer(ctx, tx, provider, region)
		if err != nil || price.PublicIPv4PerHour == "" {
			return err
		}
		_, err = syncPublicIPRate(ctx, tx, hostID, price.PublicIPv4PerHour, price.Currency, provider+"-vpc-pricing")
		return err
	})
	if err != nil || !first {
		return err
	}
	var price DataTransferPrices
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		price, _, err = cachedDataTransfer(ctx, tx, provider, region)
		return err
	}); err != nil {
		return err
	}
	s.openPublicIP(ctx, publicIPHost{ID: hostID, Provider: provider, Region: region}, price)
	return nil
}

// hostNetwork is a host's classes (nil: none known, the runner derives
// Local itself) and its own traffic as last recorded. A host with no stored
// classes gets provisional ones when it qualifies (provisionalFrom); they
// are never stored, so net_classes stays what discovery found.
func (s *Server) hostNetwork(ctx context.Context, tx pgx.Tx, hostID string) (*proto.NetworkClasses, map[string]proto.NetBytes, error) {
	var nc *proto.NetworkClasses
	var net map[string]proto.NetBytes
	if err := tx.QueryRow(ctx, `SELECT net_classes, host_net_classes FROM hosts WHERE id = $1`, hostID).Scan(&nc, &net); err != nil {
		return nil, nil, err
	}
	if nc == nil {
		var err error
		if nc, err = provisionalNetwork(ctx, tx, hostID); err != nil {
			return nil, nil, err
		}
	}
	return nc, net, nil
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
