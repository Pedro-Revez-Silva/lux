package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func pluginFixture(t *testing.T, handler http.HandlerFunc) (*Server, map[string]string, *httptest.Server) {
	t.Helper()
	s, keys := costFixture(t)
	p := httptest.NewServer(handler)
	t.Cleanup(p.Close)
	s.cfg.Costs.Plugins = []CostPluginConfig{{Name: "ledger", URL: p.URL, Timeout: 150 * time.Millisecond, MaxBatch: 1}}
	s.initCostPlugins()
	s.describeCostPlugins(context.Background())
	return s, keys, p
}

func pluginDescribeHandler(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/describe" {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": []int{1}, "name": "ledger", "maxBatch": 1, "settle": []string{"1s"}})
		return true
	}
	return false
}

func pluginLine() map[string]any {
	return map[string]any{"family": "ai", "item": "model", "amount": "1.250", "currency": "USD", "from": t0, "to": t0.Add(time.Minute)}
}

func TestPluginReportsAndIsolation(t *testing.T) {
	var mu sync.Mutex
	var seen []pluginRun
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		seen = append(seen, req.Runs...)
		mu.Unlock()
		out := []map[string]any{}
		for _, run := range req.Runs {
			out = append(out, map[string]any{"runId": run.RunID, "status": "ok", "lines": []any{pluginLine()}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": out})
	})
	execSQL(t, s, context.Background(), `UPDATE runs SET labels = '{"team":"payments"}', spec = '{"workload":{"adapter":"claude-code"}}' WHERE id = 'r1'`)
	execSQL(t, s, context.Background(), `INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id) VALUES ('t1','r1',1,'session')`)
	execSQL(t, s, context.Background(), `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1',now(),'tick'),('r2',now(),'tick')`)
	drain(t, s)
	mu.Lock()
	if len(seen) != 2 || seen[0].RunID != "r1" || seen[1].RunID != "r2" || seen[0].TenantName != "t1" || seen[0].Labels["team"] != "payments" || seen[0].Adapter != "claude-code" || len(seen[0].Sessions) != 1 {
		t.Errorf("requests: %+v", seen)
	}
	mu.Unlock()
	if _, c := getCost(t, s, keys["t1"], "r1"); totals(c.Totals) != "/USD=1.25(f0,e1.25) " {
		t.Errorf("r1: %+v", c)
	}
	if code, _ := getCost(t, s, keys["t1"], "r2"); code != http.StatusNotFound {
		t.Errorf("foreign cost: %d", code)
	}
	if _, c := getCost(t, s, keys["t2"], "r2"); totals(c.Totals) != "/USD=1.25(f0,e1.25) " {
		t.Errorf("r2: %+v", c)
	}
}

func TestPluginErrorsKeepLinesAndRetry(t *testing.T) {
	mode := "ok"
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		switch mode {
		case "down":
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case "slow":
			time.Sleep(250 * time.Millisecond)
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := []any{}
		if mode != "missing" {
			for _, run := range req.Runs {
				lines := []any{pluginLine()}
				if mode == "duplicate" {
					lines = append(lines, pluginLine())
				}
				out = append(out, map[string]any{"runId": run.RunID, "status": "ok", "lines": lines})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": out})
	})
	fire := func() {
		t.Helper()
		execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'r1' AND source = 'ledger'`)
		execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('r1','retry')`)
		drain(t, s)
	}
	fire()
	for _, state := range []string{"down", "slow", "duplicate", "missing"} {
		mode = state
		fire()
		_, c := getCost(t, s, keys["t1"], "r1")
		if c.Status != "incomplete" || totals(c.Totals) != "/USD=1.25(f0,e1.25) " {
			t.Errorf("%s: %+v", state, c)
		}
		var next *time.Time
		systemScan(t, s, `SELECT next_at FROM cost_sources WHERE run_id = 'r1' AND source = 'ledger'`, nil, &next)
		if next == nil {
			t.Errorf("%s: no retry", state)
		}
	}
	mode = "ok"
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'r1' AND source = 'ledger'`)
	fire()
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "complete" {
		t.Errorf("recovery: %+v", c)
	}
}

func TestPluginFinalAndResume(t *testing.T) {
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok", "final": true, "lines": []any{pluginLine()}}}})
	})
	finish(t, s, "t1", "r1", StateFailed)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "r1"); !c.Final || len(c.Lines) != 1 || !c.Lines[0].Final {
		t.Errorf("final: %+v", c)
	}
	resume(t, s, "r1")
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Final || c.Lines[0].Final {
		t.Errorf("resumed: %+v", c)
	}
}

func TestPluginPerRunErrorDoesNotRollbackSibling(t *testing.T) {
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := make([]any, 0, len(req.Runs))
		for _, run := range req.Runs {
			lines := []any{pluginLine()}
			if run.RunID == "r1" {
				lines = append(lines, pluginLine())
			}
			out = append(out, map[string]any{"runId": run.RunID, "status": "ok", "lines": lines})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": out})
	})
	execSQL(t, s, context.Background(), `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1',now(),'tick'),('r2',now(),'tick')`)
	if n, err := s.drainCosts(context.Background()); n != 2 || err != nil {
		t.Fatalf("drained %d: %v", n, err)
	}
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "incomplete" || len(c.Lines) != 0 {
		t.Errorf("invalid r1: %+v", c)
	}
	if _, c := getCost(t, s, keys["t2"], "r2"); c.Status != "complete" || totals(c.Totals) != "/USD=1.25(f0,e1.25) " {
		t.Errorf("valid r2: %+v", c)
	}
}

func TestPluginSettlementAndFinalSourceSkippedOnTick(t *testing.T) {
	requests := 0
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		requests++
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok", "lines": []any{pluginLine()}}}})
	})
	finish(t, s, "t1", "r1", StateFailed)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Final || c.Status != "complete" {
		t.Fatalf("initial answer: %+v", c)
	}
	time.Sleep(1100 * time.Millisecond)
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'r1' AND source = 'ledger'`)
	execSQL(t, s, context.Background(), `SELECT lux_cost_enqueue('r1','retry')`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "r1"); !c.Final || !c.Lines[0].Final {
		t.Fatalf("settled: %+v", c)
	}
	execSQL(t, s, context.Background(), `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1',now(),'tick')`)
	drain(t, s)
	if requests != 2 {
		t.Fatalf("final source queried on tick: %d requests", requests)
	}
}
