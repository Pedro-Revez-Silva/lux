package server

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/store"
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

func replaceComputeHours(ctx context.Context, tx pgx.Tx, tenant, run string, hours []computeHour, retention time.Duration) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE run_id = $1 AND source = 'compute'`, run); err != nil {
		return err
	}
	type key struct {
		hour           time.Time
		host, currency string
	}
	amounts := map[key]*big.Rat{}
	cutoff := time.Now().UTC().Add(-retention)
	for _, h := range hours {
		if h.Hour.Before(cutoff) {
			continue
		}
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

func replacePluginHours(ctx context.Context, tx pgx.Tx, tenant, run, source string, lines []costReport, retention time.Duration) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE run_id = $1 AND source = $2`, run, source); err != nil {
		return err
	}
	type key struct {
		hour             time.Time
		family, currency string
	}
	amounts := map[key]*big.Rat{}
	cutoff := time.Now().UTC().Add(-retention)
	for _, l := range lines {
		amount := mustRat(l.Amount)
		// Zero-duration reports belong to their starting hour.
		if !l.To.After(l.From) {
			k := key{l.From.UTC().Truncate(time.Hour), l.Family, l.Currency}
			if k.hour.Before(cutoff) {
				continue
			}
			if amounts[k] == nil {
				amounts[k] = new(big.Rat)
			}
			amounts[k].Add(amounts[k], amount)
			continue
		}
		remaining := new(big.Rat).Set(amount)
		forEachCostHour(l.From, l.To, func(hour time.Time, fraction *big.Rat) {
			k := key{hour, l.Family, l.Currency}
			part := moneyString(new(big.Rat).Mul(amount, fraction))
			if !hour.Add(time.Hour).Before(l.To) {
				part = moneyString(remaining)
			}
			remaining.Sub(remaining, mustRat(part))
			if k.hour.Before(cutoff) {
				return
			}
			if amounts[k] == nil {
				amounts[k] = new(big.Rat)
			}
			amounts[k].Add(amounts[k], mustRat(part))
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

// Host totals include idle hours with no Run. Each pass works at most Batch
// host-hours; the cursor survives restarts and excludes hours past retention.
func (s *Server) updateHostHours(ctx context.Context) error {
	var jobs []struct {
		id   string
		hour time.Time
	}
	now := time.Now().UTC()
	oldest := now.Add(-s.cfg.Costs.Hourly).Truncate(time.Hour).Add(time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.id, greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1)
			FROM hosts h LEFT JOIN cost_host_refresh c ON c.host_id = h.id
			WHERE (c.retry_at IS NULL OR c.retry_at <= $2)
				AND greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1) <= date_trunc('hour', $2::timestamptz)
				AND (h.terminated_at IS NULL OR h.terminated_at > greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1))
			ORDER BY 2, h.id LIMIT $3`, oldest, now, s.cfg.Costs.Batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var job struct {
				id   string
				hour time.Time
			}
			if err := rows.Scan(&job.id, &job.hour); err != nil {
				return err
			}
			jobs = append(jobs, job)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, job := range jobs {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			// Recheck the cursor under the host lock when another luxd is refreshing.
			var end *time.Time
			if err := tx.QueryRow(ctx, `SELECT terminated_at FROM hosts WHERE id = $1 FOR UPDATE`, job.id).Scan(&end); err != nil {
				return err
			}
			var cursor time.Time
			err := tx.QueryRow(ctx, `SELECT next_hour FROM cost_host_refresh WHERE host_id = $1`, job.id).Scan(&cursor)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil && cursor.After(job.hour) {
				return nil
			}
			to := minTime(now, job.hour.Add(time.Hour))
			if end != nil {
				to = minTime(to, *end)
			}
			if to.After(job.hour) {
				if err := s.writeHostHour(ctx, tx, job.id, job.hour, to); err != nil {
					return err
				}
			}
			next := job.hour.Add(time.Hour)
			if end == nil && next.After(now) {
				next = job.hour // keep the open hour fresh
			}
			var retry *time.Time
			if next.Equal(job.hour) {
				t := now.Add(max(s.cfg.Costs.Every, DefaultCostsEvery))
				retry = &t
			}
			_, err = tx.Exec(ctx, `INSERT INTO cost_host_refresh (host_id, next_hour, retry_at) VALUES ($1, $2, $3)
				ON CONFLICT (host_id) DO UPDATE SET next_hour = EXCLUDED.next_hour, retry_at = EXCLUDED.retry_at`, job.id, next, retry)
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			s.log.Warn("costs: host hour", "host", job.id, "hour", job.hour, "err", err)
			if retryErr := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO cost_host_refresh (host_id, next_hour, retry_at)
					VALUES ($1, $2, now() + interval '1 hour')
					ON CONFLICT (host_id) DO UPDATE SET retry_at = EXCLUDED.retry_at`, job.id, job.hour)
				return err
			}); retryErr != nil {
				return retryErr
			}
		}
	}
	return nil
}

func (s *Server) writeHostHour(ctx context.Context, tx pgx.Tx, id string, hour, to time.Time) error {
	in, err := loadHostCompute(ctx, tx, id, hour, to)
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
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE host_id = $1 AND run_id IS NULL AND hour = $2`, id, hour); err != nil {
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
	return nil
}
