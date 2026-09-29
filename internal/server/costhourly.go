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

// allocateCostHours rounds cumulative exact charges, keeping each hourly part
// nonnegative for nonnegative amounts and preserving the once-rounded total.
func allocateCostHours(from, to time.Time, amount *big.Rat, visit func(time.Time, *big.Rat)) {
	cumulative := new(big.Rat)
	previous := new(big.Rat)
	forEachCostHour(from, to, func(hour time.Time, fraction *big.Rat) {
		cumulative.Add(cumulative, new(big.Rat).Mul(amount, fraction))
		part := mustRat(moneyString(cumulative))
		visit(hour, new(big.Rat).Sub(part, previous))
		previous.Set(part)
	})
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
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount)
			SELECT $1, $2, $3, 'compute', 'compute', $4, h.id, h.pool_id, $5::numeric FROM hosts h WHERE h.id = $6`,
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
		allocateCostHours(l.From, l.To, amount, func(hour time.Time, part *big.Rat) {
			k := key{hour, l.Family, l.Currency}
			if k.hour.Before(cutoff) {
				return
			}
			if amounts[k] == nil {
				amounts[k] = new(big.Rat)
			}
			amounts[k].Add(amounts[k], part)
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

// Each pass refreshes at most Batch host-hours, including idle time. Reserve
// a slot for older hours when current hours are due; with Batch=1, persist
// which priority goes first so a current hour cannot starve the backlog.
// The per-host cursor resumes within retention after a restart.
func (s *Server) updateHostHours(ctx context.Context) error {
	var jobs []struct {
		id      string
		hour    time.Time
		pending bool
	}
	now := time.Now().UTC()
	oldest := now.Add(-s.cfg.Costs.Hourly).Truncate(time.Hour).Add(time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM cost_host_hour_gaps WHERE hour < $1`, oldest); err != nil {
			return err
		}
		firstLimit := max(1, s.cfg.Costs.Batch-1)
		var pendingFirst bool
		if s.cfg.Costs.Batch == 1 {
			var currentFirst bool
			if err := tx.QueryRow(ctx, `SELECT current_first FROM cost_host_turn WHERE id FOR UPDATE`).Scan(&currentFirst); err != nil {
				return err
			}
			pendingFirst = !currentFirst
			if pendingFirst {
				firstLimit = 0
			}
		}
		rows, err := tx.Query(ctx, `SELECT h.id, greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1)
			FROM hosts h LEFT JOIN cost_host_refresh c ON c.host_id = h.id
			WHERE (c.retry_at IS NULL OR c.retry_at <= $2)
				AND greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1) <= date_trunc('hour', $2::timestamptz)
				AND (h.terminated_at IS NULL OR h.terminated_at > greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1))
			ORDER BY CASE WHEN greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1) = date_trunc('hour', $2::timestamptz) THEN 0 ELSE 1 END,
				2, h.id LIMIT $3`, oldest, now, firstLimit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var job struct {
				id      string
				hour    time.Time
				pending bool
			}
			if err := rows.Scan(&job.id, &job.hour); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, job)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(jobs) < s.cfg.Costs.Batch && (pendingFirst || s.cfg.Costs.Batch > 1) {
			ids := make([]string, 0, len(jobs))
			hours := make([]time.Time, 0, len(jobs))
			for _, job := range jobs {
				ids = append(ids, job.id)
				hours = append(hours, job.hour)
			}
			var job struct {
				id      string
				hour    time.Time
				pending bool
			}
			err := tx.QueryRow(ctx, `SELECT host_id, hour FROM cost_host_hour_gaps
				WHERE retry_at <= $1 AND hour >= $2 AND NOT EXISTS (
					SELECT 1 FROM unnest($3::text[], $4::timestamptz[]) AS selected(id, hour)
					WHERE selected.id = host_id AND selected.hour = cost_host_hour_gaps.hour)
				ORDER BY retry_at, hour, host_id LIMIT 1`, now, oldest, ids, hours).Scan(&job.id, &job.hour)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				job.pending = true
				jobs = append(jobs, job)
			}
		}
		if len(jobs) < s.cfg.Costs.Batch {
			ids := make([]string, 0, len(jobs))
			for _, job := range jobs {
				ids = append(ids, job.id)
			}
			var job struct {
				id      string
				hour    time.Time
				pending bool
			}
			err = tx.QueryRow(ctx, `SELECT h.id, greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1)
				FROM hosts h LEFT JOIN cost_host_refresh c ON c.host_id = h.id
				WHERE (c.retry_at IS NULL OR c.retry_at <= $2)
					AND greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1) <= date_trunc('hour', $2::timestamptz)
					AND (h.terminated_at IS NULL OR h.terminated_at > greatest(coalesce(c.next_hour, date_trunc('hour', coalesce(h.provision_requested_at, h.registered_at, h.created_at))), $1))
					AND h.id <> ALL($3::text[])
				ORDER BY 2, h.id LIMIT 1`, oldest, now, ids).Scan(&job.id, &job.hour)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				jobs = append(jobs, job)
			}
		}
		if len(jobs) < s.cfg.Costs.Batch && !pendingFirst {
			ids := make([]string, 0, len(jobs))
			hours := make([]time.Time, 0, len(jobs))
			for _, job := range jobs {
				ids = append(ids, job.id)
				hours = append(hours, job.hour)
			}
			var job struct {
				id      string
				hour    time.Time
				pending bool
			}
			err := tx.QueryRow(ctx, `SELECT host_id, hour FROM cost_host_hour_gaps
				WHERE retry_at <= $1 AND hour >= $2 AND NOT EXISTS (
					SELECT 1 FROM unnest($3::text[], $4::timestamptz[]) AS selected(id, hour)
					WHERE selected.id = host_id AND selected.hour = cost_host_hour_gaps.hour)
				ORDER BY retry_at, hour, host_id LIMIT 1`, now, oldest, ids, hours).Scan(&job.id, &job.hour)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				job.pending = true
				jobs = append(jobs, job)
			}
		}
		if s.cfg.Costs.Batch == 1 && len(jobs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE cost_host_turn SET current_first = NOT current_first WHERE id`); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, job := range jobs {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			// Serialize occupancy changes before reading the host or its placements.
			if err := lockCostHost(ctx, tx, job.id); err != nil {
				return err
			}
			var end, registered, provisioned *time.Time
			if err := tx.QueryRow(ctx, `SELECT terminated_at, registered_at, provision_requested_at FROM hosts WHERE id = $1`, job.id).Scan(&end, &registered, &provisioned); err != nil {
				return err
			}
			var cursor time.Time
			var retryAt *time.Time
			err := tx.QueryRow(ctx, `SELECT next_hour, retry_at FROM cost_host_refresh WHERE host_id = $1`, job.id).Scan(&cursor, &retryAt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if job.pending {
				var due bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cost_host_hour_gaps
					WHERE host_id = $1 AND hour = $2 AND retry_at <= $3)`, job.id, job.hour, now).Scan(&due); err != nil {
					return err
				}
				if !due {
					return nil
				}
			} else if err == nil && (cursor.After(job.hour) || (retryAt != nil && retryAt.After(now))) {
				return nil
			}
			to := minTime(now, job.hour.Add(time.Hour))
			if end != nil {
				to = minTime(to, *end)
			}
			if to.After(job.hour) {
				if err := s.writeHostHour(ctx, tx, job.id, job.hour, to, provisioned, registered, end); err != nil {
					return err
				}
			}
			if job.pending {
				return nil
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
				if err := lockCostHost(ctx, tx, job.id); err != nil {
					return err
				}
				if job.pending {
					_, err := tx.Exec(ctx, `UPDATE cost_host_hour_gaps SET retry_at = now() + interval '1 hour'
						WHERE host_id = $1 AND hour = $2 AND retry_at IS NOT NULL`, job.id, job.hour)
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO cost_host_refresh (host_id, next_hour, retry_at)
					VALUES ($1, $2, now() + interval '1 hour')
					ON CONFLICT (host_id) DO UPDATE SET retry_at = EXCLUDED.retry_at
					WHERE cost_host_refresh.next_hour <= EXCLUDED.next_hour`, job.id, job.hour)
				return err
			}); retryErr != nil {
				return retryErr
			}
		}
	}
	return nil
}

func (s *Server) writeHostHour(ctx context.Context, tx pgx.Tx, id string, hour, to time.Time, provisioned, registered, terminated *time.Time) error {
	in, err := loadHostCompute(ctx, tx, id, hour, to)
	if err != nil {
		return err
	}
	res, err := computeCost(in)
	if err != nil {
		return err
	}
	// Unpriced intervals stay visible; recoverable gaps retry independently
	// of the forward cursor, while priced pieces remain available immediately.
	clock := time.Now().UTC()
	retry := clock.Add(min(time.Hour, max(s.cfg.Costs.Every, DefaultCostsEvery)))
	boundary := hour.Add(s.cfg.Costs.Hourly)
	if !retry.Before(boundary) {
		if remaining := boundary.Sub(clock); remaining > 0 {
			retry = clock.Add(remaining / 2)
		} else {
			retry = clock
		}
	}
	type key struct {
		hour     time.Time
		currency string
	}
	allocated, total := map[key]*big.Rat{}, map[key]*big.Rat{}
	for _, piece := range res.Pieces {
		if piece.Rate == nil {
			continue
		}
		forEachCostHour(piece.From, piece.To, func(at time.Time, fraction *big.Rat) {
			k := key{at, piece.Rate.Currency}
			if allocated[k] == nil {
				allocated[k], total[k] = new(big.Rat), new(big.Rat)
			}
			for _, v := range piece.Charged {
				allocated[k].Add(allocated[k], new(big.Rat).Mul(v, fraction))
			}
			total[k].Add(total[k], new(big.Rat).Mul(piece.Host, fraction))
		})
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cost_host_hour_gaps WHERE host_id = $1 AND hour = $2`, id, hour); err != nil {
		return err
	}
	for _, gap := range res.Missing {
		reason := "rate_pending"
		var retryAt *time.Time = &retry
		if provisioned == nil && terminated != nil {
			reason, retryAt = "static_unpriced", nil
		} else if provisioned != nil && terminated != nil && registered != nil && !gap.To.After(*registered) {
			reason, retryAt = "provider_pre_registration", nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cost_host_hour_gaps (host_id, hour, missing_from, missing_to, reason, retry_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, hour, gap.From, gap.To, reason, retryAt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cost_hourly WHERE host_id = $1 AND run_id IS NULL AND hour = $2`, id, hour); err != nil {
		return err
	}
	for k, v := range allocated {
		billed := mustRat(moneyString(total[k]))
		charged := mustRat(moneyString(v))
		idle := new(big.Rat).Sub(billed, charged)
		_, err := tx.Exec(ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool_id, allocated, unallocated)
				SELECT $1, 'compute', 'compute', $2, id, pool_id, $3::numeric, $4::numeric FROM hosts WHERE id = $5`,
			k.hour, k.currency, moneyString(charged), moneyString(idle), id)
		if err != nil {
			return err
		}
	}
	return nil
}
