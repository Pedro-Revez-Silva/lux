package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

// topFixture: t1 Runs a1..a5 labelled app=a1..a5 and n1 without app, each
// costing USD compute per hour for two hours (a1 most), a5 also EUR ai, and
// a4 USD ai only.
func topFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	for i, amount := range []string{"50", "40", "30", "20", "10"} {
		id := fmt.Sprintf("a%d", i+1)
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ($1, 't1', '{}', 'running', jsonb_build_object('app', $1::text))`, id)
		execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
			VALUES ($1, 't1', $3, 'compute', 'compute', 'USD', $4::numeric, 0, 0), ($2, 't1', $3, 'compute', 'compute', 'USD', $4::numeric, 0, 0)`,
			t0, t0.Add(time.Hour), id, amount)
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ('n1', 't1', '{}', 'running', '{}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated) VALUES
		($1, 't1', 'n1', 'compute', 'compute', 'USD', 1000, 0, 0),
		($1, 't1', 'a5', 'plugin', 'ai', 'EUR', 9, 0, 0),
		($1, 't1', 'a1', 'plugin', 'ai', 'EUR', 1, 0, 0),
		($1, 't1', 'a3', 'plugin', 'ai', 'EUR', 1, 0, 0),
		($1, 't1', 'a4', 'plugin', 'ai', 'USD', 100, 0, 0)`, t0)
	return s, keys
}

// byGroup is totals as "value currency" -> amount for group key g.
func byGroup(rows []CostSummaryRow, g string) map[string]string {
	m := map[string]string{}
	for _, r := range rows {
		m[r.Group[g]+" "+r.Currency] = r.Amount
	}
	return m
}

func TestCostSummaryTop(t *testing.T) {
	s, keys := topFixture(t)
	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&top=2&interval=hour"), &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	// USD all: a4 140, a1 100, then a2 80, a3 60, a5 20 folded; (none) 1000 kept and not counted.
	// EUR: a5 9, then a1 1 and a3 1 tie broken by value: a1 kept, a3 folded.
	want := map[string]string{
		"a4 USD": "140", "a1 USD": "100", "(other) USD": "160", "(none) USD": "1000",
		"a5 EUR": "9", "a1 EUR": "1", "(other) EUR": "1",
	}
	if m := byGroup(got.Totals, "label:app"); !maps.Equal(m, want) {
		t.Errorf("totals %v, want %v", m, want)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 3, "EUR": 1}) {
		t.Errorf("otherCount %v", got.OtherCount)
	}
	runs := map[string]int{}
	for _, r := range got.Totals {
		if r.Runs == nil {
			t.Fatalf("totals row without runs: %+v", r)
		}
		runs[r.Group["label:app"]+" "+r.Currency] = *r.Runs
	}
	if !maps.Equal(runs, map[string]int{"a4 USD": 1, "a1 USD": 1, "(other) USD": 3, "(none) USD": 1, "a5 EUR": 1, "a1 EUR": 1, "(other) EUR": 1}) {
		t.Errorf("runs %v", runs)
	}
	// The series folds the same way: every bucket sums to the totals.
	series := map[string]float64{}
	for _, r := range got.Series {
		if r.Runs != nil {
			t.Errorf("series row with runs: %+v", r)
		}
		var f float64
		fmt.Sscan(r.Amount, &f)
		series[r.Group["label:app"]+" "+r.Currency] += f
	}
	if series["(other) USD"] != 160 || series["(none) USD"] != 1000 || series["a4 USD"] != 140 || len(series) != len(want) {
		t.Errorf("series sums %v", series)
	}

	// Rank by compute: a4's AI no longer lifts it; the second group is kept under the fold.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&group=family&top=2&rank=compute"), &got); code != http.StatusOK {
		t.Fatalf("rank status %d", code)
	}
	split := map[string]string{}
	for _, r := range got.Totals {
		split[r.Group["label:app"]+" "+r.Group["family"]+" "+r.Currency] = r.Amount
	}
	wantSplit := map[string]string{
		"a1 compute USD": "100", "a2 compute USD": "80",
		"(other) compute USD": "120", "(other) ai USD": "100", "(none) compute USD": "1000",
		// No EUR value has compute: all three rank last, by value.
		"a1 ai EUR": "1", "a3 ai EUR": "1", "(other) ai EUR": "9",
	}
	if !maps.Equal(split, wantSplit) {
		t.Errorf("rank=compute totals %v, want %v", split, wantSplit)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 3, "EUR": 1}) {
		t.Errorf("rank=compute otherCount %v", got.OtherCount)
	}

	// N at least the values: nothing folds, no otherCount.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&top=10"), &got); code != http.StatusOK || got.OtherCount != nil || len(got.Totals) != 9 {
		t.Errorf("top above values: %d %+v", code, got)
	}
	// Grouped by run, the names are the kept Runs' (one per currency), never (other).
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&group=family&top=1"), &got); code != http.StatusOK || len(got.Runs) != 2 || got.Runs[0].ID != "a5" || got.Runs[1].ID != "n1" {
		t.Errorf("run names: %d %+v", code, got.Runs)
	}

	// The schema refuses top out of range and an unknown rank (422); the
	// handler refuses what the schema cannot say (400).
	for _, q := range []string{"&group=run&top=0", "&group=run&top=51", "&group=run&top=x", "&group=run&top=3&rank=ai"} {
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", q, code)
		}
	}
	for _, q := range []string{
		"&top=3", "&group=run&rank=compute",
		"&group=run&family=compute&nofamily=compute", "&group=run&family=ai&nofamily=compute",
		"&group=run&top=3&family=ai&rank=compute", "&group=run&top=3&nofamily=compute&rank=compute",
		"&group=run&top=3&family=compute&rank=external",
	} {
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	for _, q := range []string{"&group=run&top=50", "&group=run&top=3&family=compute&rank=compute", "&group=run&top=3&nofamily=compute&rank=external", "&group=run&top=3&family=ai&rank=external"} {
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusOK {
			t.Errorf("%s: status %d", q, code)
		}
	}

	// nofamily=compute: compute is gone from totals and series; USD has one
	// value left (nothing folds), EUR folds a3.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&top=2&rank=external&nofamily=compute&interval=hour"), &got); code != http.StatusOK {
		t.Fatalf("nofamily status %d", code)
	}
	if m, want := byGroup(got.Totals, "label:app"), map[string]string{"a4 USD": "100", "a5 EUR": "9", "a1 EUR": "1", "(other) EUR": "1"}; !maps.Equal(m, want) {
		t.Errorf("nofamily totals %v, want %v", m, want)
	}
	if m, want := seriesByHour(got.Series, "label:app"), map[string]string{"10 a4 USD": "100", "10 a5 EUR": "9", "10 a1 EUR": "1", "10 (other) other EUR": "1"}; !maps.Equal(m, want) {
		t.Errorf("nofamily series %v, want %v", m, want)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"EUR": 1}) {
		t.Errorf("nofamily otherCount %v", got.OtherCount)
	}

	// runs counts the Runs with cost rank counts: compute-only Runs count 0 under external.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&top=2&rank=external"), &got); code != http.StatusOK {
		t.Fatalf("rank=external status %d", code)
	}
	if m, want := runsByGroup(got.Totals, "label:app"), map[string]int{"a4 USD": 1, "a1 USD": 0, "(other) USD": 0, "(none) USD": 0, "a5 EUR": 1, "a1 EUR": 1, "(other) EUR": 1}; !maps.Equal(m, want) {
		t.Errorf("rank=external runs %v, want %v", m, want)
	}

	// otherCount is per currency: USD keeps a4 140, a1 100, a2 80 and folds
	// a3 and a5; EUR has 3 values and folds none.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&top=3"), &got); code != http.StatusOK {
		t.Fatalf("top=3 status %d", code)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 2}) {
		t.Errorf("top=3 otherCount %v, want USD 2 only", got.OtherCount)
	}

	// runs=true without top: Runs per family and currency, nothing folded.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=family&runs=true&interval=hour"), &got); code != http.StatusOK {
		t.Fatalf("runs=true status %d", code)
	}
	if m, want := runsByGroup(got.Totals, "family"), map[string]int{"compute USD": 6, "ai USD": 1, "ai EUR": 3}; !maps.Equal(m, want) || got.OtherCount != nil {
		t.Errorf("runs=true runs %v, want %v (otherCount %v)", m, want, got.OtherCount)
	}
	for _, r := range got.Series {
		if r.Runs != nil {
			t.Errorf("runs=true: series row with runs: %+v", r)
		}
	}
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=family"), &got); code != http.StatusOK || slices.ContainsFunc(got.Totals, func(r CostSummaryRow) bool { return r.Runs != nil }) {
		t.Errorf("without runs=true: %d %+v", code, got.Totals)
	}
	// runs=true by Run: each Run counts once per currency, on every family row of
	// it (a4 has compute and ai in USD); with no group, once per currency.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&runs=true"), &got); code != http.StatusOK {
		t.Fatalf("group=run runs=true status %d", code)
	}
	if m, want := runsByGroup(got.Totals, "run"), map[string]int{
		"a1 USD": 1, "a2 USD": 1, "a3 USD": 1, "a4 USD": 1, "a5 USD": 1, "n1 USD": 1, "a1 EUR": 1, "a3 EUR": 1, "a5 EUR": 1,
	}; !maps.Equal(m, want) || len(got.Totals) != len(want) || byGroup(got.Totals, "run")["a4 USD"] != "140" {
		t.Errorf("group=run runs=true %v (%d rows, a4 %s), want %v", m, len(got.Totals), byGroup(got.Totals, "run")["a4 USD"], want)
	}
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&group=family&runs=true"), &got); code != http.StatusOK {
		t.Fatalf("group=run runs=true status %d", code)
	}
	byRun := map[string]int{}
	for _, r := range got.Totals {
		byRun[rowKey(r, "run", "family")] = -1
		if r.Runs != nil {
			byRun[rowKey(r, "run", "family")] = *r.Runs
		}
	}
	if want := map[string]int{
		"a1 compute USD": 1, "a2 compute USD": 1, "a3 compute USD": 1, "a4 compute USD": 1, "a5 compute USD": 1, "n1 compute USD": 1,
		"a4 ai USD": 1, "a1 ai EUR": 1, "a3 ai EUR": 1, "a5 ai EUR": 1,
	}; !maps.Equal(byRun, want) || len(got.Totals) != len(want) {
		t.Errorf("group=run runs=true %v (%d rows), want %v", byRun, len(got.Totals), want)
	}
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&runs=true"), &got); code != http.StatusOK {
		t.Fatalf("no group runs=true status %d", code)
	}
	if m, want := runsByGroup(got.Totals, ""), map[string]int{" USD": 6, " EUR": 3}; !maps.Equal(m, want) || len(got.Totals) != len(want) {
		t.Errorf("no group runs=true %v, want %v", m, want)
	}
}

// rowKey is "value… [other] currency" for group keys gs.
func rowKey(r CostSummaryRow, gs ...string) string {
	k := ""
	for _, g := range gs {
		k += r.Group[g] + " "
	}
	if r.Other {
		k += "other "
	}
	return k + r.Currency
}

// seriesByHour is series rows as "hour value… [other] currency" -> amount.
func seriesByHour(rows []CostSummaryRow, gs ...string) map[string]string {
	m := map[string]string{}
	for _, r := range rows {
		m[r.At.UTC().Format("15")+" "+rowKey(r, gs...)] = r.Amount
	}
	return m
}

func familyNames(fs []CostFamilyInfo) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Family)
	}
	return out
}

// runsByGroup is totals as "value currency" -> runs for group key g (-1: none).
func runsByGroup(rows []CostSummaryRow, g string) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Group[g]+" "+r.Currency] = -1
		if r.Runs != nil {
			m[r.Group[g]+" "+r.Currency] = *r.Runs
		}
	}
	return m
}

// a6 costs the most in USD but has no compute; a7, added later, is labelled
// app=(other), a value like any other.
func TestCostSummaryTopEdges(t *testing.T) {
	s, keys := topFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ('a6', 't1', '{}', 'running', '{"app":"a6"}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		VALUES ($1, 't1', 'a6', 'plugin', 'ai', 'USD', 500, 0, 0)`, t0)
	key := rowKey
	read := func(q string, gs ...string) (CostSummaryBody, map[string]string, map[string]int) {
		t.Helper()
		var got CostSummaryBody
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusOK {
			t.Fatalf("%s: status %d", q, code)
		}
		amounts, runs := map[string]string{}, map[string]int{}
		for _, r := range got.Totals {
			amounts[key(r, gs...)] = r.Amount
			if r.Runs != nil {
				runs[key(r, gs...)] = *r.Runs
			}
		}
		return got, amounts, runs
	}

	// rank=compute: a6 has no compute and ranks last, so a1 and a2 are kept and a6 is folded.
	got, split, _ := read("&group=label:app&group=family&top=2&rank=compute", "label:app", "family")
	want := map[string]string{
		"a1 compute USD": "100", "a2 compute USD": "80",
		"(other) compute other USD": "120", "(other) ai other USD": "600", "(none) compute USD": "1000",
		"a1 ai EUR": "1", "a3 ai EUR": "1", "(other) ai other EUR": "9",
	}
	if !maps.Equal(split, want) {
		t.Errorf("rank=compute totals %v, want %v", split, want)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 4, "EUR": 1}) {
		t.Errorf("rank=compute otherCount %v", got.OtherCount)
	}

	// 120 hours x 50 values x 2 families: 12,000 folded series rows pass the row limit.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels)
		SELECT 'w' || n, 't1', '{}', 'running', jsonb_build_object('app', 'w' || n) FROM generate_series(1, 50) n`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		SELECT $1::timestamptz - h * interval '1 hour', 't1', 'w' || n, f, f, 'JPY', 1, 0, 0
		FROM generate_series(1, 120) h, generate_series(1, 50) n, unnest(ARRAY['compute', 'ai']) f`, t0)
	limit := "/v1/costs?from=" + url.QueryEscape(t0.Add(-120*time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&group=label:app&group=family&interval=hour&top=50"
	if code := getJSON(t, s, keys["t1"], limit, &got); code != http.StatusRequestEntityTooLarge {
		t.Errorf("folded series over the row limit: status %d, want 413", code)
	}
	execSQL(t, s, ctx, `DELETE FROM cost_hourly WHERE currency = 'JPY'`)

	// A value named (other): a row of its own, never marked other, and not merged with the fold.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ('a7', 't1', '{}', 'running', '{"app":"(other)"}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		VALUES ($1, 't1', 'a7', 'compute', 'compute', 'USD', 700, 0, 0)`, t0)
	got, totals, _ := read("&group=label:app&top=10&interval=hour", "label:app")
	if totals["(other) USD"] != "700" || got.OtherCount != nil || slices.ContainsFunc(append(got.Totals, got.Series...), func(r CostSummaryRow) bool { return r.Other }) {
		t.Errorf("top=10: totals %v, otherCount %v", totals, got.OtherCount)
	}
	// USD compute ranks (other) 700 first: kept; a1..a6 folded. EUR has no compute: a1 kept by value.
	got, totals, runs := read("&group=label:app&top=1&rank=compute", "label:app")
	wantTotals := map[string]string{"(other) USD": "700", "(other) other USD": "900", "(none) USD": "1000", "a1 EUR": "1", "(other) other EUR": "10"}
	if !maps.Equal(totals, wantTotals) || len(got.Totals) != len(wantTotals) {
		t.Errorf("top=1 totals %v (%d rows), want %v", totals, len(got.Totals), wantTotals)
	}
	if wantRuns := map[string]int{"(other) USD": 1, "(other) other USD": 5, "(none) USD": 1, "a1 EUR": 0, "(other) other EUR": 0}; !maps.Equal(runs, wantRuns) {
		t.Errorf("top=1 runs %v, want %v", runs, wantRuns)
	}
	if !maps.Equal(got.OtherCount, map[string]int{"USD": 6, "EUR": 2}) {
		t.Errorf("top=1 otherCount %v", got.OtherCount)
	}
	// Per bucket too, the real (other) and the fold stay apart and only the fold is marked other.
	got, _, _ = read("&group=label:app&top=1&rank=compute&interval=hour", "label:app")
	if m, want := seriesByHour(got.Series, "label:app"), map[string]string{
		"10 (other) USD": "700", "10 (other) other USD": "750", "11 (other) other USD": "150",
		"10 (none) USD": "1000", "10 a1 EUR": "1", "10 (other) other EUR": "10",
	}; !maps.Equal(m, want) || len(got.Series) != len(want) {
		t.Errorf("top=1 series %v (%d rows), want %v", m, len(got.Series), want)
	}
	// Families never name the fold, and still name a family seen only under it.
	got, _, _ = read("&group=family&group=label:app&top=1", "family", "label:app")
	if fs := familyNames(got.Families); !slices.Equal(fs, []string{"ai", "compute"}) {
		t.Errorf("family first, folded: families %v", fs)
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES ('a8', 't1', '{}', 'running', '{"app":"a8"}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		VALUES ($1, 't1', 'a8', 'plugin', 'storage', 'USD', 1, 0, 0)`, t0)
	got, split, _ = read("&group=label:app&group=family&top=1&rank=compute", "label:app", "family")
	if split["(other) storage other USD"] != "1" || !slices.Contains(familyNames(got.Families), "storage") {
		t.Errorf("storage only under the fold: totals %v, families %v", split, familyNames(got.Families))
	}
	// Grouped by run, the fold names no Run; the kept ones (n1, and a1 by value in EUR) are named.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=run&top=1&rank=compute"), &got); code != http.StatusOK || len(got.Runs) != 2 || got.Runs[0].ID != "a1" || got.Runs[1].ID != "n1" {
		t.Errorf("run names under the fold: %d %+v", code, got.Runs)
	}
}

// 30 days per hour of 50 values in two families: unfolded, the breakdown
// passes the row limit; folded to 7 as the panel asks (the series per value
// with Show applied by family/nofamily, the family split as totals), it fits.
func TestCostSummaryTopUnderRowLimit(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	from := t0.Add(-30 * 24 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels)
		SELECT 'v' || n, 't1', '{}', 'running', jsonb_build_object('app', 'v' || n) FROM generate_series(1, 50) n`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, amount, allocated, unallocated)
		SELECT $1::timestamptz + h * interval '1 hour', 't1', 'v' || n, f, f, 'USD', CASE f WHEN 'ai' THEN 51 - n ELSE n END, 0, 0
		FROM generate_series(0, 719) h, generate_series(1, 50) n, unnest(ARRAY['compute', 'ai']) f`, from)
	path := "/v1/costs?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Format(time.RFC3339)) + "&group=label:app"
	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], path+"&group=family&interval=hour", &got); code != http.StatusRequestEntityTooLarge {
		t.Errorf("unfolded: status %d, want 413", code)
	}
	for _, show := range []struct{ q, rank, first string }{
		{"", "all", "v1"}, // every value costs 51 per hour: ties by value
		{"&family=compute", "compute", "v50"},
		{"&nofamily=compute", "external", "v1"},
	} {
		got = CostSummaryBody{}
		if code := getJSON(t, s, keys["t1"], path+show.q+"&interval=hour&top=7&rank="+show.rank, &got); code != http.StatusOK {
			t.Fatalf("%s series: status %d", show.rank, code)
		}
		// 7 values and (other), every hour.
		if len(got.Series) != 720*8 || len(got.Totals) != 8 || got.OtherCount["USD"] != 43 {
			t.Errorf("%s series: %d rows, %d totals, otherCount %v", show.rank, len(got.Series), len(got.Totals), got.OtherCount)
		}
		if _, ok := byGroup(got.Totals, "label:app")[show.first+" USD"]; !ok {
			t.Errorf("%s: %s not kept: %v", show.rank, show.first, byGroup(got.Totals, "label:app"))
		}
		split := CostSummaryBody{}
		if code := getJSON(t, s, keys["t1"], path+"&group=family&top=7&rank="+show.rank, &split); code != http.StatusOK || len(split.Totals) != 16 {
			t.Errorf("%s split: %d %d rows", show.rank, code, len(split.Totals))
		}
		// Both calls keep the same values.
		for _, r := range got.Totals {
			if !slices.ContainsFunc(split.Totals, func(x CostSummaryRow) bool { return x.Group["label:app"] == r.Group["label:app"] }) {
				t.Errorf("%s: %s in the series call, not in the split", show.rank, r.Group["label:app"])
			}
		}
	}
	// One family only, the folded series with its family split fits too.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], path+"&group=family&family=compute&interval=hour&top=7", &got); code != http.StatusOK || len(got.Series) != 720*8 {
		t.Errorf("one family folded: %d %d", code, len(got.Series))
	}
}
