package server

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// volumeBatch bounds the instance ids of one Volumes call: EC2 accepts at
// most 200 values per filter.
const volumeBatch = 200

// recordVolumes asks each provider, once per region, for the volumes of its
// live hosts whose volumes are not known yet, and stores them. A failed call
// leaves those hosts NULL (block storage missing, retried next pass).
func (s *Server) recordVolumes(ctx context.Context) {
	type host struct {
		ID, ProviderID, Provider, Region string
		Template                         json.RawMessage
	}
	var hosts []host
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, h.provider_id, p.provider, coalesce(h.launch_template->>'region', ''),
				coalesce(h.launch_template, p.template)
			FROM hosts h JOIN pools p ON p.id = h.pool_id
			WHERE h.volumes IS NULL AND h.provider_id IS NOT NULL AND h.provision_requested_at IS NOT NULL
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
			s.log.Warn("volumes: list hosts", "err", err)
		}
		return
	}
	type key struct{ provider, region string }
	groups := map[key][]host{}
	for _, h := range hosts {
		k := key{h.Provider, h.Region}
		groups[k] = append(groups[k], h)
	}
	for _, k := range slices.SortedFunc(maps.Keys(groups), func(a, b key) int {
		return cmp.Or(strings.Compare(a.provider, b.provider), strings.Compare(a.region, b.region))
	}) {
		prov := s.cfg.Providers[k.provider]
		if prov == nil {
			continue
		}
		for batch := range slices.Chunk(groups[k], volumeBatch) {
			ids := make([]string, len(batch))
			for i, h := range batch {
				ids[i] = h.ProviderID
			}
			got, err := prov.Volumes(ctx, batch[0].Template, ids)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("volumes: provider", "provider", k.provider, "region", k.region, "err", err)
				}
				continue
			}
			for _, h := range batch {
				vols, ok := got[h.ProviderID]
				if !ok {
					continue
				}
				if err := s.storeVolumes(ctx, h.ID, vols); err != nil && ctx.Err() == nil {
					s.log.Warn("volumes: store", "host", h.ID, "err", err)
				}
			}
		}
	}
}

// storeVolumes records a host's volumes once: a value already there (one an
// operator assumed, or an earlier answer) is kept.
func (s *Server) storeVolumes(ctx context.Context, hostID string, vols []HostVolume) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET volumes = $2 WHERE id = $1 AND volumes IS NULL`, hostID, sortedVolumes(vols))
		return err
	})
}

// sortedVolumes is vols in a stable order (type, size, iops, throughput),
// never nil: the order a line's item names them in.
func sortedVolumes(vols []HostVolume) []HostVolume {
	out := slices.Clone(vols)
	if out == nil {
		out = []HostVolume{}
	}
	slices.SortFunc(out, func(a, b HostVolume) int {
		return cmp.Or(strings.Compare(a.Type, b.Type), cmp.Compare(a.SizeGiB, b.SizeGiB),
			cmp.Compare(a.IOPS, b.IOPS), cmp.Compare(a.ThroughputMiBps, b.ThroughputMiBps))
	})
	return out
}
