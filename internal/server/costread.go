package server

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type costSummaryInput struct {
	CostLabelQuery
	Group    []string `query:"group,explode" doc:"Repeat up to twice: tenant (operators), pool, host, family, run, key, label:key."`
	Family   string   `query:"family" doc:"Only this family. Not with nofamily."`
	NoFamily string   `query:"nofamily" doc:"Every family but this one."`
	Interval string   `query:"interval" doc:"hour or day; include a time series."`
	Top      int      `query:"top" minimum:"1" maximum:"50" doc:"Keep the first group's N values costing the most per currency (ties by value) and fold the rest into one row per second-group value and currency, marked other: true (its value reads (other)), in totals and series; (none) is never folded nor counted, and the second group is kept. Needs a group. The row limit applies to the folded rows. Totals rows then carry runs, and otherCount says how many values were folded."`
	Rank     string   `query:"rank" enum:"all,compute,external," doc:"With top: all (default), compute, or external (every family but compute): the cost that ranks the values and that runs counts. Refused when it counts none of the families family/nofamily keep."`
	Runs     bool     `query:"runs" doc:"Without top: totals rows carry runs too (with top they always do)."`
}

func (in *costSummaryInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Group = u.Query()["group"]
	in.CostLabelQuery.resolve(u.Query())
	return nil
}

// CostLabelQuery filters cost to Runs by their labels. Exported: huma
// binds only the parameters of exported embedded structs.
type CostLabelQuery struct {
	HistoryQuery
	Label   []string `query:"label,explode" doc:"Only Runs with this label (key=value, split at the first =). Repeat: the same key repeated matches any of its values; different keys must all match."`
	NoLabel []string `query:"nolabel,explode" doc:"Only Runs without this label key. Repeatable."`
}

func (q *CostLabelQuery) resolve(v url.Values) {
	q.Label, q.NoLabel = v["label"], v["nolabel"]
}

const costMaxLabelFilters = 64

// labelFilter is the label and nolabel filters as the scoped CTE's bind
// parameters: {key: [values]} and the keys that must be absent.
func (q *CostLabelQuery) labelFilter() (map[string][]string, []string, error) {
	if len(q.Label)+len(q.NoLabel) > costMaxLabelFilters {
		return nil, nil, errf(http.StatusBadRequest, "bad_request", "at most %d label filters", costMaxLabelFilters)
	}
	want := map[string][]string{}
	for _, l := range q.Label {
		k, v, ok := strings.Cut(l, "=")
		if !ok || !labelKeyRe.MatchString(k) {
			return nil, nil, errf(http.StatusBadRequest, "bad_request", "label %q: want key=value with a valid label key", l)
		}
		if !slices.Contains(want[k], v) {
			want[k] = append(want[k], v)
		}
	}
	absent := []string{}
	for _, k := range q.NoLabel {
		if !labelKeyRe.MatchString(k) {
			return nil, nil, errf(http.StatusBadRequest, "bad_request", "nolabel %q: not a valid label key", k)
		}
		absent = append(absent, k)
	}
	return want, absent, nil
}

// costScopedSQL is the costed Run hours in [$1, $2) of family $3 (or all),
// whose Run's labels match $4 ({key: [values]}: one of the values of every
// key) and lack every key in $5. Filters are values, never SQL text.
const costScopedSQL = `scoped AS (
		SELECT c.hour, c.currency, c.amount, c.tenant_id, c.family, c.run_id,
			coalesce(cp.name, '(none)') AS pool, coalesce(c.host_id, '(none)') AS host, r.labels,
			coalesce(r.submitted_by_key, 'email:' || r.submitted_by_email, '(none)') AS submitter
		FROM cost_hourly c JOIN runs r ON r.id = c.run_id LEFT JOIN pools cp ON cp.id = c.pool_id
		WHERE c.run_id IS NOT NULL AND c.hour >= $1 AND c.hour < $2
			AND ($3 = '' OR c.family = $3)
			AND ($4::jsonb = '{}' OR NOT EXISTS (SELECT 1 FROM jsonb_each($4::jsonb) f
				WHERE (r.labels->>f.key) IN (SELECT jsonb_array_elements_text(f.value)) IS NOT TRUE))
			AND (cardinality($5::text[]) = 0 OR NOT EXISTS (SELECT 1 FROM unnest($5::text[]) k WHERE r.labels ? k))
	)`

// costTx runs read in a read-only snapshot under the principal's RLS scope.
func (s *Server) costTx(ctx context.Context, p Principal, read func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// pgx prepares each statement; from its sixth run Postgres may switch
		// to a generic plan that cannot see the range ($1, $2), the filters
		// or the groups, and these reads then took 1.5x to 3x as long. Set
		// with the scope, in one round trip.
		const custom = `SELECT set_config('plan_cache_mode', 'force_custom_plan', true), `
		if p.TenantID == "" {
			if _, err := tx.Exec(ctx, custom+`set_config('lux.system', 'on', true)`); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, custom+`set_config('lux.tenant_id', $1, true)`, p.TenantID); err != nil {
			return err
		}
		return read(tx)
	})
}

type CostSummaryRow struct {
	At       *time.Time        `json:"at,omitempty"`
	Group    map[string]string `json:"group,omitempty"`
	Currency string            `json:"currency"`
	Amount   string            `json:"amount"`
	Runs     *int              `json:"runs,omitempty" doc:"With top or runs, on totals: the Runs with cost (that rank counts, with top) under this row's first-group value, across the second group."`
	Other    bool              `json:"other,omitempty" doc:"With top: the first-group value is the fold of every value past the top N, not a value of its own (a real value may also read (other))."`
	Family   string            `json:"family,omitempty" doc:"On unallocated rows: the host-tied family (compute or block-storage)."`
}

type HostAllocation struct {
	HostID      string `json:"hostId"`
	Family      string `json:"family" doc:"A host-tied family: compute or block-storage."`
	Currency    string `json:"currency"`
	Allocated   string `json:"allocated"`
	Unallocated string `json:"unallocated"`
}

type CostSummaryBody struct {
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Basis       string           `json:"basis"`
	Totals      []CostSummaryRow `json:"totals"`
	Series      []CostSummaryRow `json:"series,omitempty"`
	Unallocated []CostSummaryRow `json:"unallocated,omitempty" doc:"Unfiltered summaries: hosts' cost charged to no Run, per family and currency. An operator over every tenant: every host's; a tenant (or an operator narrowed to one): the hosts of its own pools, never a platform pool's."`
	Hosts       []HostAllocation `json:"hosts,omitempty" doc:"Grouped by host, unfiltered: each host's allocated and unallocated cost per family and currency, with the visibility of unallocated."`
	Families    []CostFamilyInfo `json:"families,omitempty" doc:"Grouped by family: each family in totals, with the displayName and color byFamily has on a Run's cost."`
	Runs        []CostRunInfo    `json:"runs,omitempty" doc:"Grouped by run: each Run in totals with its name and labels."`
	Keys        []CostKeyInfo    `json:"keys,omitempty" doc:"Grouped by key: each submitter in totals. (none) is Runs from before luxd recorded who submitted them."`
	OtherCount  map[string]int   `json:"otherCount,omitempty" doc:"With top: per currency, how many values the other: true rows hold; a currency with none folded is absent."`
}

type CostKeyInfo struct {
	ID       string `json:"id" doc:"The group value: an API key id, or email:<address> for a person."`
	Name     string `json:"name,omitempty" doc:"The key's name; absent for an operator's key when a tenant asks."`
	Operator bool   `json:"operator,omitempty" doc:"An operator's key."`
	Revoked  bool   `json:"revoked,omitempty"`
	Email    string `json:"email,omitempty" doc:"A person's address."`
}

type CostRunInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type CostFamilyInfo struct {
	Family      string `json:"family"`
	DisplayName string `json:"displayName,omitempty"`
	Color       string `json:"color,omitempty"`
}

type costSummaryOutput struct {
	Body CostSummaryBody `nameHint:"CostSummary"`
}

const (
	costMaxRange = 90 * 24 * time.Hour
	costMaxRows  = 10000
)

// fold is top (0: none) and what it ranks by. Range and enum are checked
// by huma (422); these are the combinations the schema cannot express.
func (in *costSummaryInput) fold() (int, string, error) {
	if in.Family != "" && in.NoFamily != "" {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "family and nofamily exclude each other")
	}
	if in.Top == 0 {
		if in.Rank != "" {
			return 0, "", errf(http.StatusBadRequest, "bad_request", "rank needs top")
		}
		return 0, "all", nil
	}
	if len(in.Group) == 0 {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "top needs a group")
	}
	rank := in.Rank
	if rank == "" {
		rank = "all"
	}
	// A rank that counts none of the families kept would rank by value alone.
	if rank == "compute" && (in.Family != "" && in.Family != "compute" || in.NoFamily == "compute") ||
		rank == "external" && in.Family == "compute" {
		return 0, "", errf(http.StatusBadRequest, "bad_request", "rank=%s counts none of the families kept", rank)
	}
	return in.Top, rank, nil
}

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
		case g == "tenant" && p.Operator, g == "pool", g == "host", g == "family", g == "run", g == "key":
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
	want, absent, err := in.labelFilter()
	if err != nil {
		return nil, err
	}
	top, rank, err := in.fold()
	if err != nil {
		return nil, err
	}
	filtered := len(want) > 0 || len(absent) > 0
	out := &costSummaryOutput{Body: CostSummaryBody{From: from, To: to, Basis: "list", Totals: []CostSummaryRow{}}}
	// Group selectors and label keys are values, never SQL identifiers.
	const base = `WITH ` + costScopedSQL + `, dimensions AS (
		SELECT hour, currency, amount, run_id, ($10 = 'all' OR (family = 'compute') = ($10 = 'compute')) AS ranks,
			CASE $6::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$8, '(none)') END AS g1,
			CASE $7::text WHEN 'tenant' THEN tenant_id WHEN 'pool' THEN pool WHEN 'host' THEN host WHEN 'key' THEN submitter
				WHEN 'family' THEN family WHEN 'run' THEN run_id WHEN 'label' THEN coalesce(labels->>$9, '(none)') END AS g2
		FROM scoped WHERE $12 = '' OR family <> $12
	)`
	// The fold ranks g1 per currency by the cost rank counts (a value with
	// none ranks last), then marks every value past $11 other and names it
	// (other); other stays a column so a real value (other) is not merged
	// with the fold. nvals is every value but (none): the folded ones are
	// nvals - $11. vals is read by the totals only: Runs per value, across g2.
	// ranked is MATERIALIZED so its window runs once, but that alone is not
	// enough: any join from dimensions to it (whose rows Postgres estimates
	// at ~1) may become a nested loop that scans it once per cost row (op 7d
	// Top Runs: 8 s). kept therefore reduces it to one row, read by shaped
	// only through uncorrelated scalar subqueries, which run once.
	const folded = `, sums AS (
		SELECT g1, currency, sum(amount) FILTER (WHERE ranks) AS total FROM dimensions WHERE g1 <> '(none)' GROUP BY 1, 2
	), ranked AS MATERIALIZED (
		SELECT g1, currency, row_number() OVER (PARTITION BY currency ORDER BY total DESC NULLS LAST, g1 COLLATE "C") AS n,
			count(*) OVER (PARTITION BY currency) AS nvals
		FROM sums
	), kept AS MATERIALIZED (
		SELECT coalesce(array_agg(currency || E'\x1f' || g1) FILTER (WHERE n <= $11), '{}') AS keys,
			jsonb_object_agg(currency, nvals) AS nvals
		FROM ranked
	), marked AS (
		SELECT d.*, coalesce(d.g1 <> '(none)' AND d.currency || E'\x1f' || d.g1 <> ALL ((SELECT keys FROM kept)::text[]), false) AS other
		FROM dimensions d
	), shaped AS (
		SELECT hour, currency, amount, run_id, ranks, g2, other,
			CASE WHEN g1 <> '(none)' THEN ((SELECT nvals FROM kept) ->> currency)::int END AS nvals,
			CASE WHEN other THEN '(other)' ELSE g1 END AS g1
		FROM marked
	), vals AS (
		SELECT g1, currency, other, (count(*) FILTER (WHERE ranks))::int AS runs, max(nvals)::int AS nvals
		FROM (SELECT DISTINCT g1, currency, other, run_id, ranks, nvals FROM shaped) d GROUP BY 1, 2, 3
	) `
	const plain = `, shaped AS (SELECT *, false AS other FROM dimensions WHERE $11::int = 0) `
	// A hashed DISTINCT, then count(*): count(DISTINCT) sorts every cost row.
	const plainRuns = plain + `, vals AS (
		SELECT g1, currency, other, count(*)::int AS runs, NULL::int AS nvals
		FROM (SELECT DISTINCT g1, currency, other, run_id FROM shaped) d GROUP BY 1, 2, 3
	) `
	query := base + plain
	switch {
	case top > 0:
		query = base + folded
	case in.Runs:
		query = base + plainRuns
	}
	args := []any{from, to, in.Family, want, absent, group[0], group[1], label[0], label[1], rank, top, in.NoFamily}
	rowCount := 0
	read := func(tx pgx.Tx, interval string) ([]CostSummaryRow, error) {
		bucket := "NULL::timestamptz"
		switch interval {
		case "hour":
			bucket = "date_trunc('hour', hour)"
		case "day":
			bucket = "date_trunc('day', hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
		}
		grouped := `SELECT ` + bucket + ` AS at, g1, g2, currency, other, trim_scale(sum(amount))::text AS amount FROM shaped GROUP BY 1, 2, 3, 4, 5`
		var sql string
		if (top > 0 || in.Runs) && interval == "" {
			// vals is read once, as a map keyed like each row, through a scalar
			// subquery: joined, both sides estimate at ~1 row and the planner
			// nests the loop, comparing every row with every value (op 7d
			// group=run&runs=true: 10.3M comparisons). currency holds no \x1f,
			// and a NULL g1 (no group, no pool, no host) keys apart from any value.
			const valKey = `(currency || E'\x1f' || other::text || coalesce(E'\x1f' || g1, ''))`
			sql = query + `, valmap AS MATERIALIZED (SELECT jsonb_object_agg(` + valKey + `, jsonb_build_array(runs, nvals)) AS m FROM vals)
				SELECT at, g1, g2, currency, other, amount,
					((SELECT m FROM valmap) -> ` + valKey + ` ->> 0)::int, ((SELECT m FROM valmap) -> ` + valKey + ` ->> 1)::int
				FROM (` + grouped + `) b ORDER BY 1 NULLS FIRST, 2, 3, 4, 5`
		} else {
			sql = query + `SELECT *, NULL::int, NULL::int FROM (` + grouped + `) b ORDER BY 1 NULLS FIRST, 2, 3, 4, 5`
		}
		result := []CostSummaryRow{}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var row CostSummaryRow
			var g1, g2 *string
			var nvals *int
			if err := rows.Scan(&row.At, &g1, &g2, &row.Currency, &row.Other, &row.Amount, &row.Runs, &nvals); err != nil {
				return nil, err
			}
			if row.Other && nvals != nil {
				if out.Body.OtherCount == nil {
					out.Body.OtherCount = map[string]int{}
				}
				out.Body.OtherCount[row.Currency] = *nvals - top
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
	err = s.costTx(ctx, p, func(tx pgx.Tx) error {
		var err error
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
		if group[0] == "run" || group[1] == "run" {
			if out.Body.Runs, err = summaryRuns(ctx, tx, out.Body.Totals, in.Group[0]); err != nil {
				return err
			}
		}
		// Unallocated cost belongs to no Run, so it has no labels to filter.
		// Host rows are read in the caller's scope: an operator over every
		// tenant sees every host's, a tenant (or an operator narrowed to one)
		// only those of hosts in its own pools (RLS, cost_hourly_own_hosts).
		// The explicit pool predicate lets the planner use
		// cost_hourly_host_pool_hour per owned pool instead of filtering every
		// tenant's host rows.
		if !filtered {
			args, ownPools := []any{from, to, in.Family, in.NoFamily}, ""
			if p.TenantID != "" {
				args, ownPools = append(args, p.TenantID), ` AND pool_id IN (SELECT id FROM pools WHERE tenant_id = $5)`
			}
			out.Body.Unallocated = []CostSummaryRow{}
			rows, err := tx.Query(ctx, `SELECT family, currency, trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3) AND ($4 = '' OR family <> $4)`+ownPools+`
				GROUP BY family, currency ORDER BY currency, family`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var row CostSummaryRow
				if err := rows.Scan(&row.Family, &row.Currency, &row.Amount); err != nil {
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
			out.Body.Hosts = []HostAllocation{}
			rows, err = tx.Query(ctx, `SELECT host_id, family, currency, trim_scale(sum(allocated))::text,
				trim_scale(sum(unallocated))::text FROM cost_hourly
				WHERE run_id IS NULL AND hour >= $1 AND hour < $2 AND ($3 = '' OR family = $3) AND ($4 = '' OR family <> $4)`+ownPools+`
				GROUP BY host_id, family, currency ORDER BY host_id, currency, family`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var row HostAllocation
				if err := rows.Scan(&row.HostID, &row.Family, &row.Currency, &row.Allocated, &row.Unallocated); err != nil {
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
	if group[0] == "family" || group[1] == "family" {
		out.Body.Families = s.summaryFamilies(out.Body.Totals, in.Group[0])
	}
	if group[0] == "key" || group[1] == "key" {
		if out.Body.Keys, err = s.summaryKeys(ctx, p, out.Body.Totals, in.Group[0]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// groupValue is r's value of group g; false for the fold's row of the
// first group (r.Other), which names no value of its own.
func groupValue(r CostSummaryRow, g, first string) (string, bool) {
	if r.Other && g == first {
		return "", false
	}
	return r.Group[g], true
}

// summaryKeys names each submitter in rows. The ids come from Runs the
// caller sees; keys are read as the system and named by keyName.
func (s *Server) summaryKeys(ctx context.Context, p Principal, rows []CostSummaryRow, first string) ([]CostKeyInfo, error) {
	seen := map[string]bool{}
	for _, r := range rows {
		if v, ok := groupValue(r, "key", first); ok && v != "(none)" {
			seen[v] = true
		}
	}
	all := slices.Sorted(maps.Keys(seen))
	out := make([]CostKeyInfo, 0, len(all))
	var keyIDs []string
	for _, v := range all {
		if email, ok := strings.CutPrefix(v, "email:"); ok {
			out = append(out, CostKeyInfo{ID: v, Email: email})
		} else {
			keyIDs = append(keyIDs, v)
		}
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, tenant_id, revoked_at IS NOT NULL
			FROM api_keys WHERE id = ANY($1) ORDER BY id`, keyIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k CostKeyInfo
			var name string
			var keyTenant *string
			var revoked bool
			if err := rows.Scan(&k.ID, &name, &keyTenant, &revoked); err != nil {
				return err
			}
			k.Operator = keyTenant == nil
			k.Name, k.Revoked = keyName(p, keyTenant, name, revoked)
			out = append(out, k)
		}
		return rows.Err()
	})
	slices.SortFunc(out, func(a, b CostKeyInfo) int { return strings.Compare(a.ID, b.ID) })
	return out, err
}

type costLabelsInput struct {
	CostLabelQuery
}

func (in *costLabelsInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.CostLabelQuery.resolve(u.Query())
	return nil
}

type CostLabelKey struct {
	Key  string `json:"key"`
	Runs int    `json:"runs" doc:"Runs with cost in the range that carry this label."`
}

type costLabelsOutput struct {
	Body struct {
		From time.Time      `json:"from"`
		To   time.Time      `json:"to"`
		Keys []CostLabelKey `json:"keys" doc:"Most Runs first, then by key."`
	} `nameHint:"CostLabels"`
}

// costLabels lists the label keys on Runs with cost in the range (after
// the label filters), for picking a breakdown or a filter; the values and
// their cost are GET /v1/costs?group=label:<key>.
func (s *Server) costLabels(ctx context.Context, in *costLabelsInput) (*costLabelsOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	if from, to, err = costRange(from, to); err != nil {
		return nil, err
	}
	want, absent, err := in.labelFilter()
	if err != nil {
		return nil, err
	}
	out := &costLabelsOutput{}
	out.Body.From, out.Body.To, out.Body.Keys = from, to, []CostLabelKey{}
	err = s.costTx(ctx, p, func(tx pgx.Tx) error {
		// Each Run's labels once, not once per cost hour and family.
		rows, err := tx.Query(ctx, `WITH `+costScopedSQL+`, labelled AS (SELECT DISTINCT run_id, labels FROM scoped)
			SELECT k, count(*)::int FROM labelled, jsonb_object_keys(labels) k
			GROUP BY k ORDER BY 2 DESC, 1`, from, to, "", want, absent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k CostLabelKey
			if err := rows.Scan(&k.Key, &k.Runs); err != nil {
				return err
			}
			out.Body.Keys = append(out.Body.Keys, k)
			if err := costRowLimit(len(out.Body.Keys)); err != nil {
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

// summaryFamilies names each family in rows once, sorted, as byFamily does.
func (s *Server) summaryFamilies(rows []CostSummaryRow, first string) []CostFamilyInfo {
	seen := map[string]bool{}
	for _, r := range rows {
		if f, ok := groupValue(r, "family", first); ok {
			seen[f] = true
		}
	}
	names := slices.Sorted(maps.Keys(seen))
	meta := s.costFamilyMetadata()
	out := make([]CostFamilyInfo, 0, len(names))
	for _, family := range names {
		name, color := meta.lookup(family)
		out = append(out, CostFamilyInfo{Family: family, DisplayName: name, Color: color})
	}
	return out
}

// summaryRuns names each Run in rows, in one query in the summary's snapshot.
func summaryRuns(ctx context.Context, tx pgx.Tx, rows []CostSummaryRow, first string) ([]CostRunInfo, error) {
	seen := map[string]bool{}
	for _, r := range rows {
		if id, ok := groupValue(r, "run", first); ok {
			seen[id] = true
		}
	}
	ids := slices.Sorted(maps.Keys(seen))
	names, err := tx.Query(ctx, `SELECT id, name, nullif(labels, '{}') FROM runs WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(names, pgx.RowToStructByPos[CostRunInfo])
}

type hostCostInput struct {
	HostPath
	HistoryQuery
}
type hostCostHour struct {
	Hour        time.Time `json:"hour"`
	Family      string    `json:"family" doc:"A host-tied family: compute or block-storage."`
	Currency    string    `json:"currency"`
	Allocated   string    `json:"allocated"`
	Unallocated *string   `json:"unallocated,omitempty" doc:"Operators, and the tenant owning the host's pool; absent for a tenant on a platform pool's host."`
}
type hostCostRate struct {
	Family   string         `json:"family" doc:"A host-tied family: compute or block-storage."`
	From     time.Time      `json:"from"`
	To       *time.Time     `json:"to,omitempty"`
	PerHour  string         `json:"perHour"`
	Currency string         `json:"currency"`
	Source   string         `json:"source"`
	Details  map[string]any `json:"details,omitempty" doc:"What the period was priced from: for block storage, the volumes, the unit prices per volume type (perGBMonth, perIOPSMonth, perGiBpsMonth) and hoursPerMonth."`
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
		// A tenant reads its own host's cost, and its unallocated part only
		// while the host is in one of its own pools, never a platform pool.
		seesUnallocated := p.TenantID == ""
		if p.TenantID != "" {
			var own bool
			if err := tx.QueryRow(ctx, `SELECT h.tenant_id IS NOT DISTINCT FROM $2, coalesce(pl.tenant_id = $2, false)
				FROM hosts h LEFT JOIN pools pl ON pl.id = h.pool_id WHERE h.id = $1`, id, p.TenantID).Scan(&own, &seesUnallocated); err != nil {
				return err
			}
			if !own {
				return errf(http.StatusForbidden, "forbidden", "a host's cost is its owner's or the operators'")
			}
		}
		out.Body.HostID = id
		rows, err := tx.Query(ctx, `SELECT hour, family, currency, trim_scale(sum(allocated))::text,
			trim_scale(sum(unallocated))::text FROM cost_hourly
			WHERE host_id = $1 AND run_id IS NULL AND hour >= $2 AND hour < $3
			GROUP BY hour, family, currency ORDER BY hour, currency, family`, id, from, to)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h hostCostHour
			if err := rows.Scan(&h.Hour, &h.Family, &h.Currency, &h.Allocated, &h.Unallocated); err != nil {
				rows.Close()
				return err
			}
			if !seesUnallocated {
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
		rows, err = tx.Query(ctx, `SELECT family, valid_from, valid_to, trim_scale(per_hour)::text, currency, source, details
			FROM host_rates WHERE host_id = $1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
			ORDER BY family DESC, valid_from`, id, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r hostCostRate
			if err := rows.Scan(&r.Family, &r.From, &r.To, &r.PerHour, &r.Currency, &r.Source, &r.Details); err != nil {
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
