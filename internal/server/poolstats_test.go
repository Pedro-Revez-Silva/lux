package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

func postAs(t *testing.T, s *Server, key, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

type metricsBody struct {
	PoolID  string
	Now     PoolNow
	Samples []PoolSample
}

type costBody struct {
	PoolID     string
	Totals     []MoneyAmount
	TopRuns    []PoolTopRun
	Idle       []MoneyAmount
	Hosts      []PoolHostTime
	HostSeries []PoolHostTime
}

// poolFixture: tenants a and b, each with read keys; a shared platform pool
// "shared" (host hp, 8 cores) running a Run of each tenant, and a tenant
// pool named "own" in each of a and b. Each Run has compute cost on its
// pool's host; the platform host has idle time.
func poolFixture(t *testing.T, s *Server, ctx context.Context) map[string]string {
	t.Helper()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('tb', 'b')`)
	keys := map[string]string{"a": ids.Secret("luxk"), "b": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('ka', 'ta', 'k', $1, ARRAY['read', 'admin']), ('kb', 'tb', 'k', $2, ARRAY['read', 'admin']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["a"]), ids.Hash(keys["b"]), ids.Hash(keys["op"]))
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared) VALUES
		('p-shared', NULL, 'shared', 'static', true), ('p-a', 'ta', 'own', 'static', false), ('p-b', 'tb', 'own', 'static', false)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, capacity) VALUES
		('hp', NULL, 'hp', 'p-shared', 'ready', '{"cpus": 8, "memory": 1000}'),
		('ha', 'ta', 'ha', 'p-a', 'ready', '{"cpus": 2, "memory": 100}'),
		('hb', 'tb', 'hb', 'p-b', 'ready', '{"cpus": 4, "memory": 200}')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, first_started_at, name) VALUES
		('ra', 'ta', '{}', 'running', 'p-shared', 1, now() - interval '3 minutes', 'run-a'),
		('rb', 'tb', '{}', 'running', 'p-shared', 1, now() - interval '3 minutes', 'run-b'),
		('rb2', 'tb', '{}', 'provisioning', 'p-shared', 0, NULL, 'run-b2'),
		('ra-own', 'ta', '{}', 'running', 'p-a', 1, now() - interval '3 minutes', 'own-a'),
		('rb-own', 'tb', '{}', 'running', 'p-b', 1, now() - interval '3 minutes', 'own-b')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES
		('pa', 'ta', 'ra', 'hp', 1, 'running', '{"cpus": 1, "memory": 10}'),
		('pb', 'tb', 'rb', 'hp', 1, 'running', '{"cpus": 3, "memory": 30}'),
		('pao', 'ta', 'ra-own', 'ha', 1, 'running', '{"cpus": 1}'),
		('pbo', 'tb', 'rb-own', 'hb', 1, 'running', '{"cpus": 2}')`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount) VALUES
		($1, 'ta', 'ra', 'compute', 'compute', 'USD', 'hp', 'p-shared', 0.10),
		($1, 'tb', 'rb', 'compute', 'compute', 'USD', 'hp', 'p-shared', 0.30),
		($1, 'tb', 'rb', 'compute', 'compute', 'EUR', 'hp', 'p-shared', 0.05),
		($1, 'ta', 'ra-own', 'compute', 'compute', 'USD', 'ha', 'p-a', 1.00),
		($1, 'tb', 'rb-own', 'compute', 'compute', 'USD', 'hb', 'p-b', 2.00)`, hour)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool_id, allocated, unallocated) VALUES
		($1, 'compute', 'compute', 'USD', 'hp', 'p-shared', 0.40, 0.25)`, hour)
	return keys
}

// poolFixtureMore adds to poolFixture what the figures need to tell right
// from wrong: a tenant b host on the shared pool (tenant a must not count
// it), two launch failures on p-a (one in range, one 3 days old) and one on
// the shared platform pool whose error names an account, a retired pool, a
// plugin-family cost line with no pool_id (it goes by ra's pool), a
// tenant b launch failure on the shared pool (the newest), 11 more
// Runs on the shared pool with cost, and a second queued Run.
func poolFixtureMore(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, retired) VALUES ('p-old', 'ta', 'gone', 'static', true)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, capacity) VALUES
		('hb-shared', 'tb', 'hb-shared', 'p-shared', 'ready', '{"cpus": 16, "memory": 1000}')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, launch_outcome, launch_finished_at, launch_error, terminated_at) VALUES
		('fa-new', 'ta', 'fa-new', 'p-a', 'terminated', now() - interval '10 minutes', 'failed', now() - interval '10 minutes', 'InsufficientInstanceCapacity', now() - interval '10 minutes'),
		('fa-old', 'ta', 'fa-old', 'p-a', 'terminated', now() - interval '3 days', 'failed', now() - interval '3 days', 'old error', now() - interval '3 days'),
		('fp', NULL, 'fp', 'p-shared', 'terminated', now() - interval '5 minutes', 'failed', now() - interval '5 minutes', 'UnauthorizedOperation arn:aws:iam::123456789012:role/x', now() - interval '5 minutes'),
		('fb', 'tb', 'fb', 'p-shared', 'terminated', now() - interval '1 minute', 'failed', now() - interval '1 minute', 'b error', now() - interval '1 minute')`)
	// The reason the provisioner writes with a failed launch.
	execSQL(t, s, ctx, `UPDATE hosts SET state_reason = 'launch failed: ' || launch_error WHERE launch_outcome = 'failed'`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, created_at, needs_host_since, name) VALUES
		('rb3', 'tb', '{}', 'submitted', 'p-shared', 0, now() - interval '20 minutes', now() - interval '20 minutes', 'run-b3')`)
	execSQL(t, s, ctx, `UPDATE runs SET needs_host_since = now() - interval '5 minutes' WHERE id = 'rb2'`)
	execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount) VALUES
		($1, 'ta', 'ra', 'llm', 'llm', 'USD', NULL, NULL, 0.02)`, hour)
	for i := range 11 {
		id := fmt.Sprintf("rx%02d", i)
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, first_started_at, name) VALUES
			($1, 'ta', '{}', 'succeeded', 'p-shared', 1, now() - interval '90 minutes', $1)`, id)
		execSQL(t, s, ctx, `INSERT INTO cost_hourly (hour, tenant_id, run_id, source, family, currency, host_id, pool_id, amount) VALUES
			($1, 'ta', $2, 'compute', 'compute', 'EUR', 'hp', 'p-shared', $3)`, hour, id, fmt.Sprintf("0.%02d", i+1))
	}
}

// The figures of stats, metrics and cost, exactly, with the pool's hosts
// seen as GET /v1/hosts shows them to each caller and a platform pool's
// provider errors only an operator's.
func TestPoolFigures(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	keys := poolFixture(t, s, ctx)
	poolFixtureMore(t, s, ctx)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE pool_samples SET at = at - interval '1 minute'`)
	get := func(who, path string, out any) {
		t.Helper()
		if code := getJSON(t, s, keys[who], path, out); code != http.StatusOK {
			t.Fatalf("%s GET %s: %d", who, path, code)
		}
	}
	// Stats: tenant a sees the shared pool's platform host, not b's; one
	// launch failure in range on its own pool; starts per hour; no retired.
	stats := func(who, q string) map[string]PoolStats {
		t.Helper()
		var list struct{ Pools []PoolStats }
		get(who, "/v1/pools/stats"+q, &list)
		by := map[string]PoolStats{}
		for _, p := range list.Pools {
			by[p.ID] = p
		}
		return by
	}
	a := stats("a", "?since=2h")
	if _, ok := a["p-old"]; ok || len(a) != 2 {
		t.Fatalf("a's pools: %v", a)
	}
	if sh := a["p-shared"]; sh.Hosts["ready"] != 1 || sh.CapacityCPUs != 8 || sh.LaunchFailures != 1 {
		t.Errorf("a's shared pool: hosts %v cpus %v launch failures %d, want 1 ready of 8 cpus and 1", sh.Hosts, sh.CapacityCPUs, sh.LaunchFailures)
	}
	// ra, ra-own started 3 minutes ago (the last hour); the rx Runs 90
	// minutes ago (the first).
	if own := a["p-a"]; own.LaunchFailures != 1 || fmt.Sprint(own.RunsHourly) != "[0 1]" || own.RunsStarted != 1 {
		t.Errorf("a's own pool: launch failures %d, hourly %v", own.LaunchFailures, own.RunsHourly)
	}
	if sh := a["p-shared"]; fmt.Sprint(sh.RunsHourly) != "[11 1]" || sh.RunsStarted != 12 {
		t.Errorf("a's shared pool: hourly %v started %d, want [11 1] and 12", sh.RunsHourly, sh.RunsStarted)
	}
	// Cost on the shared pool for a: ra's compute 0.10 and llm 0.02 (no
	// pool_id: by ra's pool), and 0.01..0.11 EUR.
	if c := money(a["p-shared"].Cost); c["USD"] != "0.12" || c["EUR"] != "0.66" {
		t.Errorf("a's shared cost %v, want USD 0.12, EUR 0.66", c)
	}
	if op := stats("op", "?since=2h")["p-shared"]; op.Hosts["ready"] != 2 || op.CapacityCPUs != 24 || op.LaunchFailures != 2 {
		t.Errorf("operator's shared pool: %+v", op)
	}

	// Metrics now: the same host rule, launch failures in range, the
	// oldest queued, the platform pool's provider error for operators only.
	var m metricsBody
	get("a", "/v1/pools/shared/metrics?owner=platform&since=1h", &m)
	if m.Now.Hosts["ready"] != 1 || m.Now.CapacityCPUs != 8 || m.Now.LaunchFailures != 1 || m.Now.LastLaunchError != "" {
		t.Errorf("a's shared metrics: %+v", m.Now)
	}
	var mb struct {
		metricsBody
		HistoryFrom *time.Time
	}
	get("b", "/v1/pools/shared/metrics?owner=platform&since=1h", &mb)
	if mb.Now.Hosts["ready"] != 2 || mb.Now.Queued != 2 || mb.Now.OldestQueuedAt == nil || time.Since(*mb.Now.OldestQueuedAt) < 19*time.Minute {
		t.Errorf("b's shared metrics: %+v", mb.Now)
	}
	var mo metricsBody
	get("op", "/v1/pools/shared/metrics?owner=platform&since=1h", &mo)
	if mo.Now.LastLaunchError != "b error" || mo.Now.LaunchFailures != 2 {
		t.Errorf("operator's shared metrics: %+v", mo.Now)
	}
	mo = metricsBody{}
	get("op", "/v1/pools/shared/metrics?owner=platform&since=1h&tenant=a", &mo)
	if mo.Now.LastLaunchError != "" {
		t.Errorf("operator narrowed to a sees the platform's launch error: %q", mo.Now.LastLaunchError)
	}
	m = metricsBody{}
	get("a", "/v1/pools/own/metrics?since=1h", &m)
	if m.Now.LaunchFailures != 1 || m.Now.LastLaunchError != "InsufficientInstanceCapacity" {
		t.Errorf("a's own pool metrics: %+v", m.Now)
	}
	get("a", "/v1/pools/own/metrics?since=4d&res=3600", &m)
	if m.Now.LaunchFailures != 2 {
		t.Errorf("a's own pool over 4 days: %d launch failures, want 2", m.Now.LaunchFailures)
	}
	// The same through the host list: a tenant never reads a platform
	// host's provider error, its own hosts' it does; nor through the
	// state reason the provisioner wrote with it.
	var hl struct {
		Hosts []struct {
			ID          string
			StateReason string
			Launch      *HostLaunch
		}
	}
	reason := func(err string) string {
		if err == "" {
			return "launch failed"
		}
		return "launch failed: " + err
	}
	for who, want := range map[string]map[string]string{
		"a":  {"fa-new": "InsufficientInstanceCapacity", "fa-old": "old error", "fp": ""},
		"op": {"fa-new": "InsufficientInstanceCapacity", "fa-old": "old error", "fb": "b error", "fp": "UnauthorizedOperation arn:aws:iam::123456789012:role/x"},
		// An operator narrowed to a: what a sees.
		"op?tenant=a": {"fa-new": "InsufficientInstanceCapacity", "fa-old": "old error", "fp": ""},
	} {
		key, narrow, _ := strings.Cut(who, "?")
		for _, path := range []string{"/v1/hosts?state=launch_failed", "/v1/hosts?state=launch_failed&sort=name"} {
			hl.Hosts = nil
			if narrow != "" {
				path += "&" + narrow
			}
			get(key, path, &hl)
			got := map[string]string{}
			for _, h := range hl.Hosts {
				got[h.ID] = h.Launch.Error
				if h.StateReason != reason(h.Launch.Error) {
					t.Errorf("%s %s: %s stateReason %q, want %q", who, path, h.ID, h.StateReason, reason(h.Launch.Error))
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s %s: errors %v, want %v", who, path, got, want)
			}
		}
		var one hostView
		path := "/v1/hosts/fp"
		if narrow != "" {
			path += "?" + narrow
		}
		get(key, path, &one)
		if one.Launch == nil || one.Launch.Outcome != "failed" || one.Launch.Error != want["fp"] || one.StateReason != reason(want["fp"]) {
			t.Errorf("%s GET /v1/hosts/fp: reason %q, %+v", who, one.StateReason, one.Launch)
		}
	}

	// History: the first sample.
	execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at) VALUES ('p-shared', '', 0, now() - interval '30 minutes')`)
	get("b", "/v1/pools/shared/metrics?owner=platform&since=1h&res=0", &mb)
	if mb.HistoryFrom == nil || time.Since(*mb.HistoryFrom) < 29*time.Minute || len(mb.Samples) != 2 {
		t.Errorf("historyFrom %v, samples %d", mb.HistoryFrom, len(mb.Samples))
	}

	// The sampler: launch failures in its window on p-a, one of them for a
	// launch requested well before the window; no row for the retired pool
	// with nothing on it.
	if n := queryOne[int](t, s, `SELECT count(*) FROM pool_samples WHERE pool_id = 'p-old'`); n != 0 {
		t.Errorf("retired pool sampled: %d rows", n)
	}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, launch_outcome, launch_finished_at, launch_error, terminated_at) VALUES
		('fa-late', 'ta', 'fa-late', 'p-a', 'terminated', now() - interval '2 hours', 'failed', now() - interval '5 minutes', 'late error', now() - interval '5 minutes')`)
	execSQL(t, s, ctx, `DELETE FROM pool_samples`)
	execSQL(t, s, ctx, `DELETE FROM system_samples`)
	execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, window_end) VALUES ('', 0, now() - interval '1 hour', now() - interval '20 minutes')`)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[string](t, s, `SELECT launches || '/' || launch_failures FROM pool_samples WHERE pool_id = 'p-a' AND tenant_id = '' AND res = 0`); got != "1/2" {
		t.Errorf("p-a sample launches/failures %s, want 1/2", got)
	}

	// Cost: the costliest 10 per currency; the plugin family by its Run's
	// pool; host time only for an operator not narrowed to a tenant.
	var c costBody
	get("a", "/v1/pools/shared/cost?owner=platform&since=6h", &c)
	var eur []string
	for _, r := range c.TopRuns {
		if r.Currency == "EUR" {
			eur = append(eur, r.ID+"="+r.Amount)
		}
	}
	if want := "[rx10=0.11 rx09=0.1 rx08=0.09 rx07=0.08 rx06=0.07 rx05=0.06 rx04=0.05 rx03=0.04 rx02=0.03 rx01=0.02]"; fmt.Sprint(eur) != want {
		t.Errorf("EUR top runs %v, want %s", eur, want)
	}
	if got := money(c.Totals); got["USD"] != "0.12" || got["EUR"] != "0.66" {
		t.Errorf("a's shared cost totals %v", got)
	}
	c = costBody{}
	get("op", "/v1/pools/shared/cost?owner=platform&since=6h&tenant=a", &c)
	if len(c.Idle) != 0 || len(c.Hosts) != 0 || len(c.HostSeries) != 0 || money(c.Totals)["USD"] != "0.12" {
		t.Errorf("operator narrowed to a: idle %v hosts %v series %v totals %v", c.Idle, c.Hosts, c.HostSeries, c.Totals)
	}
}

func money(ms []MoneyAmount) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m.Currency] = m.Amount
	}
	return out
}

// Pool metrics and cost: two tenants on a shared platform pool each see
// the pool's hosts and capacity but only their own Runs, allocation and
// cost; a tenant pool of the same name in each tenant is its own; the
// operator sees the whole pool and its idle host time, which tenants never
// see. Samples are read by the pool's id.
func TestPoolMetricsAndCostTenantIsolation(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	keys := poolFixture(t, s, ctx)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	// The database's clock may run ahead of this process's: samples are
	// stamped by it, ranges end at this process's now.
	execSQL(t, s, ctx, `UPDATE pool_samples SET at = at - interval '1 minute'`)
	get := func(who, path string, out any) {
		t.Helper()
		if code := getJSON(t, s, keys[who], path, out); code != http.StatusOK {
			t.Fatalf("%s GET %s: %d", who, path, code)
		}
	}
	for who, want := range map[string]struct {
		alloc            float64
		running, queued  int
		cost             map[string]string
		idle, hostDetail bool
	}{
		"a":  {1, 1, 0, map[string]string{"USD": "0.1"}, false, false},
		"b":  {3, 1, 1, map[string]string{"USD": "0.3", "EUR": "0.05"}, false, false},
		"op": {4, 2, 1, map[string]string{"USD": "0.4", "EUR": "0.05"}, true, true},
	} {
		var m metricsBody
		get(who, "/v1/pools/shared/metrics?owner=platform&since=1h", &m)
		if m.PoolID != "p-shared" || m.Now.CapacityCPUs != 8 || m.Now.Hosts["ready"] != 1 {
			t.Errorf("%s: shared pool capacity and hosts %+v", who, m.Now)
		}
		if m.Now.AllocatedCPUs != want.alloc || m.Now.Running != want.running || m.Now.Queued != want.queued {
			t.Errorf("%s: now alloc %v running %d queued %d, want %v %d %d", who, m.Now.AllocatedCPUs, m.Now.Running, m.Now.Queued, want.alloc, want.running, want.queued)
		}
		if len(m.Samples) != 1 {
			t.Fatalf("%s: samples %+v", who, m.Samples)
		}
		if sm := m.Samples[0]; sm.CapacityCPUs != 8 || sm.AllocatedCPUs != want.alloc || sm.Running != want.running || sm.Queued != want.queued {
			t.Errorf("%s: sample %+v, want capacity 8, alloc %v, running %d, queued %d", who, sm, want.alloc, want.running, want.queued)
		}
		var c costBody
		get(who, "/v1/pools/shared/cost?owner=platform&since=6h", &c)
		if got := money(c.Totals); len(got) != len(want.cost) || got["USD"] != want.cost["USD"] || got["EUR"] != want.cost["EUR"] {
			t.Errorf("%s: cost totals %v, want %v", who, got, want.cost)
		}
		for _, r := range c.TopRuns {
			if who == "a" && r.ID != "ra" || who == "b" && r.ID == "ra" {
				t.Errorf("%s: top runs include %s", who, r.ID)
			}
		}
		if (len(c.Idle) > 0) != want.idle || (len(c.Hosts) > 0) != want.hostDetail {
			t.Errorf("%s: idle %v hosts %v", who, c.Idle, c.Hosts)
		}
		if want.idle && (money(c.Idle)["USD"] != "0.25" || c.Hosts[0].Allocated != "0.4") {
			t.Errorf("operator idle %v hosts %+v", c.Idle, c.Hosts)
		}
	}
	// Same name, two tenants: each its own.
	for who, want := range map[string]struct {
		id   string
		cpus float64
		cost string
	}{"a": {"p-a", 2, "1"}, "b": {"p-b", 4, "2"}} {
		var m metricsBody
		get(who, "/v1/pools/own/metrics?since=1h", &m)
		var c costBody
		get(who, "/v1/pools/own/cost?since=6h", &c)
		if m.PoolID != want.id || m.Now.CapacityCPUs != want.cpus || c.PoolID != want.id || money(c.Totals)["USD"] != want.cost {
			t.Errorf("%s own: %s %v %v, want %s %v %s", who, m.PoolID, m.Now.CapacityCPUs, c.Totals, want.id, want.cpus, want.cost)
		}
	}
	// The operator, across tenants: the name is ambiguous until narrowed.
	var m metricsBody
	if code := getJSON(t, s, keys["op"], "/v1/pools/own/metrics", &m); code != http.StatusConflict {
		t.Errorf("operator, ambiguous name: %d", code)
	}
	get("op", "/v1/pools/own/metrics?tenant=b&since=1h", &m)
	if m.PoolID != "p-b" {
		t.Errorf("operator narrowed to b: %s", m.PoolID)
	}
	// The list: one read for every pool, each tenant's own figures.
	var list struct{ Pools []PoolStats }
	get("a", "/v1/pools/stats", &list)
	byID := map[string]PoolStats{}
	for _, p := range list.Pools {
		byID[p.ID] = p
	}
	if _, seen := byID["p-b"]; seen || len(byID) != 2 {
		t.Fatalf("tenant a sees pools %v", byID)
	}
	if sh := byID["p-shared"]; sh.AllocatedCPUs != 1 || sh.CapacityCPUs != 8 || sh.RunsStarted != 1 || money(sh.Cost)["USD"] != "0.1" || len(sh.Cost) != 1 {
		t.Errorf("a's view of shared: %+v", sh)
	}
}

// A pool's history follows its id: a rename keeps it, and so does removing
// the pool and setting it again under its name (DELETE then POST revives
// the retired row, with its id).
func TestPoolMetricsRenameAndRecreate(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	keys := poolFixture(t, s, ctx)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE pool_samples SET at = at - interval '1 minute'`)
	if code := postAs(t, s, keys["a"], "/v1/pools/own/rename", `{"name": "renamed"}`); code != http.StatusOK {
		t.Fatalf("rename: %d", code)
	}
	var m metricsBody
	if code := getJSON(t, s, keys["a"], "/v1/pools/renamed/metrics?since=1h", &m); code != http.StatusOK || m.PoolID != "p-a" || len(m.Samples) != 1 || m.Samples[0].CapacityCPUs != 2 {
		t.Fatalf("renamed pool: %d %s %+v", code, m.PoolID, m.Samples)
	}
	var c costBody
	if code := getJSON(t, s, keys["a"], "/v1/pools/renamed/cost?since=6h", &c); code != http.StatusOK || money(c.Totals)["USD"] != "1" {
		t.Fatalf("renamed pool cost: %d %+v", code, c.Totals)
	}
	// Removed, then set again under its name: the same pool, its history
	// continued.
	if code, body := call(t, s, keys["a"], http.MethodDelete, "/v1/pools/renamed", nil); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body := call(t, s, keys["a"], http.MethodPost, "/v1/pools", Pool{Name: "renamed", Provider: "static"}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("recreate: %d %s", code, body)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM pools WHERE tenant_id = 'ta' AND name = 'renamed'`); n != 1 {
		t.Fatalf("pools named renamed: %d", n)
	}
	m = metricsBody{}
	if code := getJSON(t, s, keys["a"], "/v1/pools/renamed/metrics?since=1h", &m); code != http.StatusOK || m.PoolID != "p-a" || len(m.Samples) != 1 {
		t.Fatalf("re-created pool: %d %s %+v", code, m.PoolID, m.Samples)
	}
	c = costBody{}
	if code := getJSON(t, s, keys["a"], "/v1/pools/renamed/cost?since=6h", &c); code != http.StatusOK || c.PoolID != "p-a" || money(c.Totals)["USD"] != "1" {
		t.Fatalf("re-created pool, cost: %d %+v", code, c.Totals)
	}
}

// Pool samples roll up like the system's: levels averaged, hosts by state
// the bucket's last, flows summed; and a second pass changes nothing.
func TestRollupPoolSamples(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('p1', 'burst', 'ec2')`)
	for i, at := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
		execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, hosts, cap_cpus, running, started, launch_failures)
			VALUES ('p1', '', 0, $1, $2, $3, $4, 1, $5)`, hour.Add(at), map[string]int{"ready": i}, float64(8*(i+1)), i*2, i%2)
	}
	for range 2 {
		if err := s.rollupHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := map[int][]string{}
	for _, res := range []int{60, 3600} {
		got[res] = queryRows(t, s, res)
	}
	want := map[int][]string{60: {"12/1/2/1/1", "28/5/2/1/3"}, 3600: {"20/3/4/2/3"}}
	for res, w := range want {
		if len(got[res]) != len(w) {
			t.Fatalf("res %d: %v", res, got[res])
		}
		for i := range w {
			if g := got[res][i]; g != w[i] {
				t.Errorf("res %d bucket %d: %s, want %s (cap/running/started/failures/ready)", res, i, g, w[i])
			}
		}
	}
}

func queryRows(t *testing.T, s *Server, res int) []string {
	t.Helper()
	var out []string
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT cap_cpus::text || '/' || running || '/' || started || '/' || launch_failures || '/' || coalesce(hosts->>'ready', '-')
			FROM pool_samples WHERE res = $1 ORDER BY at`, res)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
