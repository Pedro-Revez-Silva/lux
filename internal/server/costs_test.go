package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
func report(t *testing.T, s *Server, tenantID, runID, source string, lines ...CostLine) {
	t.Helper()
	ctx := context.Background()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return replaceCostLines(ctx, tx, tenantID, runID, source, lines)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func getCost(t *testing.T, s *Server, key, runID string) (int, RunCost) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/cost", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var c RunCost
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, c
}

var t0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func line(family, item, amount, currency string, final bool) CostLine {
	return CostLine{Family: family, Item: item, Amount: amount, Currency: currency, From: t0, To: t0.Add(time.Hour), Final: final}
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

	ctx := context.Background()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return replaceCostLines(ctx, tx, "t1", "r1", "gw", []CostLine{line("ai", "m1", "1", "USD", false), line("ai", "m1", "2", "USD", false)})
	})
	if err == nil {
		t.Fatal("an answer with one item twice was accepted")
	}
	if _, c := getCost(t, s, keys["t1"], "r1"); totals(c.Totals) != "/USD=11.5(f1.5,e10) " {
		t.Errorf("a refused answer changed the totals: %s", totals(c.Totals))
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
		return replaceCostLines(ctx, tx, "t2", "r2", "evil", []CostLine{line("ai", "m", "1", "USD", false)})
	})
	if err == nil {
		t.Error("t1 wrote a cost line for t2")
	}
}

// Status follows the sources: none reported, incomplete while any source
// is, final once all are.
func TestRunCostStatus(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "none" || len(c.Totals) != 0 || c.Lines == nil {
		t.Errorf("no lines: %+v", c)
	}
	execSQL(t, s, ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, answered_at) VALUES
		('r1', 't1', 'compute', 'final', now()), ('r1', 't1', 'gw', 'incomplete', NULL)`)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "incomplete" || c.Final || len(c.Sources) != 2 || c.Sources[0].AnsweredAt == nil {
		t.Errorf("one incomplete: %+v", c)
	}
	execSQL(t, s, ctx, `UPDATE cost_sources SET status = 'final' WHERE source = 'gw'`)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "final" || !c.Final {
		t.Errorf("all final: %+v", c)
	}
}
