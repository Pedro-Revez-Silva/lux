package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// priceFixture: tenants t1 and t2 with admin keys, an operator key, and a
// host token per tenant for pool "lab" plus a platform one.
func priceFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	keys := map[string]string{"t1": ids.Secret("luxk"), "t2": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'k', $1, ARRAY['admin']), ('k2', 't2', 'k', $2, ARRAY['admin']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["t1"]), ids.Hash(keys["t2"]), ids.Hash(keys["op"]))
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES
		('tok1', 't1', 'lab', 'h1'), ('tok2', 't2', 'lab', 'h2'), ('tokp', NULL, 'lab', 'hp')`)
	return s, keys
}

// call sends method path with key and body (JSON, nil for none) and
// returns the status and the response body.
func call(t *testing.T, s *Server, key, method, path string, body any) (int, string) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// helloAs says hello as host name with token tok (tenant nil: platform)
// and cpus / memory GiB of capacity, and returns the host's id.
func helloAs(t *testing.T, s *Server, tok string, tenant *string, name string, cpus float64, memGiB int64) string {
	t.Helper()
	w, err := s.registerHost(context.Background(), &hostToken{ID: tok, TenantID: tenant, Pool: "lab"}, proto.Hello{
		Name: name, ProtocolVersion: proto.Version, Arch: "arm64",
		Capacity: proto.Capacity{CPUs: cpus, Memory: memGiB * gib, Runs: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.HostID
}

// periods lists a host's rate periods as "price currency cpus mem open|closed".
func periods(t *testing.T, s *Server, hostID string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT format('%s %s %s %s %s', trim_scale(per_hour), currency, cap_cpus, cap_memory / (1024 * 1024 * 1024),
				CASE WHEN valid_to IS NULL THEN 'open' ELSE 'closed' END)
			FROM host_rates WHERE host_id = $1 ORDER BY valid_from`, hostID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// touching counts a host's periods that start the instant the one before
// ended: a change closes one and opens the next at the same instant.
func touching(t *testing.T, s *Server, hostID string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT valid_from, lag(valid_to) OVER (ORDER BY valid_from) AS prev
			FROM host_rates WHERE host_id = $1) x WHERE prev = valid_from`, hostID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func strp(s string) *string { return &s }

// hostCost loads a host's compute input up to now (the database's clock)
// and prices it.
func hostCost(t *testing.T, s *Server, hostID string) computeResult {
	t.Helper()
	ctx := context.Background()
	var in hostCompute
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		var err error
		in, err = loadHostCompute(ctx, tx, hostID, now.Add(-time.Hour), now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A static pool's default price is copied to a host when it registers and
// opens its first period; changing the default reprices no existing host,
// only the hosts registering after. A pool without a price leaves its
// hosts unpriced: no periods.
func TestStaticPoolDefaultPrice(t *testing.T) {
	s, keys := priceFixture(t)
	code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "lab", Provider: "static", HourlyPrice: "0.40", Currency: "USD"})
	if code != http.StatusOK {
		t.Fatalf("put pool: %d %s", code, body)
	}
	var pools struct{ Pools []Pool }
	if code := getJSON(t, s, keys["t1"], "/v1/pools", &pools); code != http.StatusOK || len(pools.Pools) != 1 ||
		pools.Pools[0].HourlyPrice != "0.4" || pools.Pools[0].Currency != "USD" {
		t.Fatalf("pools: %d %+v", code, pools)
	}

	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 open"}; !slices.Equal(got, want) {
		t.Errorf("h1 periods at registration: %v, want %v", got, want)
	}
	// Priced from its first hello: no part of its billed window is missing.
	if res := hostCost(t, s, h1); len(res.Missing) != 0 {
		t.Errorf("h1, priced since it registered, has missing ranges %v", res.Missing)
	}

	if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "lab", Provider: "static", HourlyPrice: "0.5", Currency: "USD"}); code != http.StatusOK {
		t.Fatalf("put pool: %d %s", code, body)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32) // a re-hello
	h2 := helloAs(t, s, "tok1", strp("t1"), "h2", 4, 16)
	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 open"}; !slices.Equal(got, want) {
		t.Errorf("h1 after the pool's default changed: %v, want %v", got, want)
	}
	if got, want := periods(t, s, h2), []string{"0.5 USD 4 16 open"}; !slices.Equal(got, want) {
		t.Errorf("h2, registered after: %v, want %v", got, want)
	}

	// t2 has no pool "lab" of its own with a price: no price, no period.
	h3 := helloAs(t, s, "tok2", strp("t2"), "h3", 8, 32)
	helloAs(t, s, "tok2", strp("t2"), "h3", 4, 32)
	if got := periods(t, s, h3); len(got) != 0 {
		t.Errorf("unpriced host has periods: %v", got)
	}
}

// Prices on a pool are refused unless static, and unless valid.
func TestPoolPriceValidation(t *testing.T) {
	s, keys := priceFixture(t)
	for _, p := range []Pool{
		{Name: "burst", Provider: "ec2", HourlyPrice: "0.40", Currency: "USD"},
		{Name: "lab", Provider: "static", HourlyPrice: "0.40"},
		{Name: "lab", Provider: "static", Currency: "USD"},
		{Name: "lab", Provider: "static", HourlyPrice: "-1", Currency: "USD"},
		{Name: "lab", Provider: "static", HourlyPrice: "1e3", Currency: "USD"},
		{Name: "lab", Provider: "static", HourlyPrice: "0.0000000001", Currency: "USD"},
		{Name: "lab", Provider: "static", HourlyPrice: "1", Currency: "usd"},
	} {
		if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", p); code != http.StatusUnprocessableEntity {
			t.Errorf("pool %+v: %d %s, want 422", p, code, body)
		}
	}
	if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "burst", Provider: "ec2"}); code != http.StatusOK {
		t.Errorf("an ec2 pool without a price: %d %s", code, body)
	}
}

// Setting a host's price closes its open period and opens one at the new
// price; clearing it closes the open period and opens none; setting it
// again starts over. A capacity change on a re-hello does the same, at the
// current price; a re-hello with the same capacity changes nothing; a
// re-hello of a host whose price is gone closes its open period.
func TestHostPricePeriods(t *testing.T) {
	s, keys := priceFixture(t)
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	if got := periods(t, s, h1); len(got) != 0 {
		t.Fatalf("unpriced host has periods: %v", got)
	}

	code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/price", HostPrice{HourlyPrice: "0.40", Currency: "USD"})
	if code != http.StatusOK {
		t.Fatalf("set price: %d %s", code, body)
	}
	var set struct {
		Host        string `json:"host"`
		HourlyPrice string `json:"hourlyPrice"`
	}
	if err := json.Unmarshal([]byte(body), &set); err != nil || set.Host != h1 || set.HourlyPrice != "0.4" {
		t.Errorf("set price answered %s", body)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 open"}; !slices.Equal(got, want) {
		t.Errorf("after set and a same-capacity hello: %v, want %v", got, want)
	}

	if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/"+h1+"/price", HostPrice{HourlyPrice: "0.6", Currency: "USD"}); code != http.StatusOK {
		t.Fatalf("change price: %d %s", code, body)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 4, 32)
	want := []string{"0.4 USD 8 32 closed", "0.6 USD 8 32 closed", "0.6 USD 4 32 open"}
	if got := periods(t, s, h1); !slices.Equal(got, want) {
		t.Errorf("after a price change and a capacity change: %v, want %v", got, want)
	}
	if n := touching(t, s, h1); n != 2 {
		t.Errorf("%d periods start as the one before ends, want 2", n)
	}

	if code, body := call(t, s, keys["t1"], http.MethodDelete, "/v1/hosts/h1/price", nil); code != http.StatusNoContent {
		t.Fatalf("clear price: %d %s", code, body)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 2, 32)
	want[2] = "0.6 USD 4 32 closed"
	if got := periods(t, s, h1); !slices.Equal(got, want) {
		t.Errorf("after clearing: %v, want %v", got, want)
	}

	if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/price", HostPrice{HourlyPrice: "0.3", Currency: "EUR"}); code != http.StatusOK {
		t.Fatalf("set price again: %d %s", code, body)
	}
	if got, want := periods(t, s, h1), append(want, "0.3 EUR 2 32 open"); !slices.Equal(got, want) {
		t.Errorf("after setting it again: %v, want %v", got, want)
	}
	if n := touching(t, s, h1); n != 2 {
		t.Errorf("after a gap with no price: %d periods start as the one before ends, want 2", n)
	}

	// A price cleared other than through the API: the next hello still
	// closes the open period.
	execSQL(t, s, context.Background(), `UPDATE hosts SET hourly_price = NULL, price_currency = NULL WHERE id = $1`, h1)
	helloAs(t, s, "tok1", strp("t1"), "h1", 2, 32)
	if got, want := periods(t, s, h1), append(want, "0.3 EUR 2 32 closed"); !slices.Equal(got, want) {
		t.Errorf("after the price was cleared in the database: %v, want %v", got, want)
	}

	for _, p := range []HostPrice{{}, {HourlyPrice: "0.4"}, {HourlyPrice: "x", Currency: "USD"}, {HourlyPrice: "-0.4", Currency: "USD"}} {
		if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/price", p); code != http.StatusUnprocessableEntity {
			t.Errorf("price %+v: %d %s, want 422", p, code, body)
		}
	}
}

// Who may price a host: its tenant, or an operator. Another tenant does
// not see it (404); a tenant sees a platform host but may not price it
// (403), an operator may. A host its provider launched is not priced here.
func TestHostPriceAccess(t *testing.T) {
	s, keys := priceFixture(t)
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	hp := helloAs(t, s, "tokp", nil, "hp", 8, 32)
	price := HostPrice{HourlyPrice: "0.40", Currency: "USD"}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		var body any
		if method == http.MethodPut {
			body = price
		}
		if code, resp := call(t, s, keys["t2"], method, "/v1/hosts/h1/price", body); code != http.StatusNotFound {
			t.Errorf("%s by another tenant: %d %s, want 404", method, code, resp)
		}
		if code, resp := call(t, s, keys["t1"], method, "/v1/hosts/hp/price", body); code != http.StatusForbidden {
			t.Errorf("%s of a platform host by a tenant: %d %s, want 403", method, code, resp)
		}
	}
	if code, resp := call(t, s, keys["op"], http.MethodPut, "/v1/hosts/hp/price", price); code != http.StatusOK {
		t.Errorf("operator pricing a platform host: %d %s", code, resp)
	}
	if code, resp := call(t, s, keys["op"], http.MethodPut, "/v1/hosts/h1/price?tenant=t1", price); code != http.StatusOK {
		t.Errorf("operator pricing a tenant's host: %d %s", code, resp)
	}
	if got := periods(t, s, hp); len(got) != 1 {
		t.Errorf("platform host periods: %v", got)
	}

	execSQL(t, s, context.Background(), `INSERT INTO hosts (id, tenant_id, name, pool, state, provider_id, provision_requested_at)
		VALUES ('h9', 't1', 'h9', 'burst', 'ready', 'i-9', now())`)
	if code, resp := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h9/price", price); code != http.StatusUnprocessableEntity {
		t.Errorf("pricing a provisioned host: %d %s, want 422", code, resp)
	}
}

// A priced host advertising no capacity at all gets no period: its time is
// missing, never shared out at zero. Once it advertises some, a period
// opens; if it drops back to none, that period closes and none opens. One
// resource is enough: cpus with no memory opens a period.
func TestStaticRateZeroCapacity(t *testing.T) {
	s, keys := priceFixture(t)
	if code, body := call(t, s, keys["t1"], http.MethodPost, "/v1/pools", Pool{Name: "lab", Provider: "static", HourlyPrice: "0.40", Currency: "USD"}); code != http.StatusOK {
		t.Fatalf("put pool: %d %s", code, body)
	}
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 0, 0)
	if got := periods(t, s, h1); len(got) != 0 {
		t.Errorf("a host with no capacity has periods: %v", got)
	}
	if res := hostCost(t, s, h1); len(res.Missing) != 1 || len(res.Unallocated) != 0 {
		t.Errorf("a host with no capacity: missing %v, unallocated %v", res.Missing, res.Unallocated)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	helloAs(t, s, "tok1", strp("t1"), "h1", 0, 0)
	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 closed"}; !slices.Equal(got, want) {
		t.Errorf("after capacity came and went: %v, want %v", got, want)
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 0)
	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 closed", "0.4 USD 8 0 open"}; !slices.Equal(got, want) {
		t.Errorf("after cpus with no memory: %v, want %v", got, want)
	}
}

// A transaction that began before a price change committed, then locks the
// host and syncs, closes the period that change opened no earlier than it
// opened: period boundaries are taken after the lock, not at a
// transaction's start. No period ends at or before it starts, none
// overlap.
func TestStaticRateConcurrentChange(t *testing.T) {
	s, keys := priceFixture(t)
	ctx := context.Background()
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('lux.system', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/price", HostPrice{HourlyPrice: "0.40", Currency: "USD"}); code != http.StatusOK {
		t.Fatalf("set price: %d %s", code, body)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM hosts WHERE id = $1 FOR UPDATE`, h1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET hourly_price = 0.5, price_currency = 'USD' WHERE id = $1`, h1); err != nil {
		t.Fatal(err)
	}
	if err := syncStaticRate(ctx, tx, h1, false); err != nil {
		t.Fatalf("sync after a concurrent change: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if got, want := periods(t, s, h1), []string{"0.4 USD 8 32 closed", "0.5 USD 8 32 open"}; !slices.Equal(got, want) {
		t.Errorf("periods: %v, want %v", got, want)
	}
	var bad int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM host_rates a JOIN host_rates b
			ON a.host_id = b.host_id AND a.valid_from < b.valid_from
			WHERE a.host_id = $1 AND (a.valid_to IS NULL OR a.valid_to > b.valid_from)`, h1).Scan(&bad)
	}); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d pairs of periods overlap", bad)
	}
	if n := touching(t, s, h1); n != 1 {
		t.Errorf("%d periods start as the one before ends, want 1", n)
	}
}

// host_rates refuses a period that ends at or before it starts.
func TestHostRatesCheck(t *testing.T) {
	s, _ := priceFixture(t)
	ctx := context.Background()
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	for _, to := range []string{"10:00", "09:59"} {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
				VALUES ($1, $2, $3, 0.4, 'USD', 8, 1, 'static')`, h1, at("10:00"), at(to))
			return err
		})
		if err == nil {
			t.Errorf("a period from 10:00 to %s was stored", to)
		}
	}
}

// A provisioned host's provider period is not static: a re-hello with a
// new capacity neither closes it nor opens a static one beside it, even
// with a price on the host row.
func TestStaticRateLeavesProviderPeriods(t *testing.T) {
	s, _ := priceFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provider_id, provision_requested_at, hourly_price, price_currency)
		VALUES ('h9', 't1', 'h9', 'lab', 'provisioning', 'i-9', $1, 0.40, 'USD')`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h9', $1, 0.0832, 'USD', 2, $2, 'aws-pricing')`, at("10:00"), 8*gib)
	for _, cpus := range []float64{8, 4} {
		w, err := s.registerHost(ctx, &hostToken{ID: "tok1", TenantID: strp("t1"), Pool: "lab"}, proto.Hello{
			Name: "h9", ProviderID: "i-9", ProtocolVersion: proto.Version, Arch: "arm64",
			Capacity: proto.Capacity{CPUs: cpus, Memory: 32 * gib, Runs: 4},
		})
		if err != nil || w.HostID != "h9" {
			t.Fatalf("hello with %v cpus: %v, host %q", cpus, err, w.HostID)
		}
	}
	var got []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT format('%s %s %s %s %s %s', trim_scale(per_hour), currency, cap_cpus, cap_memory / (1024 * 1024 * 1024),
				source, coalesce(valid_to::text, 'open'))
			FROM host_rates WHERE host_id = 'h9' AND valid_from = $1`, at("10:00"))
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"0.0832 USD 2 8 aws-pricing open"}; !slices.Equal(got, want) {
		t.Errorf("the provider period is now %v, want %v", got, want)
	}
	if got := periods(t, s, "h9"); len(got) != 1 {
		t.Errorf("periods: %v, want only the provider's", got)
	}
}

// A change of currency alone, or of memory alone, closes the open period
// and opens the next.
func TestHostPricePeriodTriggers(t *testing.T) {
	s, keys := priceFixture(t)
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	for _, p := range []HostPrice{{HourlyPrice: "0.40", Currency: "USD"}, {HourlyPrice: "0.40", Currency: "EUR"}} {
		if code, body := call(t, s, keys["t1"], http.MethodPut, "/v1/hosts/h1/price", p); code != http.StatusOK {
			t.Fatalf("set price %+v: %d %s", p, code, body)
		}
	}
	helloAs(t, s, "tok1", strp("t1"), "h1", 8, 16)
	want := []string{"0.4 USD 8 32 closed", "0.4 EUR 8 32 closed", "0.4 EUR 8 16 open"}
	if got := periods(t, s, h1); !slices.Equal(got, want) {
		t.Errorf("after a currency change and a memory change: %v, want %v", got, want)
	}
	if n := touching(t, s, h1); n != 2 {
		t.Errorf("%d periods start as the one before ends, want 2", n)
	}
}

// The database refuses what the API does: a price on an ec2 pool, a
// negative price on a host.
func TestPriceChecks(t *testing.T) {
	s, _ := priceFixture(t)
	ctx := context.Background()
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO pools (id, tenant_id, name, provider, hourly_price, price_currency) VALUES ('pool1', 't1', 'burst', 'ec2', 0.40, 'USD')`, nil},
		{`UPDATE hosts SET hourly_price = -0.40, price_currency = 'USD' WHERE id = $1`, []any{h1}},
	} {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q.sql, q.args...); return err }); err == nil {
			t.Errorf("stored: %s", q.sql)
		}
	}
}

// A retired pool's default price is not copied: a host registering into
// its name gets no price and no period.
func TestRetiredPoolPrice(t *testing.T) {
	s, _ := priceFixture(t)
	execSQL(t, s, context.Background(), `INSERT INTO pools (id, tenant_id, name, provider, hourly_price, price_currency, retired)
		VALUES ('pool1', 't1', 'lab', 'static', 0.40, 'USD', true)`)
	h1 := helloAs(t, s, "tok1", strp("t1"), "h1", 8, 32)
	if got := periods(t, s, h1); len(got) != 0 {
		t.Errorf("a host of a retired pool has periods: %v", got)
	}
}
