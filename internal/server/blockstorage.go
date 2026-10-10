package server

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Block storage (docs/costs.md, section 3): a provider host's volumes cost
// their provisioned size, IOPS and throughput over the host's billed window,
// whether used or not. Providers quote these per month; lux turns a month
// into hours with AWS's convention of 730 hours a month.
const hoursPerMonth = 730

// The cost families lux prices itself: the machine, and the volumes that
// live and die with it. Both are host-tied: shared between a host's Runs by
// their share, the rest unallocated.
const (
	familyCompute      = "compute"
	familyBlockStorage = "block-storage"
)

var hostFamilies = []string{familyCompute, familyBlockStorage}

// ebsBilling is what each volume type bills beyond its storage: IOPS and
// throughput above the free baseline (Free*), or not at all (false). A type
// not listed has no known price: its host's block storage is missing.
//
//	gp3: IOPS above 3000, throughput above 125 MiB/s
//	io1, io2: every provisioned IOPS (io2's higher IOPS tiers are not modelled)
//	gp2, st1, sc1, standard: storage only (standard's per-I/O charge is usage, not provisioned)
type ebsBilling struct {
	IOPS, Throughput         bool
	FreeIOPS, FreeThroughput int64
}

var ebsTypes = map[string]ebsBilling{
	"gp3":      {IOPS: true, Throughput: true, FreeIOPS: 3000, FreeThroughput: 125},
	"io1":      {IOPS: true},
	"io2":      {IOPS: true},
	"gp2":      {},
	"st1":      {},
	"sc1":      {},
	"standard": {},
}

var errUnknownVolumeType = errors.New("no price model for this volume type")

// volumeHourly is one volume's hourly list price, exact: (size × per GB-month
// + billable IOPS × per IOPS-month + billable MiB/s ÷ 1024 × per GiB/s-month)
// ÷ 730. A dimension the type bills but price lacks is an error, never zero.
func volumeHourly(v HostVolume, price BlockStoragePrice) (*big.Rat, error) {
	b, ok := ebsTypes[v.Type]
	if !ok {
		return nil, fmt.Errorf("volume type %q: %w", v.Type, errUnknownVolumeType)
	}
	month := new(big.Rat)
	add := func(units *big.Rat, unitPrice, what string) error {
		if units.Sign() <= 0 {
			return nil
		}
		p, ok := new(big.Rat).SetString(unitPrice)
		if unitPrice == "" || !ok {
			return fmt.Errorf("volume type %q: no %s price", v.Type, what)
		}
		month.Add(month, units.Mul(units, p))
		return nil
	}
	if err := add(big.NewRat(v.SizeGiB, 1), price.PerGBMonth, "storage"); err != nil {
		return nil, err
	}
	if b.IOPS {
		if err := add(big.NewRat(max(0, v.IOPS-b.FreeIOPS), 1), price.PerIOPSMonth, "IOPS"); err != nil {
			return nil, err
		}
	}
	if b.Throughput {
		if err := add(big.NewRat(max(0, v.ThroughputMiBps-b.FreeThroughput), 1024), price.PerGiBpsMonth, "throughput"); err != nil {
			return nil, err
		}
	}
	return month.Quo(month, big.NewRat(hoursPerMonth, 1)), nil
}

// Block-storage unit prices share price_cache with on-demand prices: one row
// per (region, volume type, dimension), the dimension's monthly unit price in
// per_hour, instance_type '<type>:<dimension>' and os 'EBS'. The storage row
// is always present for a fetched type, so its fetched_at dates the set.
const (
	ebsCacheOS   = "EBS"
	dimStorage   = "GB-Mo"
	dimIOPS      = "IOPS-Mo"
	dimThroughpt = "GiBps-Mo"
)

var blockStorageDims = []string{dimStorage, dimIOPS, dimThroughpt}

func (p BlockStoragePrice) dims() map[string]string {
	return map[string]string{dimStorage: p.PerGBMonth, dimIOPS: p.PerIOPSMonth, dimThroughpt: p.PerGiBpsMonth}
}

func (p *BlockStoragePrice) set(dim, v string) {
	switch dim {
	case dimStorage:
		p.PerGBMonth = v
	case dimIOPS:
		p.PerIOPSMonth = v
	case dimThroughpt:
		p.PerGiBpsMonth = v
	}
}

// cachedBlockStorage reads a type's cached unit prices, and whether they are
// within costs.prices_refresh. Missing: Currency is empty.
func cachedBlockStorage(ctx context.Context, tx pgx.Tx, provider, region, kind string) (BlockStoragePrice, time.Time, error) {
	var price BlockStoragePrice
	var fetched time.Time
	rows, err := tx.Query(ctx, `SELECT split_part(instance_type, ':', 2), trim_scale(per_hour)::text, currency, fetched_at
		FROM price_cache WHERE provider = $1 AND region = $2 AND os = $3 AND split_part(instance_type, ':', 1) = $4`,
		provider, region, ebsCacheOS, kind)
	if err != nil {
		return price, fetched, err
	}
	var dim, v, currency string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&dim, &v, &currency, &at}, func() error {
		price.set(dim, v)
		if dim == dimStorage {
			price.Currency, fetched = currency, at
		}
		return nil
	})
	if price.PerGBMonth == "" {
		return BlockStoragePrice{}, time.Time{}, err
	}
	return price, fetched, err
}

// blockStoragePrice is a volume type's unit prices: the cache while fresh,
// else the provider's answer (cached), else a stale cached answer, as
// onDemandPrice does.
func (s *Server) blockStoragePrice(ctx context.Context, provider, region, kind string) (BlockStoragePrice, error) {
	var cached BlockStoragePrice
	var fetched time.Time
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		cached, fetched, err = cachedBlockStorage(ctx, tx, provider, region, kind)
		return err
	}); err != nil {
		return cached, err
	}
	if cached.Currency != "" && time.Since(fetched) < s.cfg.Costs.PricesRefresh {
		return cached, nil
	}
	p := s.cfg.Costs.Prices[provider]
	if p == nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return cached, fmt.Errorf("no price provider for %s", provider)
	}
	price, err := p.BlockStorage(ctx, region, kind)
	if err == nil {
		price = trimmedBlockStorage(price)
		err = validBlockStorage(price)
	}
	if err != nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return BlockStoragePrice{}, fmt.Errorf("%s block storage price %s/%s: %w", provider, region, kind, err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Upserts and deletes in blockStorageDims order, so two luxds
		// refreshing one type at once take the row locks in the same order
		// and queue rather than deadlock. A dimension the new answer lacks
		// is deleted.
		dims := price.dims()
		for _, dim := range blockStorageDims {
			v := dims[dim]
			if v == "" {
				if _, err := tx.Exec(ctx, `DELETE FROM price_cache WHERE provider = $1 AND region = $2 AND instance_type = $3 AND os = $4`,
					provider, region, kind+":"+dim, ebsCacheOS); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO price_cache (provider, region, instance_type, os, per_hour, currency, fetched_at)
				VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp()) ON CONFLICT (provider, region, instance_type, os)
				DO UPDATE SET per_hour = EXCLUDED.per_hour, currency = EXCLUDED.currency, fetched_at = EXCLUDED.fetched_at`,
				provider, region, kind+":"+dim, ebsCacheOS, v, price.Currency); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return BlockStoragePrice{}, err
	}
	return price, nil
}

// trimmedBlockStorage is p with every price trimmedDecimal.
func trimmedBlockStorage(p BlockStoragePrice) BlockStoragePrice {
	for dim, v := range p.dims() {
		p.set(dim, trimmedDecimal(v))
	}
	return p
}

// validBlockStorage: a currency, a storage price, and every price a
// non-negative decimal of at most 9 fractional digits.
func validBlockStorage(p BlockStoragePrice) error {
	if p.Currency == "" || p.PerGBMonth == "" {
		return errors.New("no storage price")
	}
	for _, v := range p.dims() {
		if v == "" {
			continue
		}
		if err := validPrice(v, p.Currency); err != nil {
			return err
		}
	}
	return nil
}
