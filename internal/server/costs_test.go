package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// costFixture: tenants t1 (run r1) and t2 (run r2), a read key for each and
// an operator key.
func costFixture(t *testing.T) (s *Server, keys map[string]string) {
	t.Helper()
	s = testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running'), ('r2', 't2', '{}', 'running')`)
	keys = map[string]string{"t1": ids.Secret("luxk"), "t2": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'k', $1, ARRAY['read']), ('k2', 't2', 'k', $2, ARRAY['read']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["t1"]), ids.Hash(keys["t2"]), ids.Hash(keys["op"]))
	return s, keys
}

// report stores one source's answer for a Run, as a producer will.
func report(t *testing.T, s *Server, tenantID, runID, source string, lines ...costReport) {
	t.Helper()
	if err := reportError(s, tenantID, runID, source, lines...); err != nil {
		t.Fatal(err)
	}
}

func reportError(s *Server, tenantID, runID, source string, lines ...costReport) error {
	ctx := context.Background()
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return replaceCostLines(ctx, tx, tenantID, runID, source, lines)
	})
}

// getJSON GETs path with key and, on 200, decodes the body into out.
func getJSON(t *testing.T, s *Server, key, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func getCost(t *testing.T, s *Server, key, runID string) (int, RunCost) {
	t.Helper()
	var c RunCost
	code := getJSON(t, s, key, "/v1/runs/"+runID+"/cost", &c)
	return code, c
}

var t0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func line(family, item, amount, currency string, final bool) costReport {
	return costReport{Family: family, Item: item, Amount: amount, Currency: currency, From: t0, To: t0.Add(time.Hour), Final: final}
}

func totals(ts []CostTotal) string {
	out := ""
	for _, t := range ts {
		out += fmt.Sprintf("%s/%s=%s(f%s,e%s) ", t.Family, t.Currency, t.Amount, t.Final, t.Estimate)
	}
	return out
}

// Totals are per currency and per (family, currency), exact decimals, split
// into final and estimate; lines come back as stored.
func TestRunCostTotals(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "compute",
		line("compute", "m7i.2xlarge", "0.150000001", "USD", true),
		line("compute", "m7i.2xlarge:spot", "0.05", "USD", false))
	report(t, s, "t1", "r1", "model-gateway",
		line("ai", "large-model", "1.284310", "USD", false),
		line("ai", "small-model", "0.1", "USD", false),
		line("video", "", "2", "USD", true))

	code, c := getCost(t, s, keys["t1"], "r1")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got, want := totals(c.Totals), "/USD=3.584310001(f2.150000001,e1.43431) "; got != want {
		t.Errorf("totals:\n got %s\nwant %s", got, want)
	}
	if got, want := totals(c.ByFamily), "ai/USD=1.38431(f0,e1.38431) compute/USD=0.200000001(f0.150000001,e0.05) video/USD=2(f2,e0) "; got != want {
		t.Errorf("byFamily:\n got %s\nwant %s", got, want)
	}
	if len(c.Lines) != 5 || c.Lines[0].Family != "ai" || c.Lines[0].Item != "large-model" || c.Lines[0].Amount != "1.28431" ||
		!c.Lines[0].From.Equal(t0) || c.Lines[0].Source != "model-gateway" || c.Lines[0].RunID != "r1" {
		t.Errorf("lines: %+v", c.Lines)
	}
	if c.Status != "complete" || c.Final || c.Basis != "list" {
		t.Errorf("status %q final %v basis %q", c.Status, c.Final, c.Basis)
	}
}

func TestRunCostFamilyMetadata(t *testing.T) {
	s, keys := costFixture(t)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "first"}, {Name: "second"}}
	s.initCostPlugins()
	type metadata struct {
		DisplayName string `json:"displayName"`
		Color       string `json:"color"`
	}
	setFamilies := func(index int, values map[string]metadata) {
		t.Helper()
		p := s.plugins[index]
		p.mu.Lock()
		p.desc.Families = make(map[string]struct {
			DisplayName string `json:"displayName"`
			Color       string `json:"color"`
		})
		for family, value := range values {
			p.desc.Families[family] = value
		}
		p.usable = true
		p.mu.Unlock()
	}
	setFamilies(0, map[string]metadata{
		"ai": {"AI models", "violet"}, "compute": {"Not compute", "red"},
	})
	setFamilies(1, map[string]metadata{
		"ai": {"Other AI", "amber"}, "video": {"Video", "amber"},
	})
	report(t, s, "t1", "r1", "compute", line("compute", "host", "1", "USD", false))
	report(t, s, "t1", "r1", "first", line("ai", "model", "2", "USD", false), line("unknown", "other", "3", "USD", false))
	report(t, s, "t1", "r1", "second", line("video", "clip", "4", "USD", false))
	check := func() {
		t.Helper()
		code, c := getCost(t, s, keys["t1"], "r1")
		if code != http.StatusOK || len(c.ByFamily) != 4 {
			t.Fatalf("cost response: %d %+v", code, c)
		}
		for _, total := range c.ByFamily {
			switch total.Family {
			case "ai":
				if total.DisplayName != "AI models" || total.Color != "violet" {
					t.Errorf("ai: %+v", total)
				}
			case "compute":
				if total.DisplayName != "Compute" || total.Color != "" {
					t.Errorf("compute: %+v", total)
				}
			case "unknown":
				if total.DisplayName != "" || total.Color != "" {
					t.Errorf("unknown: %+v", total)
				}
			case "video":
				if total.DisplayName != "Video" || total.Color != "amber" {
					t.Errorf("video: %+v", total)
				}
			default:
				t.Errorf("unexpected family: %+v", total)
			}
		}
		if len(c.Totals) != 1 || c.Totals[0].DisplayName != "" || c.Totals[0].Color != "" {
			t.Errorf("currency totals have family metadata: %+v", c.Totals)
		}
	}
	check()
	check()
	if n := strings.Count(logs.String(), "cost family metadata conflict"); n != 1 {
		t.Errorf("conflict logged %d times, want once: %s", n, logs.String())
	}
	// A temporarily unusable first plugin lets the next configured one supply metadata.
	s.plugins[0].mu.Lock()
	s.plugins[0].usable = false
	s.plugins[0].mu.Unlock()
	_, c := getCost(t, s, keys["t1"], "r1")
	if c.ByFamily[0].DisplayName != "Other AI" || c.ByFamily[0].Color != "amber" {
		t.Errorf("fallback metadata: %+v", c.ByFamily[0])
	}
}

func TestCostFamilyMetadataPrecedenceAndLogging(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, nil)), cfg: Config{Costs: CostsConfig{Plugins: []CostPluginConfig{{Name: "metadata-first"}, {Name: "metadata-second"}}}}}
	s.initCostPlugins()
	for i, d := range []struct{ name, color string }{{"AI", "violet"}, {"Different", "amber"}} {
		p := s.plugins[i]
		p.desc.Families = map[string]struct {
			DisplayName string `json:"displayName"`
			Color       string `json:"color"`
		}{"ai": {d.name, d.color}}
		p.usable = true
	}
	for range 2 {
		totals := []CostTotal{{Family: "ai"}, {Family: "compute"}, {Family: "other"}}
		s.decorateCostFamilies(totals)
		if totals[0].DisplayName != "AI" || totals[0].Color != "violet" || totals[1].DisplayName != "Compute" || totals[2].DisplayName != "" {
			t.Errorf("family metadata: %+v", totals)
		}
	}
	if n := strings.Count(logs.String(), "cost family metadata conflict"); n != 1 {
		t.Errorf("conflict logged %d times, want once: %s", n, logs.String())
	}
	s.plugins[0].usable = false
	totals := []CostTotal{{Family: "ai"}}
	s.decorateCostFamilies(totals)
	if totals[0].DisplayName != "Different" || totals[0].Color != "amber" {
		t.Errorf("unusable first plugin: %+v", totals)
	}
}

// Two currencies are two totals, per Run and per family: never one sum.
func TestRunCostCurrenciesNeverSummed(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "a", line("ai", "x", "1.5", "USD", false), line("ai", "y", "2.25", "EUR", false))
	report(t, s, "t1", "r1", "b", line("ai", "z", "-0.5", "EUR", false))
	_, c := getCost(t, s, keys["t1"], "r1")
	if got, want := totals(c.Totals), "/EUR=1.75(f0,e1.75) /USD=1.5(f0,e1.5) "; got != want {
		t.Errorf("totals:\n got %s\nwant %s", got, want)
	}
	if got, want := totals(c.ByFamily), "ai/EUR=1.75(f0,e1.75) ai/USD=1.5(f0,e1.5) "; got != want {
		t.Errorf("byFamily:\n got %s\nwant %s", got, want)
	}
}

// A source's new answer replaces its earlier lines for the Run: a repeated
// item is not added twice, a dropped item goes, and other sources' lines
// stay. An answer naming one item twice is refused and changes nothing.
func TestRunCostReReportReplaces(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "gw", line("ai", "m1", "1", "USD", false), line("ai", "m2", "4", "USD", false))
	report(t, s, "t1", "r1", "other", line("video", "v", "10", "USD", false))
	report(t, s, "t1", "r1", "gw", line("ai", "m1", "1.5", "USD", true))

	_, c := getCost(t, s, keys["t1"], "r1")
	if got, want := totals(c.Totals), "/USD=11.5(f1.5,e10) "; got != want {
		t.Errorf("totals after re-report:\n got %s\nwant %s", got, want)
	}
	if len(c.Lines) != 2 {
		t.Errorf("lines after re-report: %+v", c.Lines)
	}

	if err := reportError(s, "t1", "r1", "gw", line("ai", "m1", "1", "USD", false), line("ai", "m1", "2", "USD", false)); err == nil {
		t.Fatal("an answer with one item twice was accepted")
	}
	if _, c := getCost(t, s, keys["t1"], "r1"); totals(c.Totals) != "/USD=11.5(f1.5,e10) " {
		t.Errorf("a refused answer changed the totals: %s", totals(c.Totals))
	}
}

// An amount that is not a decimal numeric(24, 9) holds exactly is refused
// and changes nothing. An empty answer is valid: that source's lines go,
// other sources' stay.
func TestRunCostInvalidAmountAndEmptyReport(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "gw", line("ai", "m1", "1", "USD", false))
	report(t, s, "t1", "r1", "other", line("video", "v", "10", "USD", false))

	for _, amount := range []string{"1e3", "abc", "1.0000000001", ""} {
		if err := reportError(s, "t1", "r1", "gw", line("ai", "m1", amount, "USD", false)); err == nil {
			t.Errorf("amount %q was accepted", amount)
		}
		if _, c := getCost(t, s, keys["t1"], "r1"); totals(c.Totals) != "/USD=11(f0,e11) " {
			t.Errorf("refused amount %q changed the totals: %s", amount, totals(c.Totals))
		}
	}

	report(t, s, "t1", "r1", "gw")
	_, c := getCost(t, s, keys["t1"], "r1")
	if got, want := totals(c.Totals), "/USD=10(f0,e10) "; got != want {
		t.Errorf("totals after an empty answer:\n got %s\nwant %s", got, want)
	}
	if len(c.Lines) != 1 || c.Lines[0].Source != "other" || c.Lines[0].Item != "v" {
		t.Errorf("lines after an empty answer: %+v", c.Lines)
	}
}

// An insert the database refuses after the delete is queued (here a tenant
// that does not exist, a foreign key) rolls the whole answer back: the
// source's earlier lines and every other source's stay.
func TestRunCostDatabaseFailureKeepsEarlierLines(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "gw", line("ai", "m1", "1", "USD", false))
	report(t, s, "t1", "r1", "other", line("video", "v", "10", "USD", false))

	if err := reportError(s, "missing-tenant", "r1", "gw", line("ai", "m2", "5", "USD", false)); err == nil {
		t.Fatal("a line for a tenant that does not exist was accepted")
	}
	_, c := getCost(t, s, keys["t1"], "r1")
	if got, want := totals(c.Totals), "/USD=11(f0,e11) "; got != want {
		t.Errorf("totals after a failed answer:\n got %s\nwant %s", got, want)
	}
	if len(c.Lines) != 2 || c.Lines[0].Source != "gw" || c.Lines[0].Item != "m1" || c.Lines[0].Amount != "1" ||
		c.Lines[1].Source != "other" || c.Lines[1].Item != "v" || c.Lines[1].Amount != "10" {
		t.Errorf("lines after a failed answer: %+v", c.Lines)
	}
}

// A tenant sees its own Runs' costs, and another tenant's Run is not found,
// as on every Run endpoint. An operator reaches either.
func TestRunCostTenantIsolation(t *testing.T) {
	s, keys := costFixture(t)
	report(t, s, "t1", "r1", "gw", line("ai", "m", "1", "USD", false))
	report(t, s, "t2", "r2", "gw", line("ai", "m", "7", "USD", false))

	if code, c := getCost(t, s, keys["t1"], "r1"); code != http.StatusOK || totals(c.Totals) != "/USD=1(f0,e1) " {
		t.Errorf("t1 own run: %d %s", code, totals(c.Totals))
	}
	if code, _ := getCost(t, s, keys["t1"], "r2"); code != http.StatusNotFound {
		t.Errorf("t1 reading t2's run: %d, want 404", code)
	}
	if code, _ := getCost(t, s, keys["t2"], "r1"); code != http.StatusNotFound {
		t.Errorf("t2 reading t1's run: %d, want 404", code)
	}
	if code, c := getCost(t, s, keys["op"], "r2"); code != http.StatusOK || totals(c.Totals) != "/USD=7(f0,e7) " {
		t.Errorf("operator: %d %s", code, totals(c.Totals))
	}
	if code, _ := getCost(t, s, keys["t1"], "nope"); code != http.StatusNotFound {
		t.Errorf("unknown run: %d", code)
	}

	// Below the API too: t1's scope reads none of t2's lines, and cannot
	// write one for t2.
	ctx := context.Background()
	var n int
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_lines`).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Errorf("t1 sees %d cost lines (%v), want 1", n, err)
	}
	err = s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return replaceCostLines(ctx, tx, "t2", "r2", "evil", []costReport{line("ai", "m", "1", "USD", false)})
	})
	if err == nil {
		t.Error("t1 wrote a cost line for t2")
	}

	// cost_sources has its own policy: the same two checks.
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES
		('r1', 't1', 'gw', 'ok'), ('r2', 't2', 'gw', 'ok'), ('r2', 't2', 'compute', 'ok')`)
	err = s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM cost_sources`).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Errorf("t1 sees %d cost sources (%v), want 1", n, err)
	}
	err = s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES ('r2', 't2', 'evil', 'final')`)
		return err
	})
	if err == nil {
		t.Error("t1 wrote a cost source for t2")
	}
}

// Status follows the sources: pending before any report, complete while every source
// has answered, incomplete while any source is, final once all are.
func TestRunCostStatus(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "pending" || len(c.Totals) != 0 || c.Lines == nil {
		t.Errorf("no lines: %+v", c)
	}
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "gw"}}
	answered, next := t0.Add(time.Hour), t0.Add(2*time.Hour)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, answered_at, next_at) VALUES
		('r1', 't1', 'compute', 'final', $1, NULL), ('r1', 't1', 'gw', 'ok', $1, $2)`, answered, next)
	_, c := getCost(t, s, keys["t1"], "r1")
	if c.Status != "complete" || c.Final {
		t.Errorf("all answered: status %q final %v", c.Status, c.Final)
	}
	sources := func(ss []CostSource) string {
		out := ""
		for _, src := range ss {
			out += fmt.Sprintf("%s/%s/answered=%v/next=%v ", src.Source, src.Status, fmtTime(src.AnsweredAt), fmtTime(src.NextAt))
		}
		return out
	}
	if got, want := sources(c.Sources), "compute/final/answered=2026-09-26T11:00:00Z/next=- gw/ok/answered=2026-09-26T11:00:00Z/next=2026-09-26T12:00:00Z "; got != want {
		t.Errorf("sources:\n got %s\nwant %s", got, want)
	}

	execSQL(t, s, ctx, `UPDATE cost_sources SET status = 'incomplete', answered_at = NULL WHERE source = 'gw'`)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "incomplete" || c.Final || len(c.Sources) != 2 || c.Sources[0].AnsweredAt == nil || c.Sources[1].AnsweredAt != nil {
		t.Errorf("one incomplete: %+v", c)
	}
	execSQL(t, s, ctx, `UPDATE cost_sources SET status = 'final' WHERE source = 'gw'`)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "final" || !c.Final {
		t.Errorf("all final: %+v", c)
	}
}

func TestRemovedPluginHistoryDoesNotBlockCurrentFinality(t *testing.T) {
	s, keys := costFixture(t)
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "current"}}
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, next_at) VALUES
		('r1', 't1', 'compute', 'final', NULL),
		('r1', 't1', 'old', 'incomplete', now() - interval '1 hour'),
		('r1', 't1', 'current', 'final', NULL)`)
	report(t, s, "t1", "r1", "old", line("ai", "historical", "2", "USD", false))
	code, c := getCost(t, s, keys["t1"], "r1")
	if code != http.StatusOK || !c.Final || c.Status != "final" || len(c.Sources) != 3 || c.Sources[2].Status != "incomplete" ||
		len(c.Lines) != 1 || c.Lines[0].Source != "old" || totals(c.Totals) != "/USD=2(f0,e2) " {
		t.Fatalf("removed plugin history: %d %+v", code, c)
	}
	if code, _ := getCost(t, s, keys["t2"], "r1"); code != http.StatusNotFound {
		t.Errorf("foreign tenant read historical costs: %d", code)
	}
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "current"}, {Name: "new"}}
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Final || c.Status != "incomplete" {
		t.Errorf("missing configured source did not block finality: %+v", c)
	}
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func listCosts(t *testing.T, s *Server, key, query string) map[string]*RunCostBrief {
	t.Helper()
	var body listRunsBody
	if code := getJSON(t, s, key, "/v1/runs"+query, &body); code != http.StatusOK {
		t.Fatalf("list runs: %d", code)
	}
	out := map[string]*RunCostBrief{}
	for _, r := range body.Runs {
		out[r.ID] = r.Cost
	}
	return out
}

// The Runs list carries each Run's totals per currency and its status,
// with the same numbers and visibility as GET /v1/runs/{id}/cost.
func TestListRunsCost(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r3', 't1', '{}', 'running')`)
	report(t, s, "t1", "r1", "compute", line("compute", "m7i", "0.150000001", "USD", true), line("compute", "spot", "0.05", "USD", false))
	report(t, s, "t1", "r1", "gw", line("ai", "m", "2.5", "EUR", false))
	report(t, s, "t2", "r2", "compute", line("compute", "m7i", "7", "USD", false))

	got := listCosts(t, s, keys["t1"], "")
	if len(got) != 2 || got["r2"] != nil {
		t.Fatalf("t1 lists %v, want r1 and r3 only", got)
	}
	if c := got["r1"]; c == nil || c.Status != "complete" || totals(c.Totals) != "/EUR=2.5(f0,e2.5) /USD=0.200000001(f0.150000001,e0.05) " {
		t.Errorf("r1: %+v", c)
	}
	if c := got["r3"]; c == nil || c.Status != "pending" || c.Totals == nil || len(c.Totals) != 0 {
		t.Errorf("r3 with no lines: %+v", c)
	}
	// The list's status and totals are the per-Run endpoint's.
	_, full := getCost(t, s, keys["t1"], "r1")
	if full.Status != got["r1"].Status || totals(full.Totals) != totals(got["r1"].Totals) {
		t.Errorf("list %+v differs from runCost %s %s", got["r1"], full.Status, totals(full.Totals))
	}

	if c := listCosts(t, s, keys["t2"], ""); len(c) != 1 || totals(c["r2"].Totals) != "/USD=7(f0,e7) " {
		t.Errorf("t2: %v", c)
	}
	all := listCosts(t, s, keys["op"], "")
	if len(all) != 3 || totals(all["r2"].Totals) != "/USD=7(f0,e7) " || totals(all["r1"].Totals) != totals(got["r1"].Totals) {
		t.Errorf("operator: %v", all)
	}
	if c := listCosts(t, s, keys["op"], "?tenant=t2"); len(c) != 1 || c["r2"] == nil {
		t.Errorf("operator narrowed to t2: %v", c)
	}

	// A configured plugin that has not answered makes it incomplete; a
	// source row that is incomplete does too.
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "gw"}}
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES ('r1', 't1', 'compute', 'final')`)
	if c := listCosts(t, s, keys["t1"], ""); c["r1"].Status != "incomplete" || c["r3"].Status != "incomplete" {
		t.Errorf("unanswered plugin: r1 %+v r3 %+v", c["r1"], c["r3"])
	}
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status) VALUES ('r1', 't1', 'gw', 'final')`)
	if c := listCosts(t, s, keys["t1"], ""); c["r1"].Status != "final" {
		t.Errorf("all final: %+v", c["r1"])
	}
}
