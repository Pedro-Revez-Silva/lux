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
	var modeMu sync.Mutex
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		modeMu.Lock()
		current := mode
		modeMu.Unlock()
		switch current {
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
		if current != "missing" {
			for _, run := range req.Runs {
				lines := []any{pluginLine()}
				if current == "duplicate" {
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
		modeMu.Lock()
		mode = state
		modeMu.Unlock()
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
	modeMu.Lock()
	mode = "ok"
	modeMu.Unlock()
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

func TestPluginRejectsOversizedHourlyReports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []any
	}{
		{
			name: "line over 90 days",
			lines: []any{map[string]any{
				"family": "ai", "item": "model", "amount": "1.250", "currency": "USD",
				"from": t0.Add(-91 * 24 * time.Hour), "to": t0,
			}},
		},
		{
			name: "combined bucket budget",
			lines: []any{
				map[string]any{"family": "ai", "item": "model-a", "amount": "1.250", "currency": "USD", "from": t0, "to": t0.Add(45 * 24 * time.Hour)},
				map[string]any{"family": "ai", "item": "model-b", "amount": "1.250", "currency": "USD", "from": t0, "to": t0.Add(45 * 24 * time.Hour)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if pluginDescribeHandler(w, r) {
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{
					map[string]any{"runId": "r1", "status": "ok", "final": true, "lines": tc.lines},
				}})
			})
			finish(t, s, "t1", "r1", StateFailed)
			drain(t, s)
			code, c := getCost(t, s, keys["t1"], "r1")
			if code != http.StatusOK || c.Final || c.Status != "incomplete" || len(c.Lines) != 0 {
				t.Fatalf("rejected report: %d %+v", code, c)
			}
			var status string
			var answered *time.Time
			systemScan(t, s, `SELECT status, answered_at FROM cost_sources WHERE run_id = 'r1' AND source = 'ledger'`, nil, &status, &answered)
			if status != "incomplete" || answered != nil {
				t.Errorf("rejected final source: status %q, answered %v", status, answered)
			}
			var lines, hours int
			systemScan(t, s, `SELECT count(*) FROM cost_lines WHERE run_id = 'r1' AND source = 'ledger'`, nil, &lines)
			systemScan(t, s, `SELECT count(*) FROM cost_hourly WHERE run_id = 'r1' AND source = 'ledger'`, nil, &hours)
			if lines != 0 || hours != 0 {
				t.Errorf("rejected report persisted %d lines and %d hours", lines, hours)
			}
		})
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

func TestPluginRejectsRedirectWithoutLeakingTokenOrRuns(t *testing.T) {
	var received int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
	}))
	defer foreign.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", foreign.URL+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer local.Close()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		s := &Server{cfg: Config{Costs: CostsConfig{Plugins: []CostPluginConfig{{Name: "ledger", URL: local.URL, TokenEnv: "LUX_TEST_COST_TOKEN"}}}}}
		t.Setenv("LUX_TEST_COST_TOKEN", "secret")
		s.initCostPlugins()
		var result map[string]any
		if err := s.plugins[0].request(context.Background(), method, "/v1/costs", map[string]any{"tenantId": "private"}, &result); err == nil {
			t.Fatalf("%s redirect accepted", method)
		}
	}
	if received != 0 {
		t.Fatalf("redirect target received %d requests", received)
	}
}

func TestPluginSlowChunkDoesNotBlockComputeOrOtherPlugin(t *testing.T) {
	blocked := make(chan struct{})
	started := make(chan struct{})
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		close(started)
		<-blocked
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{}})
	})
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/describe" {
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol": []int{1}, "name": "fast"})
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok", "lines": []any{pluginLine()}}}})
	}))
	defer fast.Close()
	s.cfg.Costs.Plugins = append(s.cfg.Costs.Plugins, CostPluginConfig{Name: "fast", URL: fast.URL, Timeout: time.Second, MaxBatch: 1})
	s.plugins = append(s.plugins, &costPlugin{cfg: s.cfg.Costs.Plugins[1], client: fast.Client(), usable: true})
	execSQL(t, s, context.Background(), `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1', now(), 'tick')`)
	done := make(chan error, 1)
	go func() { _, err := s.drainCosts(context.Background()); done <- err }()
	<-started
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, c := getCost(t, s, keys["t1"], "r1")
		if len(c.Sources) >= 2 && c.Sources[0].Source == "compute" && c.Sources[1].Source == "fast" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, c := getCost(t, s, keys["t1"], "r1")
	if len(c.Sources) < 2 || c.Sources[0].Source != "compute" || c.Sources[1].Source != "fast" {
		t.Errorf("compute and fast source blocked by slow plugin: %+v", c.Sources)
	}
	close(blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPluginFailedChunkKeepsHealthFailing(t *testing.T) {
	var requests int
	s, _, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok"}}})
	})
	execSQL(t, s, context.Background(), `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1', now(), 'tick'), ('r2', now(), 'tick')`)
	drain(t, s)
	s.plugins[0].mu.RLock()
	failing := !s.plugins[0].failing.IsZero()
	s.plugins[0].mu.RUnlock()
	if !failing {
		t.Fatal("later successful chunk cleared health")
	}
}

func TestPluginWriteFailureReleasesOnlyFailedChunk(t *testing.T) {
	broken := true
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		line := pluginLine()
		if broken && req.Runs[0].RunID == "r1" {
			line["details"] = map[string]any{"invalid": "\x00"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok", "lines": []any{line}}}})
	})
	ctx := context.Background()
	finish(t, s, "t1", "r1", StateStopped)
	finish(t, s, "t2", "r2", StateLost)
	n, err := s.drainCosts(ctx)
	if n != 2 || err == nil {
		t.Fatalf("failed write: drained %d, err %v", n, err)
	}
	if got := pending(t, s, "r1"); got != "retry -" {
		t.Errorf("failed chunk claim: %q", got)
	}
	if got := pending(t, s, "r2"); got != "" {
		t.Errorf("successful sibling still queued: %q", got)
	}
	s.plugins[0].mu.RLock()
	failing := !s.plugins[0].failing.IsZero()
	s.plugins[0].mu.RUnlock()
	if !failing {
		t.Error("plugin write failure did not mark health failing")
	}
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "incomplete" || len(c.Lines) != 0 {
		t.Errorf("failed chunk committed answer: %+v", c)
	}
	if code, c := getCost(t, s, keys["t2"], "r2"); code != http.StatusOK || len(c.Lines) != 1 {
		t.Errorf("successful sibling: %d %+v", code, c)
	}
	if code, _ := getCost(t, s, keys["t2"], "r1"); code != http.StatusNotFound {
		t.Errorf("foreign tenant saw failed run: %d", code)
	}
	broken = false
	execSQL(t, s, ctx, `UPDATE cost_pending SET due_at = now() - interval '1 second' WHERE run_id = 'r1'`)
	drain(t, s)
	if _, c := getCost(t, s, keys["t1"], "r1"); c.Status != "complete" || len(c.Lines) != 1 {
		t.Errorf("retry did not recover: %+v", c)
	}
	if got := pending(t, s, "r1"); got != "" {
		t.Errorf("recovered claim still pending: %q", got)
	}
}

func TestStoppedSettlementRestartsOnTerminalTransition(t *testing.T) {
	s, keys, _ := pluginFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if pluginDescribeHandler(w, r) {
			return
		}
		var req struct {
			Runs []pluginRun `json:"runs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "runs": []any{map[string]any{"runId": req.Runs[0].RunID, "status": "ok", "final": false, "lines": []any{pluginLine()}}}})
	})
	finish(t, s, "t1", "r1", StateStopped)
	drain(t, s)
	execSQL(t, s, context.Background(), `UPDATE cost_sources SET settles_left = 0, next_at = NULL WHERE run_id = 'r1' AND source = 'ledger'`)
	finish(t, s, "t1", "r1", StateCancelled)
	drain(t, s)
	_, c := getCost(t, s, keys["t1"], "r1")
	if c.Final || c.Status != "complete" || len(c.Lines) != 1 || c.Lines[0].Final {
		t.Fatalf("terminal answer reused stopped settlement: %+v", c)
	}
	var left *int
	systemScan(t, s, `SELECT settles_left FROM cost_sources WHERE run_id = 'r1' AND source = 'ledger'`, nil, &left)
	if left == nil || *left != 1 {
		t.Fatalf("terminal settlement left: %v", left)
	}
}
