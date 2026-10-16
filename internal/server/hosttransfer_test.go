package server

import (
	"context"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Heartbeats sample the host's own counters and a placement's only when
// they changed since the last sample: unchanged ones leave the column
// NULL, so a reading the runner repeats adds no bytes twice.
func TestHeartbeatSamplesNetOnChange(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	beat := func(host, run map[string]proto.NetBytes) {
		t.Helper()
		if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Usage: &proto.HostUsage{Net: host},
			Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: &proto.Usage{Net: run}}}}); err != nil {
			t.Fatal(err)
		}
	}
	beat(map[string]proto.NetBytes{"internet": {Tx: 10}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	beat(map[string]proto.NetBytes{"internet": {Tx: 10}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	beat(map[string]proto.NetBytes{"internet": {Tx: 30}}, map[string]proto.NetBytes{"zone": {Tx: 1}})
	var hosts, placements []*string
	systemScan(t, s, `SELECT array_agg(net_classes->'internet'->>'tx' ORDER BY at) FROM host_samples WHERE host_id = 'h1' AND res = 0`, nil, &hosts)
	systemScan(t, s, `SELECT array_agg(net_classes->'zone'->>'tx' ORDER BY at) FROM placement_samples WHERE run_id = $1 AND res = 0`, []any{r1}, &placements)
	str := func(v []*string) []string {
		out := make([]string, len(v))
		for i, p := range v {
			out[i] = "-"
			if p != nil {
				out[i] = *p
			}
		}
		return out
	}
	if got := str(hosts); len(got) != 3 || got[0] != "10" || got[1] != "-" || got[2] != "30" {
		t.Errorf("host samples %v, want [10 - 30]", got)
	}
	if got := str(placements); len(got) != 3 || got[0] != "1" || got[1] != "-" || got[2] != "-" {
		t.Errorf("placement samples %v, want [1 - -]", got)
	}
}

// The host's own transfer becomes unallocated data-transfer rows, each
// hour its counters' growth in that hour at the host's prices. The host
// registered in its first hour, so that hour measures from zero (counters
// start at boot) and the rows add up to its cumulative counters.
func TestHostTransferHours(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	cacheTransfer(t, s)
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at,
			capacity, volumes, launch_template, network)
		VALUES ('ht', 't1', 'ht', 'ec2p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', '{"region":"eu-north-1"}', '{"publicIPv4":0}')`,
		hour, hour.Add(4*time.Hour))
	// Cumulative: hour 0 reaches 2 GB zone tx; hour 1 reaches 3 GB; hour 2
	// nothing new; hour 3 reaches 5 GB zone tx and 1 GB internet tx, 3 GB
	// delivered from zone. Hour 1's last reading is also in a minute
	// rollup: the same counters at another resolution add nothing.
	for _, smp := range []struct {
		min, res int
		net      map[string]proto.NetBytes
	}{
		{10, 0, map[string]proto.NetBytes{"zone": {Tx: gb}}},
		{50, 0, map[string]proto.NetBytes{"zone": {Tx: 2 * gb}}},
		{110, 0, map[string]proto.NetBytes{"zone": {Tx: 3 * gb}}},
		{110, 60, map[string]proto.NetBytes{"zone": {Tx: 3 * gb}}},
		{190, 0, map[string]proto.NetBytes{"zone": {Tx: 4 * gb}, "internet": {Tx: gb}}},
		{230, 0, map[string]proto.NetBytes{"zone": {Tx: 5 * gb, Rx: gb, RxDelivered: 3 * gb}, "internet": {Tx: gb}, "local": {Tx: 50 * gb}}},
	} {
		execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, net_classes) VALUES ('ht', $1, $2, $3)`,
			smp.res, hour.Add(time.Duration(smp.min)*time.Minute), smp.net)
	}
	for range 5 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Hour 0: 2 GB zone tx × 0.01. Hour 1: 1 GB zone tx × 0.01. Hour 3:
	// 2 GB zone tx × 0.01 + 3 GB zone delivered × 0.01 + 1 GB internet ×
	// 0.09. Local is free.
	want := []string{"0=0.02/0", "1=0.01/0", "3=0.14/0"}
	if rows := hostTransferRows(t, s, "ht", hour); !slices.Equal(rows, want) {
		t.Fatalf("rows %v, want %v", rows, want)
	}
	p := eun1TransferTrimmed()
	_, _, total := priceTransfer(map[string]proto.NetBytes{"zone": {Tx: 5 * gb, Rx: gb, RxDelivered: 3 * gb}, "internet": {Tx: gb}, "local": {Tx: 50 * gb}}, &p, false)
	if sum := new(big.Rat).Add(new(big.Rat).Add(mustRat("0.02"), mustRat("0.01")), mustRat("0.14")); sum.Cmp(total) != 0 {
		t.Errorf("hours sum %s, cumulative priced %s", sum.FloatString(9), total.FloatString(9))
	}
}

// A new host's first hour is its image pulls: registered a few minutes
// into the hour, its counters started at boot, so the first sample's
// cumulative is that hour's growth.
func TestHostTransferFirstHourOfNewHost(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	cacheTransfer(t, s)
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at,
			capacity, volumes, launch_template, network)
		VALUES ('hn', 't1', 'hn', 'ec2p', 'terminated', $1, $2, $3, '{"cpus":8,"memory":34359738368}', '[]', '{"region":"eu-north-1"}', '{"publicIPv4":0}')`,
		hour, hour.Add(5*time.Minute), hour.Add(2*time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, net_classes) VALUES
		('hn', 0, $1, '{"internet":{"tx":5000000000}}'), ('hn', 0, $2, '{"internet":{"tx":5200000000}}')`,
		hour.Add(20*time.Minute), hour.Add(80*time.Minute))
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Hour 0: 5 GB × 0.09. Hour 1: 0.2 GB × 0.09.
	if rows, want := hostTransferRows(t, s, "hn", hour), []string{"0=0.45/0", "1=0.018/0"}; !slices.Equal(rows, want) {
		t.Errorf("rows %v, want %v", rows, want)
	}
}

// A host live when luxd started sampling its counters (an upgrade): its
// first sample already holds everything since boot. That hour gets no row,
// not the whole cumulative; the next hour is its growth only.
func TestHostTransferFirstSampleAfterUpgrade(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	cacheTransfer(t, s)
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at,
			capacity, volumes, launch_template, network)
		VALUES ('hu', 't1', 'hu', 'ec2p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', '{"region":"eu-north-1"}', '{"publicIPv4":0}')`,
		hour.Add(-30*24*time.Hour), hour.Add(2*time.Hour))
	execSQL(t, s, ctx, `INSERT INTO cost_host_refresh (host_id, next_hour) VALUES ('hu', $1)`, hour)
	execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, net_classes) VALUES
		('hu', 0, $1, '{"internet":{"tx":5000000000}}'), ('hu', 0, $2, '{"internet":{"tx":5200000000}}')`,
		hour.Add(20*time.Minute), hour.Add(80*time.Minute))
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Hour 1: 0.2 GB × 0.09.
	if rows, want := hostTransferRows(t, s, "hu", hour), []string{"1=0.018/0"}; !slices.Equal(rows, want) {
		t.Errorf("rows %v, want %v", rows, want)
	}
}

// hostTransferRows: host's data-transfer rows as "<hours after since>=unallocated/allocated".
func hostTransferRows(t *testing.T, s *Server, host string, since time.Time) []string {
	t.Helper()
	var rows []string
	systemScan(t, s, `SELECT coalesce(array_agg(extract(hour FROM hour - $2::timestamptz)::text || '=' || trim_scale(unallocated)::text || '/' || trim_scale(allocated)::text ORDER BY hour), '{}')
		FROM cost_hourly WHERE host_id = $1 AND run_id IS NULL AND family = 'data-transfer'`, []any{host, since}, &rows)
	return rows
}

// A host whose network discovery is partial prices none of its own
// transfer, as its placements' (priceTransfer's partial): no rows.
func TestHostTransferPartialHost(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	cacheTransfer(t, s)
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at,
			capacity, volumes, launch_template, network)
		VALUES ('hp', 't1', 'hp', 'ec2p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', '{"region":"eu-north-1"}', '{"partial":true,"publicIPv4":0}')`,
		hour, hour.Add(2*time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, net_classes) VALUES
		('hp', 0, $1, '{"internet":{"tx":5000000000}}'), ('hp', 0, $2, '{"internet":{"tx":5200000000}}')`,
		hour.Add(20*time.Minute), hour.Add(80*time.Minute))
	for range 3 {
		if err := s.updateHostHours(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if rows := hostTransferRows(t, s, "hp", hour); len(rows) != 0 {
		t.Errorf("partial host rows %v, want none", rows)
	}
}
