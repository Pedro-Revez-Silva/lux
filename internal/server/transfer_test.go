package server

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// transferHost adds t1's provider host "nh" in eu-north-1 (pool "ec2p" of
// provider ec2), billed from 10:00 at $0.40/h with no volumes, and its
// network (partial or not) with addresses public IPv4 addresses.
func transferHost(t *testing.T, s *Server, partial bool, addresses ...int) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	n := 0
	if len(addresses) > 0 {
		n = addresses[0]
	}
	network := fmt.Sprintf(`{"subnetCidrs":["10.0.0.0/24"],"zone":"eu-north-1a","vpcSubnets":[],"publicIPv4":%d}`, n)
	if partial {
		network = fmt.Sprintf(`{"partial":true,"subnetCidrs":[],"zone":"eu-north-1a","vpcSubnets":[],"publicIPv4":%d}`, n)
	}
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, instance_type, market, zone,
			launch_template, volumes, network)
		VALUES ('nh', 't1', 'nh', 'ec2p', 'ready', $1, $1, 'm7i.2xlarge', 'on-demand', 'eu-north-1a', '{"region":"eu-north-1"}', '[]', $2)`, at("10:00"), network)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('nh', $1, 0.40, 'USD', 8, $2, 'ec2-pricing')`, at("10:00"), 32*gib)
}

// cacheTransfer stores eu-north-1's prices as the price loop would.
func cacheTransfer(t *testing.T, s *Server) {
	t.Helper()
	p := &fakePriceProvider{transfer: map[string]DataTransferPrices{"eu-north-1": eun1Transfer()}}
	s.cfg.Costs.Prices = map[string]PriceProvider{"ec2": p}
	s.cfg.Costs.PricesRefresh = time.Hour
	if _, err := s.dataTransferPrice(context.Background(), "ec2", "eu-north-1"); err != nil {
		t.Fatal(err)
	}
}

func setNet(t *testing.T, s *Server, placement string, net map[string]proto.NetBytes) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE placements SET net_classes = $2 WHERE id = $1`, placement, net)
}

// transferLinesOf is a Run's data-transfer lines as "item amount final".
func transferLinesOf(t *testing.T, s *Server, key, runID string) ([]string, RunCost) {
	t.Helper()
	code, c := getCost(t, s, key, runID)
	if code != http.StatusOK {
		t.Fatalf("GET cost: %d", code)
	}
	var out []string
	for _, l := range c.Lines {
		if l.Family == familyDataTransfer {
			out = append(out, fmt.Sprintf("%s %s %v", l.Item, l.Amount, l.Final))
		}
	}
	return out, c
}

const gb = 1_000_000_000

// Each billable class and direction is bytes × its per-GB price, GB being
// 10^9 bytes; inbound is priced by what was delivered (rxDelivered), not
// what the filters accepted. Free classes (local, region-service,
// provider-global-free) produce no line; provider-global bills as internet.
func TestTransferSnapshotPerClass(t *testing.T) {
	s, keys := costFixture(t)
	transferHost(t, s, false)
	cacheTransfer(t, s)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	setNet(t, s, "p-A", map[string]proto.NetBytes{
		"internet":             {Tx: 3 * gb, Rx: 1 * gb, RxDelivered: 2 * gb}, // inbound from the internet is free
		"zone":                 {Tx: gb / 2, Rx: gb / 4, RxDelivered: gb},     // both ways at 0.01
		"region-public":        {Tx: 2 * gb},
		"provider:eu-west-1":   {Tx: 5 * gb, Rx: 9 * gb}, // inter-region inbound is free
		"provider-global":      {Tx: gb},                 // Route 53 et al.: internet out
		"provider-global-free": {Tx: 7 * gb},             // CloudFront
		"local":                {Tx: 100 * gb, Rx: 100 * gb},
		"region-service":       {Tx: 40 * gb},
	})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	want := []string{
		"internet:tx 0.27 true",          // 3 GB × 0.09
		"provider-global:tx 0.09 true",   // 1 GB × 0.09
		"provider:eu-west-1:tx 0.1 true", // 5 GB × 0.02
		"region-public:tx 0.02 true",     // 2 GB × 0.01
		"zone:rx 0.01 true",              // rxDelivered 1 GB × 0.01, not rx's 0.25 GB
		"zone:tx 0.005 true",             // 0.5 GB × 0.01
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %v\nwant  %v", lines, want)
	}
	if len(c.Unpriced) != 0 {
		t.Errorf("unpriced %v", c.Unpriced)
	}
	for _, l := range c.Lines {
		if l.Item == "zone:rx" {
			d := l.Details
			if d["class"] != "zone" || d["direction"] != "rx" || d["bytes"] != float64(gb) || d["perGB"] != "0.01" || d["currency"] != "USD" {
				t.Errorf("zone:rx details %v", d)
			}
		}
	}
	var total string
	for _, f := range c.ByFamily {
		if f.Family == familyDataTransfer {
			total = f.Amount + " " + f.DisplayName
		}
	}
	if total != "0.495 Data transfer" {
		t.Errorf("byFamily data-transfer %q", total)
	}
	var src string
	systemScan(t, s, `SELECT status FROM cost_sources WHERE run_id = 'A' AND source = 'compute'`, nil, &src)
	if src != "final" {
		t.Errorf("source %s", src)
	}
}

// unknown bytes produce no line and are reported as unpriced
// (unclassified); a host whose discovery was partial prices nothing and
// says why. Neither holds up finality: there is nothing a retry could
// price.
func TestTransferUnpriced(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			s, keys := costFixture(t)
			transferHost(t, s, partial)
			cacheTransfer(t, s)
			placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
			execSQL(t, s, context.Background(), `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, net_classes) VALUES ('A', 1, 't1', 0, $1, $2)`,
				at("10:20"), map[string]proto.NetBytes{"internet": {Tx: gb / 2}})
			setNet(t, s, "p-A", map[string]proto.NetBytes{"unknown": {Tx: 4 * gb, Rx: 1, RxDelivered: 6}, "internet": {Tx: gb}, "local": {Tx: gb}})
			finish(t, s, "t1", "A", StateSucceeded)
			drain(t, s)
			lines, c := transferLinesOf(t, s, keys["t1"], "A")
			var unpriced []string
			for _, u := range c.Unpriced {
				unpriced = append(unpriced, fmt.Sprintf("%s %s rx%d tx%d", u.Class, u.Reason, u.Rx, u.Tx))
			}
			wantLines, wantUnpriced := []string{"internet:tx 0.09 true"}, []string{"unknown unclassified rx6 tx4000000000"}
			if partial {
				wantLines = nil
				wantUnpriced = []string{"internet partial-network rx0 tx1000000000", "unknown partial-network rx6 tx4000000000"}
			}
			if !slices.Equal(lines, wantLines) || !slices.Equal(unpriced, wantUnpriced) {
				t.Errorf("lines %v unpriced %v; want %v %v", lines, unpriced, wantLines, wantUnpriced)
			}
			for _, src := range c.Sources {
				if src.Source == "compute" && src.Status != "final" {
					t.Errorf("source %s", src.Status)
				}
			}
			// The hourly rows and the summary agree with the lines: the
			// internet bytes in their hours, or nothing on a partial host.
			var hours string
			systemScan(t, s, `SELECT coalesce(string_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || '=' || trim_scale(amount)::text, ' ' ORDER BY hour), '')
				FROM cost_hourly WHERE run_id = 'A' AND family = 'data-transfer'`, nil, &hours)
			var summary CostSummaryBody
			path := "/v1/costs?from=" + url.QueryEscape(at("09:00").Format(time.RFC3339)) + "&to=" + url.QueryEscape(at("13:00").Format(time.RFC3339)) +
				"&family=data-transfer&interval=hour"
			if code := getJSON(t, s, keys["t1"], path, &summary); code != http.StatusOK {
				t.Fatalf("GET /v1/costs: %d", code)
			}
			var totals []string
			for _, r := range summary.Totals {
				totals = append(totals, r.Amount)
			}
			wantHours, wantTotals, wantSeries := "10=0.045 11=0.045", []string{"0.09"}, 2
			if partial {
				wantHours, wantTotals, wantSeries = "", nil, 0
			}
			if hours != wantHours || !slices.Equal(totals, wantTotals) || len(summary.Series) != wantSeries {
				t.Errorf("cost_hourly %q totals %v series %v; want %q %v %d series", hours, totals, summary.Series, wantHours, wantTotals, wantSeries)
			}
		})
	}
}

// A live placement's partial treatment follows its host's network at each
// evaluation: priced while the host is not partial, then, once the host's
// discovery turns partial, its line becomes unpriced (partial-network) and
// its hourly rows are removed with it, so lines and hours agree.
func TestTransferPartialFlipWhileLive(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false)
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	placeRun(t, s, "t1", "A", StateRunning, "nh", place("A", 2, 8, "10:00", ""))
	insertNetSamples(t, s, "A", []netSampleRow{{"10:20", 0, gb}})
	setNet(t, s, "p-A", map[string]proto.NetBytes{"internet": {Tx: gb}})
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('A', 'retry')`)
	drain(t, s)
	hours := func() string {
		t.Helper()
		var h string
		systemScan(t, s, `SELECT coalesce(string_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || '=' || trim_scale(amount)::text, ' ' ORDER BY hour), '')
			FROM cost_hourly WHERE run_id = 'A' AND family = 'data-transfer'`, nil, &h)
		return h
	}
	if lines, c := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.09 false"}) || len(c.Unpriced) != 0 || hours() != "10=0.09" {
		t.Fatalf("live, not partial: lines %v unpriced %v hours %q", lines, c.Unpriced, hours())
	}
	execSQL(t, s, ctx, `UPDATE hosts SET network = network || '{"partial":true}' WHERE id = 'nh'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('A', 'retry')`)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	var unpriced []string
	for _, u := range c.Unpriced {
		unpriced = append(unpriced, fmt.Sprintf("%s %s tx%d", u.Class, u.Reason, u.Tx))
	}
	if len(lines) != 0 || !slices.Equal(unpriced, []string{"internet partial-network tx1000000000"}) || hours() != "" {
		t.Errorf("after the flip: lines %v unpriced %v hours %q; want none, internet partial-network, none", lines, unpriced, hours())
	}
}

// A provider host's inter-region destination the price list lacks is
// unpriced (no-price), not billed at zero; the class is final all the same.
func TestTransferProviderDestinationWithoutPrice(t *testing.T) {
	s, keys := costFixture(t)
	transferHost(t, s, false)
	cacheTransfer(t, s)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	setNet(t, s, "p-A", map[string]proto.NetBytes{"provider:eu-west-1": {Tx: gb}, "provider:ap-east-9": {Tx: 2 * gb}})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	if !slices.Equal(lines, []string{"provider:eu-west-1:tx 0.02 true"}) || len(c.Unpriced) != 1 ||
		fmt.Sprintf("%+v", c.Unpriced[0]) != fmt.Sprintf("%+v", Unpriced{Class: "provider:ap-east-9", Reason: unpricedNoPrice, Tx: 2 * gb}) {
		t.Errorf("lines %v unpriced %+v", lines, c.Unpriced)
	}
}

// No price cached for the host's region: billable bytes are missing, the
// source incomplete and retried, never zero; once the price is cached the
// retry prices and freezes it, and a later price change does not reprice.
func TestTransferMissingThenFinal(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	setNet(t, s, "p-A", map[string]proto.NetBytes{"internet": {Tx: 2 * gb}})
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, c := transferLinesOf(t, s, keys["t1"], "A")
	if len(lines) != 0 || len(c.Unpriced) != 1 || c.Unpriced[0].Reason != unpricedNoPrice {
		t.Fatalf("no price: lines %v unpriced %v", lines, c.Unpriced)
	}
	var src, lastError string
	systemScan(t, s, `SELECT status, last_error FROM cost_sources WHERE run_id = 'A'`, nil, &src, &lastError)
	if src != "incomplete" || !strings.Contains(lastError, "no data-transfer price") {
		t.Fatalf("source %s %q", src, lastError)
	}
	cacheTransfer(t, s)
	execSQL(t, s, ctx, `UPDATE cost_sources SET next_at = now() - interval '1 second' WHERE run_id = 'A'`)
	if err := s.pollDueCostSources(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if lines, c := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.18 true"}) || len(c.Unpriced) != 0 {
		t.Fatalf("priced: %v %v", lines, c.Unpriced)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET per_hour = 9 WHERE os = 'TRANSFER'`)
	execSQL(t, s, ctx, `SELECT lux_cost_enqueue('A', 'retry')`)
	drain(t, s)
	if lines, _ := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.18 true"}) {
		t.Errorf("frozen snapshot repriced: %v", lines)
	}
}

// The placement's transfer goes to the hours its bytes moved in (sample
// deltas), not spread evenly over its window nor all in its last hour; the
// hours add up to the line. Within an hour only its last reading counts:
// an earlier one, or a coarser rollup of the hour, adds nothing. Hours are
// UTC hours whatever the session's time zone, including a half-hour offset.
func TestTransferHourlyFromSamples(t *testing.T) {
	for _, tz := range append(slices.Clone(dailyTimeZones), "Asia/Kolkata") {
		t.Run(tz, func(t *testing.T) {
			s, keys := costFixture(t)
			sessionTimeZone(t, s, tz)
			transferHost(t, s, false)
			cacheTransfer(t, s)
			s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
			placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "13:30"))
			// 1 GB in the 10:00 hour, 1 GB in 11:00, 2 GB in 12:00, 1 GB
			// unsampled before the end (13:30). 10:50 and 11:20 share a
			// Kolkata (+5:30) hour but not a UTC one.
			// The 12:00 hour's readings are 2 GB then 4 GB, and its minute
			// rollup at 12:00 repeats 2 GB: only the later 4 GB reading sets
			// the hour.
			insertNetSamples(t, s, "A", []netSampleRow{{"10:20", 0, gb}, {"10:50", 0, gb}, {"11:20", 0, 2 * gb},
				{"12:10", 0, 2 * gb}, {"12:00", 60, 2 * gb}, {"12:40", 0, 4 * gb}})
			if read, want := readNetSamples(t, s, "A"), []string{"10:50=1", "11:20=2", "12:40=4"}; !slices.Equal(read, want) {
				t.Errorf("samples read %v, want each hour's last %v", read, want)
			}
			setNet(t, s, "p-A", map[string]proto.NetBytes{"internet": {Tx: 5 * gb}})
			finish(t, s, "t1", "A", StateSucceeded)
			drain(t, s)
			if lines, _ := transferLinesOf(t, s, keys["t1"], "A"); !slices.Equal(lines, []string{"internet:tx 0.45 true"}) {
				t.Fatalf("line %v", lines)
			}
			var hours string
			systemScan(t, s, `SELECT string_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || '=' || trim_scale(amount)::text, ' ' ORDER BY hour)
				FROM cost_hourly WHERE run_id = 'A' AND family = 'data-transfer'`, nil, &hours)
			if hours != "10=0.09 11=0.09 12=0.18 13=0.09" {
				t.Errorf("hours %s", hours)
			}
		})
	}
}

// A rolled-up hour is read from its hour rollup alone: raw or minute rows
// of that hour still in retention are not read again, and the hours after
// the newest rollup come from raw and minute rows.
func TestTransferSamplesFromHourRollups(t *testing.T) {
	s, _ := costFixture(t)
	insertNetSamples(t, s, "A", []netSampleRow{{"09:00", 3600, gb}, {"09:10", 0, gb / 2}, {"10:00", 3600, 2 * gb},
		{"10:20", 0, gb}, {"10:50", 60, 2 * gb}, {"11:10", 0, 3 * gb}, {"11:40", 0, 4 * gb}})
	if read, want := readNetSamples(t, s, "A"), []string{"09:00=1", "10:00=2", "11:40=4"}; !slices.Equal(read, want) {
		t.Errorf("samples read %v, want %v", read, want)
	}
}

type netSampleRow struct {
	at  string
	res int
	tx  int64
}

func insertNetSamples(t *testing.T, s *Server, runID string, rows []netSampleRow) {
	t.Helper()
	for _, r := range rows {
		execSQL(t, s, context.Background(), `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, net_classes) VALUES ($1, 1, 't1', $2, $3, $4)`,
			runID, r.res, at(r.at), map[string]proto.NetBytes{"internet": {Tx: r.tx}})
	}
}

// readNetSamples is placementNetSamples for the Run's epoch 1 as
// "HH:MM=GB" in UTC.
func readNetSamples(t *testing.T, s *Server, runID string) []string {
	t.Helper()
	ctx := context.Background()
	var samples []netSample
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) {
		samples, err = placementNetSamples(ctx, tx, runID, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var read []string
	for _, smp := range samples {
		read = append(read, fmt.Sprintf("%s=%d", smp.At.UTC().Format("15:04"), smp.Net["internet"].Tx/gb))
	}
	return read
}

// transferHours' rounding keeps the hours' sum equal to the rounded total.
func TestTransferHoursSumToTotal(t *testing.T) {
	p := eun1TransferTrimmed()
	var samples []netSample
	for i := range 7 {
		samples = append(samples, netSample{At: at("10:00").Add(time.Duration(i) * 50 * time.Minute),
			Net: map[string]proto.NetBytes{"zone": {Tx: int64(i+1) * 333_333_333, RxDelivered: int64(i) * 111_111_111}}})
	}
	final := map[string]proto.NetBytes{"zone": {Tx: 7*333_333_333 + 5, RxDelivered: 6*111_111_111 + 9}}
	hours := transferHours(samples, final, at("16:00"), &p, false)
	sum := new(big.Rat)
	for _, h := range hours {
		sum.Add(sum, mustRat(h.Amount))
	}
	_, _, total := priceTransfer(final, &p, false)
	if moneyString(sum) != moneyString(total) {
		t.Errorf("hours sum %s, total %s (%v)", moneyString(sum), moneyString(total), hours)
	}
}

// CloudFront (provider-global-free) is a free class: no line, never
// unpriced, whatever the price list.
func TestTransferClassFreeCloudFront(t *testing.T) {
	if !transferClassFree(string(proto.NetProviderGlobalFree)) {
		t.Error("provider-global-free is not a free class")
	}
	if transferClassFree(string(proto.NetProviderGlobal)) {
		t.Error("provider-global is a free class")
	}
}

// Priced on its own, provider-global-free costs 0 per GB in both
// directions, while provider-global costs internet out.
func TestTransferPerGBCloudFront(t *testing.T) {
	p := eun1TransferTrimmed()
	for _, dir := range []string{"rx", "tx"} {
		if v, ok := transferPerGB(p, string(proto.NetProviderGlobalFree), dir); !ok || v != "0" {
			t.Errorf("provider-global-free %s: %q %v, want \"0\" true", dir, v, ok)
		}
	}
	if v, ok := transferPerGB(p, string(proto.NetProviderGlobal), "tx"); !ok || v != "0.09" {
		t.Errorf("provider-global tx: %q %v, want 0.09", v, ok)
	}
}
