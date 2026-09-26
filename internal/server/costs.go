package server

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// CostLine is one thing a Run cost, as one source reported it. Amounts are
// decimal strings: numeric in the database, never a float.
type CostLine struct {
	RunID    string         `json:"runId"`
	Source   string         `json:"source" doc:"compute, or a cost plugin's configured name."`
	Family   string         `json:"family" doc:"Free-form: compute, ai, video, ..."`
	Item     string         `json:"item" doc:"Empty when the source gives none."`
	Amount   string         `json:"amount" doc:"A decimal string, up to 9 fractional digits; may be negative (a refund)." example:"1.28431"`
	Currency string         `json:"currency" doc:"As reported (ISO 4217); never converted." example:"USD"`
	From     time.Time      `json:"from" doc:"Start of the time window the line covers."`
	To       time.Time      `json:"to"`
	Final    bool           `json:"final" doc:"false: an estimate that may still change."`
	Details  map[string]any `json:"details"`
	Reported time.Time      `json:"reportedAt"`
}

// CostTotal is a sum of lines in one currency: amounts in different
// currencies are never added together.
type CostTotal struct {
	Family   string `json:"family,omitempty" doc:"In byFamily only."`
	Currency string `json:"currency"`
	Amount   string `json:"amount" doc:"final + estimate."`
	Final    string `json:"final" doc:"The part from final lines."`
	Estimate string `json:"estimate" doc:"The part from lines that may still change."`
}

// CostSource is how far one source has answered for the Run.
type CostSource struct {
	Source     string     `json:"source"`
	Status     string     `json:"status" enum:"ok,incomplete,final"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	NextAt     *time.Time `json:"nextAt,omitempty"`
}

type RunCost struct {
	RunID  string `json:"runId"`
	Status string `json:"status" enum:"none,complete,incomplete,final" doc:"none: no source has reported. incomplete: some source has not answered or failed (see sources). final: every source is final."`
	Final  bool   `json:"final"`
	Basis  string `json:"basis" doc:"list: list prices, before discounts, credits and tax."`
	// Totals and ByFamily are ordered by currency (and family first).
	Totals   []CostTotal  `json:"totals"`
	ByFamily []CostTotal  `json:"byFamily"`
	Lines    []CostLine   `json:"lines"`
	Sources  []CostSource `json:"sources"`
}

type runCostOutput struct {
	Body *RunCost
}

func (s *Server) runCost(ctx context.Context, in *RunPath) (*runCostOutput, error) {
	p := principal(ctx)
	c := &RunCost{RunID: in.ID, Basis: "list", Totals: []CostTotal{}, ByFamily: []CostTotal{}, Lines: []CostLine{}, Sources: []CostSource{}}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT run_id, source, family, item, trim_scale(amount)::text, currency,
				period_from, period_to, final, details, reported_at
			FROM cost_lines WHERE run_id = $1 ORDER BY family, source, item`, in.ID)
		if err != nil {
			return err
		}
		c.Lines, err = pgx.CollectRows(rows, pgx.RowToStructByPos[CostLine])
		if err != nil {
			return err
		}
		// One pass: GROUPING SETS gives the per-currency totals (family
		// NULL) and the per-family ones.
		rows, err = tx.Query(ctx, `SELECT family, currency, trim_scale(sum(amount))::text,
				trim_scale(coalesce(sum(amount) FILTER (WHERE final), 0))::text,
				trim_scale(coalesce(sum(amount) FILTER (WHERE NOT final), 0))::text
			FROM cost_lines WHERE run_id = $1
			GROUP BY GROUPING SETS ((currency), (family, currency))
			ORDER BY family NULLS FIRST, currency`, in.ID)
		if err != nil {
			return err
		}
		var family *string
		var t CostTotal
		_, err = pgx.ForEachRow(rows, []any{&family, &t.Currency, &t.Amount, &t.Final, &t.Estimate}, func() error {
			if family == nil {
				t.Family = ""
				c.Totals = append(c.Totals, t)
			} else {
				t.Family = *family
				c.ByFamily = append(c.ByFamily, t)
			}
			return nil
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT source, status, answered_at, next_at FROM cost_sources WHERE run_id = $1 ORDER BY source`, in.ID)
		if err != nil {
			return err
		}
		c.Sources, err = pgx.CollectRows(rows, pgx.RowToStructByPos[CostSource])
		return err
	})
	if err != nil {
		return nil, err
	}
	c.Status = costStatus(c.Sources, len(c.Lines) > 0)
	c.Final = c.Status == "final"
	return &runCostOutput{c}, nil
}

// costStatus: incomplete if any source is; final once every source is;
// complete otherwise. Lines without any source row (none is written until
// a producer exists) count as complete.
func costStatus(sources []CostSource, lines bool) string {
	if len(sources) == 0 {
		if lines {
			return "complete"
		}
		return "none"
	}
	final := true
	for _, src := range sources {
		switch src.Status {
		case "incomplete":
			return "incomplete"
		case "ok":
			final = false
		}
	}
	if final {
		return "final"
	}
	return "complete"
}

// costAmount: a decimal with at most 9 fractional digits, what
// numeric(24, 9) holds without rounding.
var costAmount = regexp.MustCompile(`^-?[0-9]{1,15}(\.[0-9]{1,9})?$`)

// costReport is one line as a source reports it. The Run, the source and
// the time reported come from the caller and the database, not the line.
type costReport struct {
	Family   string
	Item     string
	Amount   string
	Currency string
	From     time.Time
	To       time.Time
	Final    bool
	Details  map[string]any
}

// replaceCostLines stores a source's latest answer for a Run: every earlier
// line of that source for the Run goes, in the caller's transaction. An
// answer naming one item twice is refused whole, never summed.
func replaceCostLines(ctx context.Context, tx pgx.Tx, tenantID, runID, source string, lines []costReport) error {
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l.Item] {
			return fmt.Errorf("cost source %s: item %q twice for run %s", source, l.Item, runID)
		}
		seen[l.Item] = true
		if !costAmount.MatchString(l.Amount) {
			return fmt.Errorf("cost source %s: amount %q is not a decimal with up to 9 fractional digits", source, l.Amount)
		}
	}
	// The delete and every insert in one round trip. A failed statement
	// aborts the caller's transaction, so the earlier lines stay.
	b := &pgx.Batch{}
	b.Queue(`DELETE FROM cost_lines WHERE source = $1 AND run_id = $2`, source, runID)
	for _, l := range lines {
		details := l.Details
		if details == nil {
			details = map[string]any{}
		}
		b.Queue(`INSERT INTO cost_lines (tenant_id, run_id, source, item, family, amount, currency,
				period_from, period_to, final, details)
			VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7, $8, $9, $10, $11)`,
			tenantID, runID, source, l.Item, l.Family, l.Amount, l.Currency, l.From, l.To, l.Final, details)
	}
	return tx.SendBatch(ctx, b).Close()
}
