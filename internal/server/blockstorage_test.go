package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// eun1GP3 is AWS's eu-north-1 gp3 list price (Pricing API, October 2026).
var eun1GP3 = BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0836", PerIOPSMonth: "0.0052", PerGiBpsMonth: "42.8032"}

// A volume's hourly price: storage always; gp3's IOPS and throughput only
// above 3000 and 125 MiB/s; io1/io2 every IOPS; gp2 storage only; the
// throughput price is per GiB/s, so 1024 MiB/s cost one unit; a month is
// 730 hours.
func TestVolumeHourly(t *testing.T) {
	io2 := BlockStoragePrice{Currency: "USD", PerGBMonth: "0.125", PerIOPSMonth: "0.065"}
	gp2 := BlockStoragePrice{Currency: "USD", PerGBMonth: "0.1"}
	for _, c := range []struct {
		name  string
		v     HostVolume
		price BlockStoragePrice
		month string // the expected monthly price; hourly is it / 730
	}{
		{"gp3 baseline", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, eun1GP3, "8.36"},
		{"gp3 below baseline", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 1000, ThroughputMiBps: 0}, eun1GP3, "8.36"},
		{"gp3 extra IOPS", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 4000, ThroughputMiBps: 125}, eun1GP3, "13.56"},
		// 1024 MiB/s above the baseline is one GiB/s-month: 42.8032.
		{"gp3 extra throughput", HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 1149}, eun1GP3, "51.1632"},
		{"gp3 both", HostVolume{Type: "gp3", SizeGiB: 10, IOPS: 3500, ThroughputMiBps: 253}, eun1GP3, "8.7864"},
		{"io2 every IOPS", HostVolume{Type: "io2", SizeGiB: 100, IOPS: 1000}, io2, "77.5"},
		{"io1 every IOPS", HostVolume{Type: "io1", SizeGiB: 100, IOPS: 1000}, io2, "77.5"},
		{"gp2 storage only", HostVolume{Type: "gp2", SizeGiB: 100, IOPS: 300}, gp2, "10"},
		{"st1 storage only", HostVolume{Type: "st1", SizeGiB: 500, ThroughputMiBps: 500}, gp2, "50"},
	} {
		got, err := volumeHourly(c.v, c.price)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := new(big.Rat).Quo(mustRat(c.month), big.NewRat(730, 1))
		if got.Cmp(want) != 0 {
			t.Errorf("%s: %s/h, want %s/730 = %s", c.name, got.FloatString(12), c.month, want.FloatString(12))
		}
	}
	// gp3 100 GiB at the baseline in eu-north-1: 8.36 / 730 = 0.011452055 per hour.
	got, _ := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, eun1GP3)
	if moneyString(got) != "0.011452055" {
		t.Errorf("gp3 100GiB: %s", moneyString(got))
	}
}

// What cannot be priced is an error, never zero: an unknown type, or a
// dimension the type bills without a price.
func TestVolumeHourlyRefusesUnpriced(t *testing.T) {
	if _, err := volumeHourly(HostVolume{Type: "magnetic-x", SizeGiB: 1}, eun1GP3); !errors.Is(err, errUnknownVolumeType) {
		t.Errorf("unknown type: %v", err)
	}
	noIOPS := eun1GP3
	noIOPS.PerIOPSMonth = ""
	if _, err := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 1, IOPS: 4000}, noIOPS); err == nil {
		t.Error("gp3 above the IOPS baseline without an IOPS price")
	}
	if _, err := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 1, IOPS: 3000}, noIOPS); err != nil {
		t.Errorf("gp3 at the baseline needs no IOPS price: %v", err)
	}
}

// gp3eun1Answer is the Pricing API's gp3 eu-north-1 answer as ec2.Prices
// returns it (ten fractional digits).
var gp3eun1Answer = BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0836000000", PerIOPSMonth: "0.0052000000", PerGiBpsMonth: "42.8032000000"}

// The price refresh opens one block-storage period per provider host whose
// volumes are known: from its launch request to its end, at the sum of its
// volumes' hourly prices, with the volumes and unit prices it used. Hosts
// with unknown volumes, or a type that cannot be priced, get none (missing,
// not zero); compute periods are untouched; a second refresh changes nothing.
func TestBlockStorageRates(t *testing.T) {
	p := &fakePriceProvider{onDemand: []fakeOnDemandAnswer{{rate: HourlyRate{PerHour: "0.10", Currency: "USD"}}},
		blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "od", MarketOnDemand, from)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at, registered_at,
			instance_type, market, zone, launch_template, capacity, volumes) VALUES
		('unknown', 't1', 'unknown', 'pool-t1-ec2-burst', 'ready', 'i-u', $1, $1, 'm7i.large', 'on-demand', 'us-east-1a', '{"region":"us-east-1"}', '{"cpus":4}', NULL),
		('weird', 't1', 'weird', 'pool-t1-ec2-burst', 'ready', 'i-w', $1, $1, 'm7i.large', 'on-demand', 'us-east-1a', '{"region":"us-east-1"}', '{"cpus":4}',
			'[{"type":"floppy","sizeGiB":1}]')`, from)
	execSQL(t, s, ctx, `UPDATE hosts SET volumes = '[{"type":"gp3","sizeGiB":100,"iops":3000,"throughputMiBps":125},{"type":"gp3","sizeGiB":50,"iops":4000,"throughputMiBps":125}]' WHERE id = 'od'`)

	s.refreshPrices(ctx)
	s.refreshBlockStorage(ctx)
	type rate struct {
		Family, PerHour, Source string
		From                    time.Time
		Open                    bool
		CPUs                    float64
		Details                 *string
	}
	rates := func(host string) []rate {
		var out []rate
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT family, trim_scale(per_hour)::text, source, valid_from, valid_to IS NULL, cap_cpus, details::text
				FROM host_rates WHERE host_id = $1 ORDER BY family, valid_from`, host)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[rate])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := rates("od")
	// 100 GiB × 0.0836 + 50 GiB × 0.0836 + 1000 IOPS × 0.0052 = 17.74 a month; ÷ 730.
	if len(got) != 2 || got[0].Family != "block-storage" || got[0].PerHour != "0.02430137" || !got[0].From.Equal(from) ||
		!got[0].Open || got[0].Source != "ec2-ebs-pricing" || got[0].CPUs != 4 || got[1].Family != "compute" || got[1].PerHour != "0.1" {
		t.Fatalf("rates: %+v", got)
	}
	var d struct {
		Volumes []HostVolume
		Prices  map[string]map[string]string
	}
	if err := json.Unmarshal([]byte(*got[0].Details), &d); err != nil || len(d.Volumes) != 2 || d.Prices["gp3"]["perGiBpsMonth"] != "42.8032" {
		t.Errorf("details %s (%v)", *got[0].Details, err)
	}
	if r := rates("unknown"); len(r) != 1 || r[0].Family != "compute" {
		t.Errorf("unknown volumes priced: %+v", r)
	}
	if r := rates("weird"); len(r) != 1 || r[0].Family != "compute" {
		t.Errorf("unpriceable type priced: %+v", r)
	}
	s.refreshPrices(ctx)
	s.refreshBlockStorage(ctx)
	if again := rates("od"); len(again) != 2 || again[0].PerHour != got[0].PerHour || !again[0].From.Equal(got[0].From) || *again[0].Details != *got[0].Details {
		t.Errorf("second refresh: %+v", again)
	}
}

// A launch that never registered (no capacity, no compute period) is not
// selected by the block-storage refresh, so it is not priced or locked on
// every pass; once it registers it is. Capacity may come from the host row
// or from its compute period.
func TestBlockStorageHostsSkipNeverRegistered(t *testing.T) {
	p := &fakePriceProvider{blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	insertProviderHost(t, s, "t1", "burst", "ec2", "od", MarketOnDemand, from)
	execSQL(t, s, ctx, `UPDATE hosts SET volumes = $1::jsonb WHERE id = 'od'`, gp3Root)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at, terminated_at, launch_template, volumes) VALUES
		('ghost', 't1', 'ghost', 'pool-t1-ec2-burst', 'terminated', 'i-g', $1, $2, '{"region":"us-east-1"}', $3::jsonb),
		('booting', 't1', 'booting', 'pool-t1-ec2-burst', 'provisioning', 'i-b', $1, NULL, '{"region":"us-east-1"}', $3::jsonb)`,
		from, from.Add(10*time.Minute), gp3Root)
	// Capacity known only from its compute period, as for a host whose
	// capacity column was never filled.
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provider_id, provision_requested_at, launch_template, capacity, volumes) VALUES
		('rated', 't1', 'rated', 'pool-t1-ec2-burst', 'ready', 'i-r', $1, '{"region":"us-east-1"}', '{}', $2::jsonb)`, from, gp3Root)
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('rated', $1, 0.10, 'USD', 8, 0, 'test')`, from)
	ids := func() []string {
		hosts, err := s.blockStorageHosts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hosts {
			out = append(out, h.ID)
		}
		return out
	}
	if got := fmt.Sprint(ids()); got != "[od rated]" {
		t.Fatalf("selected %s, want [od rated]", got)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = '{"cpus": 4}', registered_at = $1, state = 'ready' WHERE id = 'booting'`, from.Add(time.Minute))
	if got := fmt.Sprint(ids()); got != "[booting od rated]" {
		t.Fatalf("after registering: %s", got)
	}
}

// The worked example's host with its 100 GiB gp3 root disk (eu-north-1 list
// price, 0.011452055/h after rounding to the period's 9 digits): placements
// pay the same share of the disk as of the machine, scaled by 1/max(1, S),
// and in every piece allocated + unallocated is exactly the disk's cost.
func TestComputeCostBlockStorage(t *testing.T) {
	hourly, _ := volumeHourly(HostVolume{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, eun1GP3)
	disk := usdRate("10:00", "", moneyString(hourly))
	disk.Source = "ec2-ebs-pricing"
	quarter := new(big.Rat).Quo(mustRat(disk.PerHour), big.NewRat(4, 1)) // one 15-minute piece
	for _, c := range []struct {
		name       string
		placements []placementWindow
		// Each Run's share of the disk-hour (sum of its pieces' share/max(1,S)
		// × 1/4), and the unallocated share.
		share   map[string]*big.Rat
		unalloc *big.Rat
	}{{
		name:       "worked example",
		placements: workedExample,
		// A 0.25+0.25, B 0.5×3, C 0.5 (quarters); unallocated 0.75+0.25+0+0.5.
		share:   map[string]*big.Rat{"A": big.NewRat(2, 16), "B": big.NewRat(6, 16), "C": big.NewRat(2, 16)},
		unalloc: big.NewRat(6, 16),
	}, {
		name:       "S > 1",
		placements: append(append([]placementWindow{}, workedExample...), place("D", 2, 4, "10:30", "10:45")),
		// 10:30–10:45: B, C, D pay 0.5/1.25, 0.5/1.25, 0.25/1.25 of the quarter.
		share: map[string]*big.Rat{"A": big.NewRat(2, 16), "B": new(big.Rat).Add(big.NewRat(4, 16), big.NewRat(1, 10)),
			"C": big.NewRat(1, 10), "D": big.NewRat(1, 20)},
		unalloc: big.NewRat(6, 16),
	}} {
		t.Run(c.name, func(t *testing.T) {
			res, err := computeCost(hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{disk}, Placements: c.placements})
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range c.placements {
				want := new(big.Rat).Mul(mustRat(disk.PerHour), c.share[p.RunID])
				if got := res.Placements[i].Amounts["USD"]; got == nil || got.Cmp(want) != 0 {
					t.Errorf("%s: %v, want %s", p.RunID, got, want.FloatString(12))
				}
			}
			if want := new(big.Rat).Mul(mustRat(disk.PerHour), c.unalloc); res.Unallocated["USD"].Cmp(want) != 0 {
				t.Errorf("unallocated %s, want %s", res.Unallocated["USD"].FloatString(12), want.FloatString(12))
			}
			for _, piece := range res.Pieces {
				sum := new(big.Rat).Set(piece.Unallocated)
				for _, v := range piece.Charged {
					sum.Add(sum, v)
				}
				if sum.Cmp(piece.Host) != 0 || piece.Host.Cmp(quarter) != 0 {
					t.Errorf("piece %s: allocated + unallocated %s, disk %s", piece.From.Format("15:04"), sum.FloatString(12), piece.Host.FloatString(12))
				}
			}
		})
	}
}

// Unit prices are fetched once per (region, type) and cached in price_cache
// for costs.prices_refresh; a failed refresh keeps the stale cache; with
// nothing cached a failure is an error.
func TestBlockStoragePriceCache(t *testing.T) {
	p := &fakePriceProvider{blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3")
	if err != nil || got != eun1GP3 {
		t.Fatalf("first: %+v %v", got, err)
	}
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != eun1GP3 || len(p.blockStorageCalls) != 1 {
		t.Fatalf("cached: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	var rows int
	systemScan(t, s, `SELECT count(*) FROM price_cache WHERE os = 'EBS'`, nil, &rows)
	if rows != 3 {
		t.Errorf("%d cache rows, want 3", rows)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET fetched_at = now() - interval '2 hours'`)
	p.blockStorageErr = errors.New("throttled")
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != eun1GP3 || len(p.blockStorageCalls) != 2 {
		t.Errorf("stale after failure: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	if _, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "io2"); err == nil {
		t.Error("nothing cached and the provider failing: no error")
	}
}

// A refresh of a stale type overwrites the cached rows in place, and drops
// a dimension the new answer no longer has.
func TestBlockStoragePriceCacheRefreshReplaces(t *testing.T) {
	p := &fakePriceProvider{blockStorage: map[string]BlockStoragePrice{"gp3": gp3eun1Answer}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != eun1GP3 {
		t.Fatalf("first: %+v %v", got, err)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET fetched_at = now() - interval '2 hours'`)
	p.blockStorage["gp3"] = BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0900000000", PerIOPSMonth: "0.0052000000"}
	want := BlockStoragePrice{Currency: "USD", PerGBMonth: "0.09", PerIOPSMonth: "0.0052"}
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != want || len(p.blockStorageCalls) != 2 {
		t.Fatalf("refresh: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	// The refresh stamped fetched_at: the next read is the cache's, not another call.
	if got, err := s.blockStoragePrice(ctx, "ec2", "eu-north-1", "gp3"); err != nil || got != want || len(p.blockStorageCalls) != 2 {
		t.Fatalf("after refresh: %+v %v, calls %v", got, err, p.blockStorageCalls)
	}
	var rows []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT split_part(instance_type, ':', 2) || '=' || trim_scale(per_hour)::text
			FROM price_cache WHERE os = 'EBS' ORDER BY instance_type COLLATE "C"`)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows); got != "[GB-Mo=0.09 IOPS-Mo=0.0052]" {
		t.Errorf("cache rows %s, want [GB-Mo=0.09 IOPS-Mo=0.0052]", got)
	}
}
