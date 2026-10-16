package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// marketFake is an ec2-like provider and price provider: templates are
// {"region", "instanceType", "fallbackInstanceTypes"}; every answer comes
// from its maps, and an entry in an err map fails that call.
type marketFake struct {
	fakeLaunchProvider
	mu       sync.Mutex
	zones    map[string][]Zone // by region
	zonesErr error
	scores   map[string]map[string]int // type → zone id → score
	scoreErr map[string]error          // by type
	onDemand map[string]string         // type → price
	spot     map[[2]string]string      // (zone, type) → price
	spotErr  map[[2]string]error
	types    map[string]InstanceTypeInfo
	typesErr error
	calls    []string
	// during, when set, runs inside each call (after it is recorded), as
	// a provider call that blocks or outlasts the pass.
	during func(call string)
}

func (f *marketFake) call(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during(c)
	}
}

func (f *marketFake) MarketScope(raw json.RawMessage) (string, []string, error) {
	var t struct {
		Region       string   `json:"region"`
		InstanceType string   `json:"instanceType"`
		Fallback     []string `json:"fallbackInstanceTypes"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", nil, err
	}
	var out []string
	for _, it := range append([]string{t.InstanceType}, t.Fallback...) {
		if it != "" && !slices.Contains(out, it) {
			out = append(out, it)
		}
	}
	return t.Region, out, nil
}

func (f *marketFake) Zones(_ context.Context, raw json.RawMessage) ([]Zone, error) {
	region, _, _ := f.MarketScope(raw)
	f.call("zones " + region)
	if f.zonesErr != nil {
		return nil, f.zonesErr
	}
	return f.zones[region], nil
}

func (f *marketFake) PlacementScores(_ context.Context, region, it string) (map[string]int, error) {
	f.call("score " + region + " " + it)
	if err := f.scoreErr[it]; err != nil {
		return nil, err
	}
	return f.scores[it], nil
}

func (f *marketFake) OnDemand(_ context.Context, region, it string) (HourlyRate, error) {
	f.call("on-demand " + region + " " + it)
	p, ok := f.onDemand[it]
	if !ok {
		return HourlyRate{}, errors.New("no price data")
	}
	return HourlyRate{PerHour: p, Currency: "USD"}, nil
}

func (f *marketFake) SpotHistory(_ context.Context, zone, it string, _, to time.Time) ([]SpotRate, error) {
	f.call("spot " + zone + " " + it)
	if err := f.spotErr[[2]string{zone, it}]; err != nil {
		return nil, err
	}
	p, ok := f.spot[[2]string{zone, it}]
	if !ok {
		return nil, nil
	}
	// An older price, and the latest, which a sample takes.
	return []SpotRate{{At: to.Add(-3 * time.Hour), HourlyRate: HourlyRate{"9.9", "USD"}}, {At: to.Add(-time.Hour), HourlyRate: HourlyRate{p, "USD"}}}, nil
}

func (f *marketFake) BlockStorage(context.Context, string, string) (BlockStoragePrice, error) {
	return BlockStoragePrice{}, errors.New("not asked")
}

func (f *marketFake) DataTransfer(context.Context, string) (DataTransferPrices, error) {
	return DataTransferPrices{}, errors.New("not asked")
}

func (f *marketFake) InstanceTypes(_ context.Context, region string, names []string) ([]InstanceTypeInfo, error) {
	f.call("describe " + region + " " + strings.Join(names, ","))
	if f.typesErr != nil {
		return nil, f.typesErr
	}
	var out []InstanceTypeInfo
	for _, n := range names {
		if info, ok := f.types[n]; ok {
			out = append(out, info)
		}
	}
	return out, nil
}

func (f *marketFake) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	slices.Sort(c)
	return c
}

// marketServer is a test server whose ec2 provider and prices are f, with
// the provisioner lease held by this process, and a log to read.
func marketServer(t *testing.T, f *marketFake, candidates ...string) (*Server, *bytes.Buffer) {
	t.Helper()
	s := testServer(t)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.cfg.Providers = map[string]Provider{"ec2": f}
	s.cfg.Costs.Prices = map[string]PriceProvider{"ec2": f}
	s.cfg.Market = MarketConfig{Every: time.Hour, Candidates: candidates}
	execSQL(t, s, context.Background(), `INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', $1, now() + interval '1 minute')`, instanceID)
	return s, &logs
}

type marketRowOut struct {
	Region, Zone   string
	ZoneID         *string
	Type           string
	Spot, OnDemand *string
	Currency       *string
	Score          *int
}

func (r marketRowOut) String() string {
	s := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	sc := "-"
	if r.Score != nil {
		sc = fmt.Sprint(*r.Score)
	}
	return fmt.Sprintf("%s %s/%s %s spot=%s od=%s %s score=%s", r.Region, r.Zone, s(r.ZoneID), r.Type, s(r.Spot), s(r.OnDemand), s(r.Currency), sc)
}

func marketRows(t *testing.T, s *Server) []string {
	t.Helper()
	var out []string
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT region, zone, zone_id, instance_type,
				trim_scale(spot_per_hour)::text, trim_scale(on_demand_per_hour)::text, currency, placement_score
			FROM market_samples ORDER BY region, instance_type, zone`)
		if err != nil {
			return err
		}
		rs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[marketRowOut])
		for _, r := range rs {
			out = append(out, r.String())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// One pass samples, per region, every pool's instanceType and fallbacks
// plus the configured candidates, once each, in every zone its pools'
// subnets are in. Each figure fails on its own: a refused placement score
// (no IAM permission), a missing on-demand price or a failed spot call
// leaves that column NULL and keeps the rest; the pass logs one warning.
// Retired pools are not sampled, nor p5, whose provider has no
// MarketProvider here; the zones of a pool's hosts are not added to those
// its subnets list.
func TestSampleMarketPartialFailures(t *testing.T) {
	f := &marketFake{
		zones: map[string][]Zone{
			"us-east-1": {{"us-east-1a", "use1-az1"}, {"us-east-1b", "use1-az2"}},
			"eu-west-1": {{"eu-west-1a", "euw1-az3"}},
		},
		scores:   map[string]map[string]int{"m7g.large": {"use1-az1": 7}, "c7g.large": {"use1-az1": 3, "use1-az2": 9, "euw1-az3": 5}},
		scoreErr: map[string]error{"m7i.large": errors.New("UnauthorizedOperation: not authorized to perform ec2:GetSpotPlacementScores")},
		onDemand: map[string]string{"m7i.large": "0.1008", "m7g.large": "0.0816"},
		spot: map[[2]string]string{
			{"us-east-1a", "m7i.large"}: "0.0400", {"us-east-1b", "m7i.large"}: "0.0410",
			{"us-east-1a", "m7g.large"}: "0.0300", {"us-east-1a", "c7g.large"}: "0.0250", {"us-east-1b", "c7g.large"}: "0.0260",
			{"eu-west-1a", "c7g.large"}: "0.0270",
		},
		spotErr: map[[2]string]error{{"us-east-1b", "m7g.large"}: errors.New("RequestLimitExceeded")},
	}
	s, logs := marketServer(t, f, "c7g.large")
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template, retired) VALUES
		('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large","fallbackInstanceTypes":["m7g.large"]}', false),
		('p2', 'more', 'ec2', '{"region":"us-east-1","instanceType":"m7g.large"}', false),
		('p3', 'eu', 'ec2', '{"region":"eu-west-1"}', false),
		('p4', 'old', 'ec2', '{"region":"ap-south-1","instanceType":"r7i.large"}', true),
		('p5', 'static', 'static', '{}', false)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, pool_id, zone) VALUES ('h1', 'h1', 'terminated', 'p1', 'us-east-1c')`)

	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"eu-west-1 eu-west-1a/euw1-az3 c7g.large spot=0.027 od=- USD score=5",
		"us-east-1 us-east-1a/use1-az1 c7g.large spot=0.025 od=- USD score=3",
		"us-east-1 us-east-1b/use1-az2 c7g.large spot=0.026 od=- USD score=9",
		"us-east-1 us-east-1a/use1-az1 m7g.large spot=0.03 od=0.0816 USD score=7",
		"us-east-1 us-east-1b/use1-az2 m7g.large spot=- od=0.0816 USD score=-",
		"us-east-1 us-east-1a/use1-az1 m7i.large spot=0.04 od=0.1008 USD score=-",
		"us-east-1 us-east-1b/use1-az2 m7i.large spot=0.041 od=0.1008 USD score=-",
	}
	if got := marketRows(t, s); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	scoreCalls := 0
	for _, c := range f.takeCalls() {
		if strings.HasPrefix(c, "score ") {
			scoreCalls++
		}
		if strings.Contains(c, "ap-south-1") || strings.Contains(c, "r7i") {
			t.Errorf("a retired pool was sampled: %s", c)
		}
	}
	// One GetSpotPlacementScores per (region, type): us-east-1 × 3, eu-west-1 × 1.
	if scoreCalls != 4 {
		t.Errorf("placement score calls %d, want 4", scoreCalls)
	}
	warns := strings.Count(logs.String(), "level=WARN")
	if warns != 1 || !strings.Contains(logs.String(), "GetSpotPlacementScores") || !strings.Contains(logs.String(), "RequestLimitExceeded") {
		t.Fatalf("want one warning naming the failures, got %d:\n%s", warns, logs)
	}

	// Within the hour nothing is due: no call, no row, failed or not.
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.takeCalls(); len(calls) != 0 || len(marketRows(t, s)) != len(want) {
		t.Fatalf("second pass within the hour: calls %v, %d rows", calls, len(marketRows(t, s)))
	}
}

// Only the provisioner lease's live holder samples; a candidate type is
// sampled again once its last sample is older than Every, not before.
func TestSampleMarketLeaseAndCadence(t *testing.T) {
	f := &marketFake{zones: map[string][]Zone{"us-east-1": {{"us-east-1a", "use1-az1"}}},
		onDemand: map[string]string{"m7i.large": "0.1"}, spot: map[[2]string]string{{"us-east-1a", "m7i.large"}: "0.04"}}
	s, _ := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large"}')`)
	aged := func(by string) {
		execSQL(t, s, ctx, `UPDATE market_samples SET sampled_at = sampled_at - $1::interval`, by)
		d, _ := time.ParseDuration(by)
		s.marketMu.Lock()
		for k, at := range s.marketTried {
			s.marketTried[k] = at.Add(-d)
		}
		s.marketMu.Unlock()
	}

	for _, holder := range []struct{ id, expires string }{{"other", "1 minute"}, {instanceID, "-1 second"}} {
		execSQL(t, s, ctx, `UPDATE leases SET holder = $1, expires_at = now() + $2::interval WHERE name = 'provisioner'`, holder.id, holder.expires)
		if err := s.sampleMarketPass(ctx); err != nil {
			t.Fatal(err)
		}
		if calls := f.takeCalls(); len(calls) != 0 || len(marketRows(t, s)) != 0 {
			t.Fatalf("lease held by %s, expiring in %s: calls %v, rows %v", holder.id, holder.expires, calls, marketRows(t, s))
		}
	}

	execSQL(t, s, ctx, `UPDATE leases SET holder = $1, expires_at = now() + interval '1 minute' WHERE name = 'provisioner'`, instanceID)
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(marketRows(t, s)); n != 1 {
		t.Fatalf("with the lease: %d rows, want 1", n)
	}
	// Half an hour later: not due.
	aged("30m")
	f.takeCalls()
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.takeCalls(); len(calls) != 0 {
		t.Fatalf("half an hour later: calls %v, want none", calls)
	}
	// An hour later (the sample and this process's attempt both aged).
	aged("31m")
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(marketRows(t, s)); n != 2 {
		t.Fatalf("an hour later: %d rows, want 2", n)
	}
}

// When the provisioner lease moves to another luxd while a provider call
// is in flight, the pass writes nothing it fetched after that and stops:
// neither the type's rows nor the catalog's, nor any later type's.
func TestSampleMarketStopsWhenLeaseLost(t *testing.T) {
	two := 2
	for blockOn, wantCatalog := range map[string]int{
		"score us-east-1 m7i.large":              2, // described before the lease moved
		"describe us-east-1 m7i.large,m7g.large": 0,
	} {
		t.Run(strings.Fields(blockOn)[0], func(t *testing.T) {
			f := &marketFake{zones: map[string][]Zone{"us-east-1": {{"us-east-1a", "use1-az1"}}},
				onDemand: map[string]string{"m7i.large": "0.1", "m7g.large": "0.08"},
				types:    map[string]InstanceTypeInfo{"m7i.large": {Name: "m7i.large", VCPUs: &two}, "m7g.large": {Name: "m7g.large", VCPUs: &two}}}
			s, _ := marketServer(t, f)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large","fallbackInstanceTypes":["m7g.large"]}')`)
			// The call blocks until another luxd has taken the lease.
			f.during = func(c string) {
				if c == blockOn {
					execSQL(t, s, ctx, `UPDATE leases SET holder = 'other', expires_at = now() + interval '1 minute' WHERE name = 'provisioner'`)
				}
			}
			if err := s.sampleMarketPass(ctx); err != nil {
				t.Fatal(err)
			}
			var catalogRows int
			if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM instance_types`).Scan(&catalogRows)
			}); err != nil {
				t.Fatal(err)
			}
			calls := f.takeCalls()
			if rows := marketRows(t, s); len(rows) != 0 || catalogRows != wantCatalog {
				t.Fatalf("after the lease moved: market rows %v, %d catalog rows, want none and %d", rows, catalogRows, wantCatalog)
			}
			if slices.ContainsFunc(calls, func(c string) bool { return !strings.HasPrefix(c, "describe ") && strings.HasSuffix(c, "m7g.large") }) {
				t.Fatalf("the pass went on to the next type: %v", calls)
			}
		})
	}
}

// A type is marked attempted only when its own calls start: when the
// pass ends during the first type, the next pass samples the second, and
// does not retry the first.
func TestSampleMarketUntouchedTypeStaysDue(t *testing.T) {
	f := &marketFake{zones: map[string][]Zone{"us-east-1": {{"us-east-1a", "use1-az1"}}},
		onDemand: map[string]string{"m7i.large": "0.1", "m7g.large": "0.08"}}
	s, _ := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large","fallbackInstanceTypes":["m7g.large"]}')`)
	// The pass ends inside m7i.large's score call, as a deadline would,
	// without depending on how fast the database is.
	passCtx, cancel := context.WithCancel(ctx)
	f.during = func(c string) {
		if c == "score us-east-1 m7i.large" {
			cancel()
		}
	}
	_ = s.sampleMarketPass(passCtx)
	if calls := f.takeCalls(); slices.Contains(calls, "score us-east-1 m7g.large") || !slices.Contains(calls, "score us-east-1 m7i.large") {
		t.Fatalf("first pass: %v, want m7i.large reached and m7g.large not", calls)
	}

	f.during = nil
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	calls := f.takeCalls()
	if !slices.Contains(calls, "score us-east-1 m7g.large") || slices.Contains(calls, "score us-east-1 m7i.large") {
		t.Fatalf("second pass calls %v, want m7g.large sampled and m7i.large not retried", calls)
	}
	if rows := marketRows(t, s); len(rows) != 1 || !strings.Contains(rows[0], "m7g.large") {
		t.Fatalf("rows %v, want one for m7g.large", rows)
	}
}

// A pass that wrote no row (zones refused, no hosts to fall back on) is
// not repeated a minute later: its types were attempted.
func TestSampleMarketNoRowsNotRetried(t *testing.T) {
	f := &marketFake{zonesErr: errors.New("UnauthorizedOperation: ec2:DescribeSubnets"), onDemand: map[string]string{"m7i.large": "0.1"}}
	s, _ := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large"}')`)
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.takeCalls(); !slices.Contains(calls, "score us-east-1 m7i.large") || len(marketRows(t, s)) != 0 {
		t.Fatalf("first pass: calls %v, rows %v", calls, marketRows(t, s))
	}
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.takeCalls(); len(calls) != 0 {
		t.Fatalf("second pass: calls %v, want none", calls)
	}
}

// A pass that ends while a type writes nothing (no zones) marks no
// further type: the next one stays due rather than being backed off for
// calls made on a dead context.
func TestSampleMarketDeadPassMarksNoMore(t *testing.T) {
	f := &marketFake{zonesErr: errors.New("UnauthorizedOperation: ec2:DescribeSubnets"),
		onDemand: map[string]string{"m7i.large": "0.1", "m7g.large": "0.08"}}
	s, _ := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large","fallbackInstanceTypes":["m7g.large"]}')`)
	passCtx, cancel := context.WithCancel(ctx)
	f.during = func(c string) {
		if c == "score us-east-1 m7i.large" {
			cancel()
		}
	}
	if err := s.sampleMarketPass(passCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first pass: %v, want the context's error", err)
	}
	if calls := f.takeCalls(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasSuffix(c, "m7g.large") && !strings.HasPrefix(c, "describe ") }) {
		t.Fatalf("first pass went on to m7g.large on a dead context: %v", calls)
	}
	f.during = nil
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.takeCalls(); !slices.Contains(calls, "score us-east-1 m7g.large") || slices.Contains(calls, "score us-east-1 m7i.large") {
		t.Fatalf("second pass calls %v, want m7g.large sampled and m7i.large not retried", calls)
	}
}

// When the provider cannot list a pool's zones (no ec2:DescribeSubnets),
// the zones its hosts were launched in are sampled, by name only: prices
// still come, a placement score cannot be matched without the zone id.
func TestSampleMarketZonesFromHosts(t *testing.T) {
	f := &marketFake{zonesErr: errors.New("UnauthorizedOperation: ec2:DescribeSubnets"),
		scores:   map[string]map[string]int{"m7i.large": {"use1-az1": 8}},
		onDemand: map[string]string{"m7i.large": "0.1"},
		spot:     map[[2]string]string{{"us-east-1c", "m7i.large"}: "0.05"}}
	s, logs := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large"}')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, pool_id, zone) VALUES ('h1', 'h1', 'terminated', 'p1', 'us-east-1c')`)
	if err := s.sampleMarketPass(ctx); err != nil {
		t.Fatal(err)
	}
	want := "us-east-1 us-east-1c/- m7i.large spot=0.05 od=0.1 USD score=-"
	if got := marketRows(t, s); len(got) != 1 || got[0] != want {
		t.Fatalf("rows %v, want [%s]", got, want)
	}
	if !strings.Contains(logs.String(), "DescribeSubnets") {
		t.Fatalf("the zones failure was not logged:\n%s", logs)
	}
}

// The rollup pass deletes market samples older than the hourly history's
// retention, and keeps the rest.
func TestMarketSamplesRetention(t *testing.T) {
	s := testServer(t)
	s.cfg.HistoryHours = 48 * time.Hour
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO market_samples (provider, region, zone, instance_type, sampled_at, spot_per_hour) VALUES
		('ec2', 'us-east-1', 'us-east-1a', 'm7i.large', now() - interval '49 hours', 0.04),
		('ec2', 'us-east-1', 'us-east-1a', 'm7i.large', now() - interval '47 hours', 0.05)`)
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if got := marketRows(t, s); len(got) != 1 || !strings.Contains(got[0], "spot=0.05") {
		t.Fatalf("after retention: %v, want only the 47h-old row", got)
	}
}

// The sampler caches each candidate type's description in instance_types:
// one describe per region for the types without a fresh row, none again
// within a day, and again once a row is a day old or a new candidate
// appears. A failed describe is logged and changes nothing.
func TestCatalogRefresh(t *testing.T) {
	two, yes, no := 2, true, false
	gbps := 12.5
	f := &marketFake{zones: map[string][]Zone{"us-east-1": {{"us-east-1a", "use1-az1"}}},
		types: map[string]InstanceTypeInfo{
			"m7i.large": {Name: "m7i.large", VCPUs: &two, MemoryBytes: 8 << 30, Architectures: []string{"x86_64"},
				NestedVirtualization: &yes, Burstable: &no, NetworkBaselineGbps: &gbps, NetworkPerformance: "Up to 12.5 Gigabit"},
			"t4g.small": {Name: "t4g.small", VCPUs: &two, MemoryBytes: 2 << 30, Architectures: []string{"arm64"},
				NestedVirtualization: &no, Burstable: &yes},
		}}
	s, logs := marketServer(t, f)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, template) VALUES ('p1', 'burst', 'ec2', '{"region":"us-east-1","instanceType":"m7i.large"}')`)
	catalog := func() []string {
		t.Helper()
		var out []string
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT format('%s %s vcpus=%s mem=%s arch=%s nested=%s burst=%s net=%s/%s', region, instance_type, vcpus,
					memory_bytes, architectures, nested_virtualization, burstable, network_baseline_gbps, network_performance)
				FROM instance_types ORDER BY instance_type`)
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
	describes := func() (n []string) {
		for _, c := range f.takeCalls() {
			if strings.HasPrefix(c, "describe ") {
				n = append(n, c)
			}
		}
		return n
	}
	pass := func() {
		t.Helper()
		if err := s.sampleMarketPass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ageAttempts := func() {
		s.marketMu.Lock()
		for k, at := range s.marketTried {
			s.marketTried[k] = at.Add(-2 * time.Hour)
		}
		s.marketMu.Unlock()
		execSQL(t, s, ctx, `UPDATE market_samples SET sampled_at = sampled_at - interval '2 hours'`)
	}

	pass()
	want := []string{"us-east-1 m7i.large vcpus=2 mem=8589934592 arch={x86_64} nested=t burst=f net=12.5/Up to 12.5 Gigabit"}
	if got := catalog(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("catalog %v, want %v", got, want)
	}
	if d := describes(); fmt.Sprint(d) != "[describe us-east-1 m7i.large]" {
		t.Fatalf("describes %v", d)
	}

	// Due again for prices an hour later; the catalog row is still fresh.
	ageAttempts()
	pass()
	if d := describes(); len(d) != 0 {
		t.Fatalf("a fresh row was described again: %v", d)
	}

	// A new candidate: described at once, alone.
	s.cfg.Market.Candidates = []string{"t4g.small"}
	pass()
	if d := describes(); fmt.Sprint(d) != "[describe us-east-1 t4g.small]" {
		t.Fatalf("describes for a new candidate %v", d)
	}
	if got := catalog(); len(got) != 2 || !strings.Contains(got[1], "t4g.small vcpus=2 mem=2147483648 arch={arm64} nested=f burst=t net=/") {
		t.Fatalf("catalog %v", got)
	}

	// A day later both are refetched; a failure leaves the rows as they were.
	execSQL(t, s, ctx, `UPDATE instance_types SET fetched_at = fetched_at - interval '25 hours'`)
	f.typesErr = errors.New("UnauthorizedOperation: ec2:DescribeInstanceTypes")
	ageAttempts()
	before := catalog()
	pass()
	if d := describes(); fmt.Sprint(d) != "[describe us-east-1 m7i.large,t4g.small]" {
		t.Fatalf("describes a day later %v", d)
	}
	if got := catalog(); fmt.Sprint(got) != fmt.Sprint(before) || !strings.Contains(logs.String(), "DescribeInstanceTypes") {
		t.Fatalf("after a failed describe: %v (was %v); log:\n%s", got, before, logs)
	}

	// Once the describe answers again, the stale rows take its new values
	// and are fresh again.
	four := 4
	info := f.types["m7i.large"]
	info.VCPUs = &four
	f.types["m7i.large"] = info
	f.typesErr = nil
	ageAttempts()
	pass()
	if d := describes(); fmt.Sprint(d) != "[describe us-east-1 m7i.large,t4g.small]" {
		t.Fatalf("describes after the failure %v", d)
	}
	if got := catalog(); !strings.Contains(got[0], "m7i.large vcpus=4 ") {
		t.Fatalf("catalog %v, want m7i.large's vcpus updated to 4", got)
	}
	var fresh int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM instance_types WHERE fetched_at > now() - interval '1 minute'`).Scan(&fresh)
	}); err != nil {
		t.Fatal(err)
	}
	if fresh != 2 {
		t.Fatalf("%d rows fetched within the minute, want 2", fresh)
	}
}
