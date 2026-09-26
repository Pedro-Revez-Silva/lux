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
		in, err = loadHostCompute(ctx, tx, hostID, now)
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
// current price; a re-hello with the same capacity changes nothing.
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
	if err := json.Unmarshal([]byte(body), &set); err != nil || set.Host != h1 || set.HourlyPrice != "0.40" {
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
