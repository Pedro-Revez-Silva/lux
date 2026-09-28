package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

// basisNote is said once per command: amounts are list prices.
const basisNote = "list price, before discounts, credits and tax"

// getRaw GETs path, keeping the body as luxd sent it (for -o json) and
// decoding it into out.
func (a *app) getRaw(cmd *cobra.Command, path string, out any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := a.c.Do(ctxOf(cmd), "GET", path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, json.Unmarshal(raw, out)
}

func (a *app) costCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cost <run>",
		Short: "What a Run cost: totals per currency, families, lines and sources",
		Long: `What a Run cost, as its cost sources reported it: compute (the hosts it
ran on) and each cost plugin. Totals are per currency, never added across
currencies, and split into the final part and the estimate that may still
change. The status is pending until a source reports, incomplete while a
source has not answered, complete, or final once every source has settled.

Amounts are list prices, rounded half-even to 4 decimals; -o json prints
luxd's response as it came, with exact amounts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var c server.RunCost
			raw, err := a.getRaw(cmd, "/v1/runs/"+url.PathEscape(args[0])+"/cost", &c)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(raw)
			}
			a.renderRunCost(&c)
			return nil
		},
	}
}

func (a *app) renderRunCost(c *server.RunCost) {
	w := a.stdout
	fmt.Fprintf(w, "run:     %s (%s)\n", c.RunID, basisNote)
	fmt.Fprintf(w, "status:  %s\n", costStatusText(c))
	if len(c.Totals) == 0 {
		fmt.Fprintf(w, "total:   %s\n", noValue)
	} else {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(c.Totals))
		for _, t := range c.Totals {
			rows = append(rows, []string{money(t.Amount, t.Currency), money(t.Final, t.Currency), money(t.Estimate, t.Currency)})
		}
		a.table("TOTAL\tFINAL\tESTIMATE", rows)
	}
	names := map[string]string{}
	if len(c.ByFamily) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(c.ByFamily))
		for _, t := range c.ByFamily {
			names[t.Family] = familyName(t)
			rows = append(rows, []string{familyName(t), money(t.Amount, t.Currency), money(t.Final, t.Currency), money(t.Estimate, t.Currency)})
		}
		a.table("FAMILY\tAMOUNT\tFINAL\tESTIMATE", rows)
	}
	if len(c.Lines) > 0 {
		fmt.Fprintln(w)
		// Lines come ordered by family: the family is named on its first line.
		rows := make([][]string, 0, len(c.Lines))
		prev := ""
		for i, l := range c.Lines {
			family := ""
			if i == 0 || l.Family != prev {
				family = cmpOr(names[l.Family], l.Family)
			}
			prev = l.Family
			kind := "estimate"
			if l.Final {
				kind = "final"
			}
			rows = append(rows, []string{family, cmpOr(l.Item, noValue), l.Source, money(l.Amount, ""), l.Currency,
				timeRange(l.From, l.To), kind})
		}
		a.table("FAMILY\tITEM\tSOURCE\tAMOUNT\tCURRENCY\tFROM–TO\tKIND", rows)
	}
	if len(c.Sources) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(c.Sources))
		for _, s := range c.Sources {
			answered := noValue
			if s.AnsweredAt != nil {
				answered = s.AnsweredAt.Local().Format("Jan 2 15:04:05")
			}
			rows = append(rows, []string{s.Source, s.Status, answered})
		}
		a.table("SOURCE\tSTATUS\tANSWERED", rows)
	}
}

// costStatusText is the status, naming the sources an incomplete one waits on.
func costStatusText(c *server.RunCost) string {
	switch c.Status {
	case "pending":
		return "pending (no cost reported yet)"
	case "incomplete":
		var waiting []string
		for _, s := range c.Sources {
			if s.Status == "incomplete" {
				waiting = append(waiting, s.Source)
			}
		}
		if len(waiting) > 0 {
			return "incomplete (not answered: " + strings.Join(waiting, ", ") + ")"
		}
	}
	return c.Status
}

func familyName(t server.CostTotal) string {
	if t.DisplayName != "" && t.DisplayName != t.Family {
		return t.DisplayName + " (" + t.Family + ")"
	}
	return t.Family
}

func timeRange(from, to time.Time) string {
	f, t := from.Local(), to.Local()
	end := t.Format("Jan 2 15:04")
	if f.YearDay() == t.YearDay() && f.Year() == t.Year() {
		end = t.Format("15:04")
	}
	return f.Format("Jan 2 15:04") + "–" + end
}

func (a *app) costsCmd() *cobra.Command {
	var since, from, to, family, interval string
	var by []string
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Costs over a time range, by currency and up to two groups",
		Long: `Costs over a range of whole UTC hours (--since, default 7d, as lux
history; or --from/--to, RFC 3339), per currency, grouped by up to two of
--by tenant (operators), pool, host, family, run or label:KEY. Grouping by
pool or host applies to compute; other families show as "(none)".
--interval hour|day adds a series (in UTC). With an operator key and no --tenant,
the hosts' cost not charged to any Run is shown as unallocated.

Amounts are list prices, rounded half-even to 4 decimals; -o json prints
luxd's response as it came.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from != "" && cmd.Flags().Changed("since") {
				return errors.New("--since and --from are alternatives: give one")
			}
			q := url.Values{}
			if from == "" {
				q.Set("since", since)
			} else {
				q.Set("from", from)
			}
			if to != "" {
				q.Set("to", to)
			}
			for _, g := range by {
				q.Add("group", g)
			}
			if family != "" {
				q.Set("family", family)
			}
			if interval != "" {
				q.Set("interval", interval)
			}
			var s server.CostSummaryBody
			raw, err := a.getRaw(cmd, "/v1/costs?"+q.Encode(), &s)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(raw)
			}
			a.renderCostSummary(&s, by, interval)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "7d", "how far back from now (e.g. 24h, 7d)")
	cmd.Flags().StringVar(&from, "from", "", "the start (RFC 3339), instead of --since")
	cmd.Flags().StringVar(&to, "to", "", "the end (RFC 3339); default now")
	cmd.Flags().StringArrayVar(&by, "by", nil, "group by tenant|pool|host|family|run|label:KEY (repeat for two levels)")
	cmd.Flags().StringVar(&family, "family", "", "only this family")
	cmd.Flags().StringVar(&interval, "interval", "", "hour or day: also print a series")
	return cmd
}

func (a *app) renderCostSummary(s *server.CostSummaryBody, by []string, interval string) {
	w := a.stdout
	// Buckets are whole UTC hours and days, so the range and series are shown in UTC.
	fmt.Fprintf(w, "%s → %s UTC (%s)\n", s.From.UTC().Format("Jan 2 15:04"), s.To.UTC().Format("Jan 2 15:04"), basisNote)
	header := make([]string, 0, len(by)+2)
	for _, g := range by {
		header = append(header, strings.ToUpper(g))
	}
	rowOf := func(r server.CostSummaryRow) []string {
		row := make([]string, 0, len(by)+2)
		for _, g := range by {
			row = append(row, r.Group[g])
		}
		return append(row, money(r.Amount, r.Currency))
	}
	fmt.Fprintln(w)
	if len(s.Totals) == 0 {
		fmt.Fprintln(w, "no costs in this range")
	} else {
		rows := make([][]string, 0, len(s.Totals))
		for _, r := range s.Totals {
			rows = append(rows, rowOf(r))
		}
		a.table(strings.Join(append(header, "TOTAL"), "\t"), rows)
	}
	if interval != "" && len(s.Series) > 0 {
		fmt.Fprintln(w)
		layout := "Jan 2 15:04"
		if interval == "day" {
			layout = "Jan 2"
		}
		rows := make([][]string, 0, len(s.Series))
		for _, r := range s.Series {
			at := noValue
			if r.At != nil {
				at = r.At.UTC().Format(layout)
			}
			rows = append(rows, append([]string{at}, rowOf(r)...))
		}
		a.table(strings.Join(append(append([]string{strings.ToUpper(interval)}, header...), "AMOUNT"), "\t"), rows)
	}
	if len(s.Unallocated) > 0 {
		fmt.Fprintln(w)
		amounts := make([]string, 0, len(s.Unallocated))
		for _, r := range s.Unallocated {
			amounts = append(amounts, money(r.Amount, r.Currency))
		}
		fmt.Fprintf(w, "unallocated (hosts' cost charged to no Run): %s\n", strings.Join(amounts, ", "))
	}
	if len(s.Hosts) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(s.Hosts))
		for _, h := range s.Hosts {
			rows = append(rows, []string{h.HostID, money(h.Allocated, h.Currency), money(h.Unallocated, h.Currency)})
		}
		a.table("HOST\tALLOCATED\tUNALLOCATED", rows)
	}
}
