package server

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// The instance-type catalog: what each market candidate type is (vCPUs,
// memory, architectures, nested virtualization), cached in instance_types
// by the market sampler and refreshed daily (docs/telemetry.md).

// catalogRefresh is how old a catalog row may be before it is fetched again.
const catalogRefresh = 24 * time.Hour

// InstanceTypeInfo is an instance type as its provider describes it. A nil
// or empty field is one the provider did not give.
type InstanceTypeInfo struct {
	Name                 string
	VCPUs                *int
	MemoryBytes          int64
	Architectures        []string
	NestedVirtualization *bool
	Burstable            *bool
	NetworkBaselineGbps  *float64
	NetworkPerformance   string
}

// CatalogProvider is a MarketProvider that can describe instance types.
type CatalogProvider interface {
	InstanceTypes(ctx context.Context, region string, instanceTypes []string) ([]InstanceTypeInfo, error)
}

// refreshCatalog fetches, in one call, t's types with no catalog row or
// one older than catalogRefresh, and stores what the provider answers. The
// error is the database's, or errMarketLeaseLost; the provider's goes to
// problems.
func (s *Server) refreshCatalog(ctx context.Context, t *marketTarget, problems marketProblems) error {
	cp, ok := t.mp.(CatalogProvider)
	if !ok {
		return nil
	}
	fresh, err := s.marketStrings(ctx, `SELECT instance_type FROM instance_types
			WHERE provider = $1 AND region = $2 AND instance_type = ANY($3) AND fetched_at > now() - $4::interval`,
		t.provider, t.region, t.types, interval(catalogRefresh))
	if err != nil {
		return err
	}
	var stale []string
	for _, it := range t.types {
		if !slices.Contains(fresh, it) {
			stale = append(stale, it)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	infos, err := cp.InstanceTypes(ctx, t.region, stale)
	if err != nil {
		problems.add("instance types", err)
		return nil
	}
	if err := s.requireProvisionLease(ctx); err != nil {
		return err
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for _, in := range infos {
			if !slices.Contains(stale, in.Name) {
				continue
			}
			var mem *int64
			if in.MemoryBytes > 0 {
				mem = &in.MemoryBytes
			}
			if _, err := tx.Exec(ctx, `INSERT INTO instance_types (provider, region, instance_type, vcpus, memory_bytes, architectures,
					nested_virtualization, burstable, network_baseline_gbps, network_performance, fetched_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), clock_timestamp())
				ON CONFLICT (provider, region, instance_type) DO UPDATE SET vcpus = EXCLUDED.vcpus, memory_bytes = EXCLUDED.memory_bytes,
					architectures = EXCLUDED.architectures, nested_virtualization = EXCLUDED.nested_virtualization,
					burstable = EXCLUDED.burstable, network_baseline_gbps = EXCLUDED.network_baseline_gbps,
					network_performance = EXCLUDED.network_performance, fetched_at = EXCLUDED.fetched_at`,
				t.provider, t.region, in.Name, in.VCPUs, mem, in.Architectures, in.NestedVirtualization, in.Burstable,
				in.NetworkBaselineGbps, in.NetworkPerformance); err != nil {
				return err
			}
		}
		return nil
	})
}
