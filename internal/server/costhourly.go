package server

import (
	"context"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

type computeHour struct {
	Hour     time.Time
	Host     string
	Currency string
	Amount   *big.Rat
}

// Split an exact amount across UTC billing hours before rounding money.
func forEachCostHour(from, to time.Time, visit func(time.Time, *big.Rat)) {
	if !to.After(from) {
		return
	}
	for at := from; at.Before(to); {
		hour := at.UTC().Truncate(time.Hour)
		end := minTime(to, hour.Add(time.Hour))
		visit(hour, big.NewRat(int64(end.Sub(at)), int64(to.Sub(from))))
		at = end
	}
}

func replaceComputeHours(ctx context.Context, tx pgx.Tx, tenant, run string, hours []computeHour) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE run_id = $1 AND source = 'compute'`, run); err != nil {
		return err
	}
	type key struct {
		hour           time.Time
		host, currency string
	}
	amounts := map[key]*big.Rat{}
	for _, h := range hours {
		k := key{h.Hour, h.Host, h.Currency}
		if amounts[k] == nil {
			amounts[k] = new(big.Rat)
		}
		amounts[k].Add(amounts[k], h.Amount)
	}
	for k, v := range amounts {
		// The aggregation key includes the host to keep host and pool groups accurate.
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool, amount)
			SELECT $1, $2, $3, 'compute', 'compute', $4, h.id, h.pool, $5::numeric FROM hosts h WHERE h.id = $6`,
			k.hour, tenant, run, k.currency, moneyString(v), k.host)
		if err != nil {
			return err
		}
	}
	return nil
}

func replacePluginHours(ctx context.Context, tx pgx.Tx, tenant, run, source string, lines []costReport) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE run_id = $1 AND source = $2`, run, source); err != nil {
		return err
	}
	type key struct {
		hour             time.Time
		family, currency string
	}
	amounts := map[key]*big.Rat{}
	for _, l := range lines {
		amount := mustRat(l.Amount)
		// Zero-duration reports belong to their starting hour.
		if !l.To.After(l.From) {
			k := key{l.From.UTC().Truncate(time.Hour), l.Family, l.Currency}
			if amounts[k] == nil {
				amounts[k] = new(big.Rat)
			}
			amounts[k].Add(amounts[k], amount)
			continue
		}
		remaining := new(big.Rat).Set(amount)
		forEachCostHour(l.From, l.To, func(hour time.Time, fraction *big.Rat) {
			k := key{hour, l.Family, l.Currency}
			if amounts[k] == nil {
				amounts[k] = new(big.Rat)
			}
			part := moneyString(new(big.Rat).Mul(amount, fraction))
			if !hour.Add(time.Hour).Before(l.To) {
				part = moneyString(remaining)
			}
			amounts[k].Add(amounts[k], mustRat(part))
			remaining.Sub(remaining, mustRat(part))
		})
	}
	for k, v := range amounts {
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`, k.hour, tenant, run, source, k.family, k.currency, moneyString(v))
		if err != nil {
			return err
		}
	}
	return nil
}

// Host totals include idle hours with no Run, and are regenerated at each tick.
func (s *Server) updateHostHours(ctx context.Context, tx pgx.Tx, now time.Time) error {
	hour := now.UTC().Truncate(time.Hour).Add(-time.Hour)
	rows, err := tx.Query(ctx, `SELECT id FROM hosts WHERE coalesce(provision_requested_at, registered_at, created_at) < $2
		AND (terminated_at IS NULL OR terminated_at > $1)`, hour, now)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		in, err := loadHostCompute(ctx, tx, id, hour, now)
		if err != nil {
			return err
		}
		res, err := computeCost(in)
		if err != nil {
			return err
		}
		type key struct {
			hour     time.Time
			currency string
		}
		allocated, unallocated := map[key]*big.Rat{}, map[key]*big.Rat{}
		for _, piece := range res.Pieces {
			if piece.Rate == nil {
				continue
			}
			forEachCostHour(piece.From, piece.To, func(at time.Time, fraction *big.Rat) {
				k := key{at, piece.Rate.Currency}
				if allocated[k] == nil {
					allocated[k], unallocated[k] = new(big.Rat), new(big.Rat)
				}
				for _, v := range piece.Charged {
					allocated[k].Add(allocated[k], new(big.Rat).Mul(v, fraction))
				}
				unallocated[k].Add(unallocated[k], new(big.Rat).Mul(piece.Unallocated, fraction))
			})
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE host_id = $1 AND run_id IS NULL AND hour >= $2`, id, hour); err != nil {
			return err
		}
		for k, v := range allocated {
			_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool, allocated, unallocated)
				SELECT $1, 'compute', 'compute', $2, id, pool, $3::numeric, $4::numeric FROM hosts WHERE id = $5`,
				k.hour, k.currency, moneyString(v), moneyString(unallocated[k]), id)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
