package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type costSummaryInput struct {
	HistoryQuery
	Group    []string `query:"group,explode" doc:"Repeat up to twice: tenant (operators), pool, host, family, run, label:key."`
	Family   string   `query:"family" doc:"Only this family."`
	Interval string   `query:"interval" doc:"hour or day; include a time series."`
}

func (in *costSummaryInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Group = u.Query()["group"]
	return nil
}

type costSummaryRow struct {
	At       *time.Time        `json:"at,omitempty"`
	Group    map[string]string `json:"group,omitempty"`
	Currency string            `json:"currency"`
	Amount   string            `json:"amount"`
}

type hostAllocation struct {
	HostID      string `json:"hostId"`
	Currency    string `json:"currency"`
	Allocated   string `json:"allocated"`
	Unallocated string `json:"unallocated"`
}

type costSummaryBody struct {
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Basis       string           `json:"basis"`
	Totals      []costSummaryRow `json:"totals"`
	Series      []costSummaryRow `json:"series,omitempty"`
	Unallocated []costSummaryRow `json:"unallocated,omitempty"`
	Hosts       []hostAllocation `json:"hosts,omitempty"`
}

type costSummaryOutput struct {
	Body costSummaryBody `nameHint:"CostSummary"`
}

const (
	costMaxRange = 90 * 24 * time.Hour
	costMaxRows  = 10000
)

// Cost buckets are whole UTC hours; a partial boundary includes its hour.
func costRange(from, to time.Time) (time.Time, time.Time, error) {
	if !from.Before(to) {
		return from, to, errf(http.StatusBadRequest, "bad_request", "from must precede to")
	}
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC()
	if !to.Equal(to.Truncate(time.Hour)) {
		to = to.Truncate(time.Hour).Add(time.Hour)
	}
	if to.Sub(from) > costMaxRange {
		return from, to, errf(http.StatusBadRequest, "bad_request", "cost range must not exceed 90 days")
	}
	return from, to, nil
}

func costRowLimit(n int) error {
	if n > costMaxRows {
		return errf(http.StatusRequestEntityTooLarge, "too_large", "cost response exceeds %d rows", costMaxRows)
	}
	return nil
}

func (s *Server) costSummary(ctx context.Context, in *costSummaryInput) (*costSummaryOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	from, to, err = costRange(from, to)
	if err != nil {
		return nil, err
	}
	if in.Interval != "" && in.Interval != "hour" && in.Interval != "day" {
		return nil, errf(http.StatusBadRequest, "bad_request", "interval must be hour or day")
	}
	if len(in.Group) > 2 {
		return nil, errf(http.StatusBadRequest, "bad_request", "at most two groups")
	}
	var group [2]string
	var label [2]string
	for i, g := range in.Group {
		switch {
		case g == "tenant" && p.Operator, g == "pool", g == "host", g == "family", g == "run":
			group[i] = g
		case strings.HasPrefix(g, "label:") && len(g) > len("label:"):
			group[i], label[i] = "label", strings.TrimPrefix(g, "label:")
		default:
			return nil, errf(http.StatusBadRequest, "bad_request", "invalid group %q", g)
		}
		if i > 0 && g == in.Group[0] {
			return nil, errf(http.StatusBadRequest, "bad_request", "duplicate group %q", g)
		}
	}
	out := &costSummaryOutput{Body: costSummaryBody{From: from, To: to, Basis: "list", Totals: []costSummaryRow{}}}
	// Group selectors and label keys are values, never SQL identifiers.
	const base = `WITH scoped AS (
		SELECT c.hour, c.currency, c.amount, c.tenant_id, c.family, c.run_id,
			coalesce(c.pool, '(none)') AS pool, coalesce(c.host_id, '(none)') AS host, r.labels
		FROM cost_hourly c JOIN runs r ON r.id = c.run_id
		WHERE c.run_id IS NOT NULL AND c.hour >= $1 AND c.hour < $2
			AND ($3 = '' OR c.family = $3)
	), dimensions AS (
		SELECT hour, currency, amount,
			CASE $4::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$6, '(none)') END AS g1,
			CASE $5::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$7, '(none)') END AS g2
		FROM scoped
	) SELECT `
	rowCount := 0
	read := func(tx pgx.Tx, interval string) ([]costSummaryRow, error) {
		bucket := "NULL::timestamptz"
		switch interval {
		case "hour":
			bucket = "date_trunc('hour', hour)"
		case "day":
			bucket = "date_trunc('day', hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
		}
		result := []costSummaryRow{}
		rows, err := tx.Query(ctx, base+bucket+`, g1, g2, currency, trim_scale(sum(amount))::text
				FROM dimensions GROUP BY 1, 2, 3, 4 ORDER BY 1 NULLS FIRST, 2, 3, 4`,
			from, to, in.Family, group[0], group[1], label[0], label[1])
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var row costSummaryRow
			var g1, g2 *string
			if err := rows.Scan(&row.At, &g1, &g2, &row.Currency, &row.Amount); err != nil {
				return nil, err
			}
			if len(in.Group) > 0 {
				value := "(none)"
				if g1 != nil {
					value = *g1
				}
				row.Group = map[string]string{in.Group[0]: value}
			}
			if len(in.Group) > 1 {
				value := "(none)"
				if g2 != nil {
					value = *g2
				}
				row.Group[in.Group[1]] = value
			}
			result = append(result, row)
			rowCount++
			if err := costRowLimit(rowCount); err != nil {
				return nil, err
			}
		}
		return result, rows.Err()
	}
	err = pgx.BeginTxFunc(ctx, s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if p.TenantID == "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `SELECT set_config('lux.tenant_id', $1, true)`, p.TenantID); err != nil {
				return err
			}
		}
		out.Body.Totals, err = read(tx, "")
		if err != nil {
			return err
		}
		if in.Interval != "" {
			out.Body.Series, err = read(tx, in.Interval)
			if err != nil {
				return err
			}
		}
		if p.Operator && p.TenantID == "" {
			out.Body.Unallocated = []costSummaryRow{}
			rows, err := tx.Query(ctx, `SELECT currency, trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3)
				GROUP BY currency ORDER BY currency`, from, to, in.Family)
			if err != nil {
				return err
			}
			for rows.Next() {
				var row costSummaryRow
				if err := rows.Scan(&row.Currency, &row.Amount); err != nil {
					rows.Close()
					return err
				}
				out.Body.Unallocated = append(out.Body.Unallocated, row)
				rowCount++
				if err := costRowLimit(rowCount); err != nil {
					rows.Close()
					return err
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil || group[0] != "host" && group[1] != "host" {
				return err
			}
			out.Body.Hosts = []hostAllocation{}
			rows, err = tx.Query(ctx, `SELECT host_id, currency, trim_scale(sum(allocated))::text,
				trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3)
				GROUP BY host_id, currency ORDER BY host_id, currency`, from, to, in.Family)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var row hostAllocation
				if err := rows.Scan(&row.HostID, &row.Currency, &row.Allocated, &row.Unallocated); err != nil {
					return err
				}
				out.Body.Hosts = append(out.Body.Hosts, row)
				rowCount++
				if err := costRowLimit(rowCount); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type hostCostInput struct {
	HostPath
	HistoryQuery
}
type hostCostHour struct {
	Hour        time.Time `json:"hour"`
	Currency    string    `json:"currency"`
	Allocated   string    `json:"allocated"`
	Unallocated *string   `json:"unallocated,omitempty"`
}
type hostCostRate struct {
	From     time.Time  `json:"from"`
	To       *time.Time `json:"to,omitempty"`
	PerHour  string     `json:"perHour"`
	Currency string     `json:"currency"`
	Source   string     `json:"source"`
}
type hostCostOutput struct {
	Body struct {
		HostID string         `json:"hostId"`
		From   time.Time      `json:"from"`
		To     time.Time      `json:"to"`
		Basis  string         `json:"basis"`
		Hours  []hostCostHour `json:"hours"`
		Rates  []hostCostRate `json:"rates,omitempty"`
	} `nameHint:"HostCost"`
}

func (s *Server) hostCost(ctx context.Context, in *hostCostInput) (*hostCostOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	from, to, err = costRange(from, to)
	if err != nil {
		return nil, err
	}
	out := &hostCostOutput{}
	out.Body.From, out.Body.To, out.Body.Basis = from, to, "list"
	out.Body.Hours = []hostCostHour{}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, true)
		if err != nil {
			return err
		}
		if p.TenantID != "" {
			var own bool
			if err := tx.QueryRow(ctx, `SELECT tenant_id IS NOT DISTINCT FROM $2 FROM hosts WHERE id = $1`, id, p.TenantID).Scan(&own); err != nil {
				return err
			}
			if !own {
				return errf(http.StatusForbidden, "forbidden", "a host's cost is its owner's or the operators'")
			}
		}
		out.Body.HostID = id
		rows, err := tx.Query(ctx, `SELECT hour, currency, trim_scale(sum(allocated))::text,
			trim_scale(sum(unallocated))::text FROM cost_hourly
			WHERE host_id = $1 AND run_id IS NULL AND hour >= $2 AND hour < $3
			GROUP BY hour, currency ORDER BY hour, currency`, id, from, to)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h hostCostHour
			if err := rows.Scan(&h.Hour, &h.Currency, &h.Allocated, &h.Unallocated); err != nil {
				rows.Close()
				return err
			}
			if p.TenantID != "" {
				h.Unallocated = nil
			}
			out.Body.Hours = append(out.Body.Hours, h)
			if err := costRowLimit(len(out.Body.Hours)); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		out.Body.Rates = []hostCostRate{}
		if p.TenantID != "" {
			return nil
		}
		rows, err = tx.Query(ctx, `SELECT valid_from, valid_to, trim_scale(per_hour)::text, currency, source
			FROM host_rates WHERE host_id = $1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
			ORDER BY valid_from`, id, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r hostCostRate
			if err := rows.Scan(&r.From, &r.To, &r.PerHour, &r.Currency, &r.Source); err != nil {
				return err
			}
			out.Body.Rates = append(out.Body.Rates, r)
			if err := costRowLimit(len(out.Body.Hours) + len(out.Body.Rates)); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
