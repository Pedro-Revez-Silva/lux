package server

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
)

// Network transfer cost (docs/costs.md, section 2, "Data transfer"): a
// placement's bytes per billing class and direction (proto.NetBytes, as
// its runner counted them) times the per-GB list price of that class.
// Unlike compute it is not shared: the bytes are the placement's own.

const (
	familyDataTransfer = "data-transfer"
	familyPublicIP     = "public-ip"
)

// bytesPerGB: providers bill transfer per decimal gigabyte (AWS: "per GB",
// 10^9 bytes), not per GiB.
const bytesPerGB = 1_000_000_000

// Why bytes are not priced (transferUnpriced.Reason).
const (
	// unpricedUnclassified: class unknown, off the host's subnets with no
	// provider catalog to tell the provider from the internet.
	unpricedUnclassified = "unclassified"
	// unpricedPartialNetwork: the host's network discovery was partial
	// (hosts.network.partial), so its classes cannot be trusted.
	unpricedPartialNetwork = "partial-network"
	// unpricedNoPrice: a billable class with no price (no price known for
	// the host's region yet, an inter-region destination the price list
	// lacks, a static host without that class's price).
	unpricedNoPrice = "no-price"
)

type transferItem struct {
	Class     string `json:"class"`
	Direction string `json:"direction"`
	Bytes     int64  `json:"bytes"`
	PerGB     string `json:"perGB"`
	Amount    string `json:"amount"`
}

type transferUnpriced struct {
	Class  string `json:"class"`
	Reason string `json:"reason"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
}

type transferHour struct {
	Hour   time.Time `json:"hour"`
	Amount string    `json:"amount"`
}

// billedRx is a class's inbound bytes as priced: those the provider
// delivered (RxDelivered, counted ahead of every filter, dropped packets
// included, since the provider bills them) or, where that counter started
// later than rx (a runner upgraded mid-placement) or is absent (an older
// runner), the accepted bytes, a lower bound.
func billedRx(b proto.NetBytes) int64 { return max(b.RxDelivered, b.Rx) }

// transferClassFree: classes the provider does not charge for, which
// produce no line (not a zero line): same-zone private traffic, the
// region's free services, and CloudFront.
func transferClassFree(class string) bool {
	switch proto.NetClass(class) {
	case proto.NetLocal, proto.NetRegionService, proto.NetProviderGlobalFree:
		return true
	}
	return false
}

// transferPerGB is the per-GB price of key (a proto.NetKey) in direction
// dir ("rx" or "tx"); ok false when p has none for the class (a static
// host's operator priced other classes only, an inter-region destination
// the price list lacks). Within a priced class a missing direction, or
// "0", is not charged.
func transferPerGB(p DataTransferPrices, key, dir string) (string, bool) {
	class, region, _ := strings.Cut(key, ":")
	var g PerGB
	switch proto.NetClass(class) {
	case proto.NetLocal, proto.NetRegionService, proto.NetProviderGlobalFree:
		return "0", true
	case proto.NetZone:
		g = p.Zone
	case proto.NetRegionPublic:
		g = p.RegionPublic
	// Every GLOBAL prefix but CloudFront's (Route 53, Global Accelerator,
	// AMAZON) is billed as transfer out to the internet.
	case proto.NetProviderGlobal, proto.NetInternet:
		g = p.Internet
	case proto.NetProvider:
		g = p.Provider[region]
	default:
		return "", false
	}
	if p.Currency == "" || g == (PerGB{}) {
		return "", false
	}
	v := g.Tx
	if dir == "rx" {
		v = g.Rx
	}
	return cmp.Or(v, "0"), true
}

// priceTransfer prices net at p (nil: no price known). Items are the
// charged (class, direction) pairs, sorted; unpriced the bytes no price
// applies to, by class and reason. partial: the host's discovery was
// partial, and nothing is priced.
func priceTransfer(net map[string]proto.NetBytes, p *DataTransferPrices, partial bool) ([]transferItem, []transferUnpriced, *big.Rat) {
	total := new(big.Rat)
	var items []transferItem
	var unpriced []transferUnpriced
	for _, key := range slices.Sorted(maps.Keys(net)) {
		b := net[key]
		rx, tx := billedRx(b), b.Tx
		if rx == 0 && tx == 0 {
			continue
		}
		class, _, _ := strings.Cut(key, ":")
		switch {
		case transferClassFree(class):
			continue
		case partial:
			unpriced = append(unpriced, transferUnpriced{Class: key, Reason: unpricedPartialNetwork, Rx: rx, Tx: tx})
			continue
		case class == string(proto.NetUnknown):
			unpriced = append(unpriced, transferUnpriced{Class: key, Reason: unpricedUnclassified, Rx: rx, Tx: tx})
			continue
		}
		missing := transferUnpriced{Class: key, Reason: unpricedNoPrice}
		for _, d := range []struct {
			dir      string
			bytes    int64
			unpriced *int64
		}{{"rx", rx, &missing.Rx}, {"tx", tx, &missing.Tx}} {
			if d.bytes == 0 {
				continue
			}
			var perGB string
			ok := false
			if p != nil {
				perGB, ok = transferPerGB(*p, key, d.dir)
			}
			if !ok {
				*d.unpriced = d.bytes
				continue
			}
			rate := mustRat(perGB)
			if rate.Sign() == 0 {
				continue
			}
			amount := new(big.Rat).Mul(big.NewRat(d.bytes, bytesPerGB), rate)
			total.Add(total, amount)
			items = append(items, transferItem{Class: key, Direction: d.dir, Bytes: d.bytes, PerGB: trimmedDecimal(perGB), Amount: moneyString(amount)})
		}
		if missing.Rx != 0 || missing.Tx != 0 {
			unpriced = append(unpriced, missing)
		}
	}
	return items, unpriced, total
}

// netSample is a placement's cumulative counters at a sample.
type netSample struct {
	At  time.Time
	Net map[string]proto.NetBytes
}

// netGrowth is each key's billed growth from cumulative counters from to
// to (none where it did not grow), as Tx and Rx (billedRx's).
func netGrowth(from, to map[string]proto.NetBytes) map[string]proto.NetBytes {
	delta := map[string]proto.NetBytes{}
	for k, v := range to {
		b := from[k]
		d := proto.NetBytes{Tx: max(0, v.Tx-b.Tx), Rx: max(0, billedRx(v)-billedRx(b))}
		if d.Tx > 0 || d.Rx > 0 {
			delta[k] = d
		}
	}
	return delta
}

// transferHours spreads a placement's transfer over the UTC hours its
// bytes moved in: the difference of its cumulative counters between
// samples, at p's prices; whatever the last sample did not see (final's
// excess) goes to end's hour. Each hour is rounded cumulatively, so the
// hours add up to the placement's rounded amount. partial is priceTransfer's:
// a partial host's hours price nothing, as its lines do.
func transferHours(samples []netSample, final map[string]proto.NetBytes, end time.Time, p *DataTransferPrices, partial bool) []transferHour {
	if p == nil {
		return nil
	}
	cum := map[string]proto.NetBytes{}
	byHour := map[time.Time]*big.Rat{}
	add := func(hour time.Time, next map[string]proto.NetBytes) {
		_, _, amount := priceTransfer(netGrowth(cum, next), p, partial)
		for k, v := range next {
			c := cum[k]
			cum[k] = proto.NetBytes{Tx: max(c.Tx, v.Tx), Rx: max(billedRx(c), billedRx(v))}
		}
		if amount.Sign() == 0 {
			return
		}
		if byHour[hour] == nil {
			byHour[hour] = new(big.Rat)
		}
		byHour[hour].Add(byHour[hour], amount)
	}
	for _, s := range samples {
		add(s.At.UTC().Truncate(time.Hour), s.Net)
	}
	add(end.UTC().Truncate(time.Hour), final)
	out := make([]transferHour, 0, len(byHour))
	running, previous := new(big.Rat), new(big.Rat)
	for _, h := range slices.SortedFunc(maps.Keys(byHour), time.Time.Compare) {
		running.Add(running, byHour[h])
		part := mustRat(moneyString(running))
		out = append(out, transferHour{Hour: h, Amount: moneyString(new(big.Rat).Sub(part, previous))})
		previous = part
	}
	return out
}

// transferSnapshot is a placement and its data-transfer snapshot.
type transferSnapshot struct {
	id, hostID       string
	epoch            int
	from             time.Time
	to               *time.Time
	net              map[string]proto.NetBytes
	providerHost     bool
	provider, region string
	partial          bool
	currency, amount *string
	items            []transferItem
	hours            []transferHour
	unpriced         []transferUnpriced
	final            bool
}

// placementTransferPrices is what a placement's host transfer is priced
// at: a provider host's region's cached list prices (refreshed by the
// price loop, never fetched here), or nil when none is cached yet.
func placementTransferPrices(ctx context.Context, tx pgx.Tx, v *transferSnapshot) (*DataTransferPrices, error) {
	if !v.providerHost {
		return staticTransferPrices(ctx, tx, v.hostID)
	}
	if v.region == "" {
		return nil, nil
	}
	p, _, err := cachedDataTransfer(ctx, tx, v.provider, v.region)
	if err != nil || p.Currency == "" {
		return nil, err
	}
	return &p, nil
}

// needsPrice: net has bytes in a class that a price list prices.
func needsPrice(net map[string]proto.NetBytes) bool {
	for k, b := range net {
		class, _, _ := strings.Cut(k, ":")
		if (billedRx(b) > 0 || b.Tx > 0) && !transferClassFree(class) && class != string(proto.NetUnknown) {
			return true
		}
	}
	return false
}

// writeTransferSnapshots refreshes a Run's unfrozen data-transfer
// snapshots and returns its data-transfer lines; their hours go to
// e.Hours. A provider host's placement with billable bytes and no cached
// price is missing (retried); bytes no price applies to are recorded as
// unpriced, never as zero.
func writeTransferSnapshots(ctx context.Context, tx pgx.Tx, runID string, e *computeEval, now time.Time) ([]costReport, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, p.epoch, p.created_at, p.ended_at, p.host_id, coalesce(p.net_classes, '{}'),
			h.provision_requested_at IS NOT NULL AND coalesce(pl.provider, '') <> 'static', coalesce(pl.provider, ''),
			coalesce(h.launch_template->>'region', ''), coalesce((h.network->>'partial')::boolean, false),
			s.currency, s.amount::text, coalesce(s.items, '[]'), coalesce(s.hours, '[]'), coalesce(s.unpriced, '[]'), coalesce(s.finalized, false)
		FROM placements p JOIN hosts h ON h.id = p.host_id LEFT JOIN pools pl ON pl.id = h.pool_id
		LEFT JOIN cost_transfer_snapshots s ON s.placement_id = p.id
		WHERE p.run_id = $1 ORDER BY p.created_at, p.id`, runID)
	if err != nil {
		return nil, err
	}
	var entries []transferSnapshot
	for rows.Next() {
		var v transferSnapshot
		if err := rows.Scan(&v.id, &v.epoch, &v.from, &v.to, &v.hostID, &v.net, &v.providerHost, &v.provider,
			&v.region, &v.partial, &v.currency, &v.amount, &v.items, &v.hours, &v.unpriced, &v.final); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		v := &entries[i]
		if v.final {
			continue
		}
		end := now
		if v.to != nil {
			end = *v.to
		}
		prices, err := placementTransferPrices(ctx, tx, v)
		if err != nil {
			return nil, err
		}
		items, unpriced, total := priceTransfer(v.net, prices, v.partial)
		missing := prices == nil && v.providerHost && !v.partial && needsPrice(v.net)
		if missing {
			e.Missing = append(e.Missing, fmt.Sprintf("placement %s on host %s has no %s price", v.id, v.hostID, familyDataTransfer))
		}
		v.items, v.unpriced, v.hours = items, unpriced, nil
		v.currency, v.amount = nil, nil
		if prices != nil {
			a := moneyString(total)
			v.currency, v.amount = &prices.Currency, &a
			samples, err := placementNetSamples(ctx, tx, runID, v.epoch)
			if err != nil {
				return nil, err
			}
			v.hours = transferHours(samples, v.net, end, prices, v.partial)
		}
		v.final = v.to != nil && !missing
		if _, err := tx.Exec(ctx, `INSERT INTO cost_transfer_snapshots
				(placement_id, run_id, tenant_id, host_id, currency, amount, items, hours, unpriced, priced_to, finalized)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11)
			ON CONFLICT (placement_id) DO UPDATE SET currency = EXCLUDED.currency, amount = EXCLUDED.amount,
				items = EXCLUDED.items, hours = EXCLUDED.hours, unpriced = EXCLUDED.unpriced,
				priced_to = EXCLUDED.priced_to, finalized = EXCLUDED.finalized
			WHERE NOT cost_transfer_snapshots.finalized`,
			v.id, runID, e.TenantID, v.hostID, v.currency, v.amount, nonNil(v.items), nonNil(v.hours), nonNil(v.unpriced), end, v.final); err != nil {
			return nil, err
		}
	}
	return transferLines(entries, e, now), nil
}

// placementNetSamples is a placement's last sampled counters in each UTC
// hour, oldest first: the counters are cumulative, so an hour's growth is
// its last reading less the previous hour's, and earlier readings or
// coarser rollups of the same hour add nothing.
//
// A rolled-up hour is its res=3600 row: the rollup keeps the hour's last
// reading (history.go lastNetClasses), and rollups run in order with one
// table-wide cutoff, so every hour before the placement's newest res=3600
// row has one. Only the hours after it are read from raw and minute rows,
// one per UTC hour (transferHours buckets in UTC, not the session zone).
// The tail's bound is an uncorrelated (SELECT coalesce(max(..))) so it is
// an index condition on the primary key, as rollupSince's is.
func placementNetSamples(ctx context.Context, tx pgx.Tx, runID string, epoch int) ([]netSample, error) {
	rows, err := tx.Query(ctx, `SELECT at, net_classes FROM placement_samples
			WHERE run_id = $1 AND epoch = $2 AND res = 3600 AND net_classes IS NOT NULL
		UNION ALL
		(SELECT DISTINCT ON (date_trunc('hour', at, 'UTC')) at, net_classes FROM placement_samples
			WHERE run_id = $1 AND epoch = $2 AND res IN (0, 60) AND net_classes IS NOT NULL
				AND at >= (SELECT coalesce(max(at) + interval '1 hour', '-infinity') FROM placement_samples
					WHERE run_id = $1 AND epoch = $2 AND res = 3600)
			ORDER BY date_trunc('hour', at, 'UTC'), at DESC, res)
		ORDER BY 1`, runID, epoch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[netSample])
}

// transferLines are the Run's data-transfer lines from its snapshots: one
// per (class, direction, currency), the currency appended to the item
// when a Run's placements were priced in several. Their hours are added to
// e.Hours.
func transferLines(entries []transferSnapshot, e *computeEval, now time.Time) []costReport {
	type key struct{ item, currency string }
	type line struct {
		class, dir string
		bytes      int64
		amount     *big.Rat
		perGB      map[string]bool
		from, to   time.Time
		placements []map[string]any
	}
	lines := map[key]*line{}
	for _, v := range entries {
		if v.currency == nil {
			continue
		}
		end := now
		if v.to != nil {
			end = *v.to
		}
		for _, it := range v.items {
			k := key{it.Class + ":" + it.Direction, *v.currency}
			l := lines[k]
			if l == nil {
				l = &line{class: it.Class, dir: it.Direction, amount: new(big.Rat), perGB: map[string]bool{}, from: v.from, to: end}
				lines[k] = l
			}
			l.bytes += it.Bytes
			l.amount.Add(l.amount, mustRat(it.Amount))
			l.perGB[it.PerGB] = true
			l.from, l.to = minTime(l.from, v.from), maxTime(l.to, end)
			l.placements = append(l.placements, map[string]any{"epoch": v.epoch, "hostId": v.hostID, "from": v.from, "to": v.to,
				"bytes": it.Bytes, "perGB": it.PerGB, "amount": it.Amount, "finalized": v.final})
		}
		for _, h := range v.hours {
			e.Hours = append(e.Hours, computeHour{h.Hour, v.hostID, familyDataTransfer, *v.currency, mustRat(h.Amount)})
		}
	}
	perItem := map[string]int{}
	for k := range lines {
		perItem[k.item]++
	}
	out := make([]costReport, 0, len(lines))
	for k, l := range lines {
		item := k.item
		if perItem[item] > 1 {
			item += ":" + k.currency
		}
		d := map[string]any{"class": l.class, "direction": l.dir, "bytes": l.bytes, "currency": k.currency, "placements": l.placements}
		if len(l.perGB) == 1 {
			for v := range l.perGB {
				d["perGB"] = v
			}
		}
		out = append(out, costReport{Family: familyDataTransfer, Item: item, Amount: moneyString(l.amount), Currency: k.currency,
			From: l.from, To: l.to, Details: d})
	}
	return out
}
