package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

// A pool's history follows its id: a rename keeps it, and a pool removed
// and a new one set with its old name keep theirs apart.
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
	// The old name, taken by a new pool: none of the old one's history.
	execSQL(t, s, ctx, `UPDATE pools SET retired = true WHERE id = 'p-a'`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p-a2', 'ta', 'own', 'static')`)
	m = metricsBody{}
	if code := getJSON(t, s, keys["a"], "/v1/pools/own/metrics?since=1h", &m); code != http.StatusOK || m.PoolID != "p-a2" || len(m.Samples) != 0 {
		t.Fatalf("new pool of an old name: %d %s %+v", code, m.PoolID, m.Samples)
	}
	c = costBody{}
	if code := getJSON(t, s, keys["a"], "/v1/pools/own/cost?since=6h", &c); code != http.StatusOK || len(c.Totals) != 0 {
		t.Fatalf("new pool of an old name, cost: %d %+v", code, c.Totals)
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
