package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// costLabelFixture: costReadFixture plus t1 Runs r3 (app=dude) and r4 (no
// labels), and labels on r1 that need escaping.
func costLabelFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, keys := costReadFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET labels = $1 WHERE id = 'r1'`,
		map[string]string{"team": "alpha", "app": "jervasion", "note": "a=b,c ünï"})
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, labels) VALUES
		('r3', 't1', '{}', 'running', '{"app": "dude", "team": "beta"}'), ('r4', 't1', '{}', 'running', '{}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount, allocated, unallocated) VALUES
		($1, 't1', 'r3', 'compute', 'compute', 'USD', 'h1', 'blue', 10, 0, 0),
		($1, 't1', 'r4', 'compute', 'compute', 'USD', 'h1', 'blue', 100, 0, 0)`, t0)
	return s, keys
}

// runTotals is a summary grouped by run as "run currency" -> amount.
func runTotals(rows []CostSummaryRow) map[string]string {
	m := map[string]string{}
	for _, r := range rows {
		m[r.Group["run"]+" "+r.Currency] = r.Amount
	}
	return m
}

func TestCostSummaryLabelFilters(t *testing.T) {
	s, keys := costLabelFixture(t)
	for _, tc := range []struct {
		q    string
		want map[string]string
	}{
		{"&label=app=jervasion", map[string]string{"r1 USD": "2", "r1 EUR": "2.5"}},
		// The same key repeated: either value.
		{"&label=app=jervasion&label=app=dude", map[string]string{"r1 USD": "2", "r1 EUR": "2.5", "r3 USD": "10"}},
		// Different keys: both.
		{"&label=app=dude&label=team=alpha", map[string]string{}},
		{"&label=app=dude&label=team=beta", map[string]string{"r3 USD": "10"}},
		{"&nolabel=app", map[string]string{"r4 USD": "100"}},
		{"&nolabel=app&nolabel=team", map[string]string{"r4 USD": "100"}},
		{"&label=team=beta&nolabel=note", map[string]string{"r3 USD": "10"}},
		// nolabel removes a Run the label matched.
		{"&label=team=alpha&nolabel=note", map[string]string{}},
		{"&label=app=jervasion&label=app=dude&nolabel=note", map[string]string{"r3 USD": "10"}},
		// A value with = , and non-ASCII is a value, split at the first =.
		{"&label=" + url.QueryEscape("note=a=b,c ünï"), map[string]string{"r1 USD": "2", "r1 EUR": "2.5"}},
		{"&label=" + url.QueryEscape("note=a=b"), map[string]string{}},
		{"&label=app=", map[string]string{}},
	} {
		var got CostSummaryBody
		if code := getJSON(t, s, keys["t1"], costPath("&group=run&interval=hour"+tc.q), &got); code != http.StatusOK {
			t.Errorf("%s: status %d", tc.q, code)
			continue
		}
		if m := runTotals(got.Totals); !maps.Equal(m, tc.want) {
			t.Errorf("%s: totals %v, want %v", tc.q, m, tc.want)
		}
		for _, r := range got.Series {
			if _, ok := tc.want[r.Group["run"]+" "+r.Currency]; !ok {
				t.Errorf("%s: series row %+v outside the filter", tc.q, r)
			}
		}
	}
	// Unallocated cost has no Run to filter by: a filtered summary leaves it out.
	var got CostSummaryBody
	if code := getJSON(t, s, keys["op"], costPath("&label=app=dude"), &got); code != http.StatusOK || got.Unallocated != nil ||
		len(got.Totals) != 1 || got.Totals[0].Amount != "10" {
		t.Errorf("operator filtered: %d %+v", code, got)
	}
	for _, q := range []string{"&label=app", "&label=" + url.QueryEscape("bad key=x"), "&label=-x=1", "&nolabel=" + url.QueryEscape("a b"), "&nolabel="} {
		if code := getJSON(t, s, keys["t1"], costPath(q), &got); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	// At most 64 filters, label and nolabel together, on both endpoints.
	filters := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i%2 == 0 {
				fmt.Fprintf(&b, "&label=app=v%d", i)
			} else {
				fmt.Fprintf(&b, "&nolabel=k%d", i)
			}
		}
		return b.String()
	}
	for _, path := range []string{costPath(""), "/v1/costs/labels" + costPath("")[len("/v1/costs"):]} {
		if code := getJSON(t, s, keys["t1"], path+filters(64), &got); code != http.StatusOK {
			t.Errorf("64 filters %s: status %d", path, code)
		}
		if code := getJSON(t, s, keys["t1"], path+filters(65), &got); code != http.StatusBadRequest {
			t.Errorf("65 filters %s: status %d, want 400", path, code)
		}
	}
	// Grouped by a label: Runs without it are (none), never dropped.
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=label:app&family=compute"), &got); code != http.StatusOK {
		t.Fatalf("group label status %d", code)
	}
	byApp := map[string]string{}
	for _, r := range got.Totals {
		byApp[r.Group["label:app"]] = r.Amount
	}
	if !maps.Equal(byApp, map[string]string{"jervasion": "2", "dude": "10", "(none)": "100"}) {
		t.Errorf("by app: %v", byApp)
	}
}

func TestCostLabels(t *testing.T) {
	s, keys := costLabelFixture(t)
	type labelsBody struct {
		Keys []CostLabelKey `json:"keys"`
	}
	path := func(q string) string {
		return "/v1/costs/labels" + costPath(q)[len("/v1/costs"):]
	}
	var got labelsBody
	if code := getJSON(t, s, keys["t1"], path(""), &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want := []CostLabelKey{{"app", 2}, {"team", 2}, {"note", 1}}
	if !slices.Equal(got.Keys, want) {
		t.Errorf("t1 keys: %+v", got.Keys)
	}
	got = labelsBody{}
	if code := getJSON(t, s, keys["t1"], path("&label=app=dude"), &got); code != http.StatusOK || !slices.Equal(got.Keys, []CostLabelKey{{"app", 1}, {"team", 1}}) {
		t.Errorf("filtered keys: %d %+v", code, got.Keys)
	}
	// t2's Run r2 carries a label t1 must not see.
	execSQL(t, s, context.Background(), `UPDATE runs SET labels = '{"secret": "x"}' WHERE id = 'r2'`)
	got = labelsBody{}
	if code := getJSON(t, s, keys["t1"], path(""), &got); code != http.StatusOK || slices.ContainsFunc(got.Keys, func(k CostLabelKey) bool { return k.Key == "secret" }) {
		t.Errorf("t1 sees t2's labels: %+v", got.Keys)
	}
	// A tenant key's ?tenant= is ignored: still its own.
	got = labelsBody{}
	if code := getJSON(t, s, keys["t1"], path("&tenant=t2"), &got); code != http.StatusOK || !slices.Equal(got.Keys, want) {
		t.Errorf("t1 asking for t2: %d %+v", code, got.Keys)
	}
	got = labelsBody{}
	if code := getJSON(t, s, keys["op"], path(""), &got); code != http.StatusOK || !slices.Contains(got.Keys, CostLabelKey{"secret", 1}) {
		t.Errorf("operator keys: %d %+v", code, got.Keys)
	}
	got = labelsBody{}
	if code := getJSON(t, s, keys["op"], path("&tenant=t1"), &got); code != http.StatusOK || !slices.Equal(got.Keys, want) {
		t.Errorf("narrowed operator keys: %d %+v", code, got.Keys)
	}
	if code := getJSON(t, s, keys["t1"], path("&nolabel=a%20b"), &got); code != http.StatusBadRequest {
		t.Errorf("bad nolabel: %d", code)
	}
}

func TestCostSummaryByKey(t *testing.T) {
	s, keys := costLabelFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes, revoked_at) VALUES
		('k1old', 't1', 'old-bot', 'h-old', ARRAY['run'], now())`)
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'k1' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'ko' WHERE id = 'r3'`)
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'k1old' WHERE id = 'r4'`)
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_email = 'ada@example.com' WHERE id = 'r2'`)

	var got CostSummaryBody
	if code := getJSON(t, s, keys["t1"], costPath("&group=key&family=compute"), &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	byKey := map[string]string{}
	for _, r := range got.Totals {
		byKey[r.Group["key"]] = r.Amount
	}
	if !maps.Equal(byKey, map[string]string{"k1": "2", "ko": "10", "k1old": "100"}) {
		t.Errorf("t1 by key: %v", byKey)
	}
	// The tenant names its own keys; the operator key stays unnamed.
	wantT1 := []CostKeyInfo{{ID: "k1", Name: "k"}, {ID: "k1old", Name: "old-bot", Revoked: true}, {ID: "ko", Operator: true}}
	if !slices.Equal(got.Keys, wantT1) {
		t.Errorf("t1 keys: %+v", got.Keys)
	}
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t2"], costPath("&group=key"), &got); code != http.StatusOK ||
		!slices.Equal(got.Keys, []CostKeyInfo{{ID: "email:ada@example.com", Email: "ada@example.com"}}) {
		t.Errorf("t2 keys: %d %+v", code, got.Keys)
	}
	// A key id of another tenant on a tenant's own Run is not named either.
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'k2' WHERE id = 'r4'`)
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=key&nolabel=app"), &got); code != http.StatusOK ||
		!slices.Equal(got.Keys, []CostKeyInfo{{ID: "k2"}}) {
		t.Errorf("foreign key id: %d %+v", code, got.Keys)
	}
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = 'k1old' WHERE id = 'r4'`)
	for _, q := range []string{"&group=key", "&group=key&tenant=t1"} {
		got = CostSummaryBody{}
		if code := getJSON(t, s, keys["op"], costPath(q), &got); code != http.StatusOK {
			t.Fatalf("operator %s: %d", q, code)
		}
		if !slices.Contains(got.Keys, CostKeyInfo{ID: "ko", Name: "o", Operator: true}) ||
			!slices.Contains(got.Keys, CostKeyInfo{ID: "k1", Name: "k"}) {
			t.Errorf("operator %s keys: %+v", q, got.Keys)
		}
		if q == "&group=key&tenant=t1" && slices.ContainsFunc(got.Keys, func(k CostKeyInfo) bool { return k.Email != "" }) {
			t.Errorf("narrowed operator sees t2's submitter: %+v", got.Keys)
		}
	}
	// Runs from before tracking group as (none) and are not in keys.
	execSQL(t, s, ctx, `UPDATE runs SET submitted_by_key = NULL WHERE id = 'r3'`)
	got = CostSummaryBody{}
	if code := getJSON(t, s, keys["t1"], costPath("&group=key&group=family"), &got); code != http.StatusOK ||
		!slices.ContainsFunc(got.Totals, func(r CostSummaryRow) bool { return r.Group["key"] == "(none)" && r.Amount == "10" }) ||
		slices.ContainsFunc(got.Keys, func(k CostKeyInfo) bool { return k.ID == "(none)" }) {
		t.Errorf("before tracking: %d %+v", code, got)
	}
}
