package server

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

type storedPublicIP struct {
	From      time.Time
	To        *time.Time
	PerHour   string
	Addresses int
}

func publicIPRates(t *testing.T, s *Server, host string) []storedPublicIP {
	t.Helper()
	ctx := context.Background()
	var out []storedPublicIP
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT valid_from, valid_to, trim_scale(per_hour)::text, coalesce((details->>'addresses')::int, 0)
			FROM host_rates WHERE host_id = $1 AND family = 'public-ip' ORDER BY valid_from`, host)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[storedPublicIP])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A provider host's public-ip period opens from its registration once its
// addresses are known and its region's price is cached; a provider check
// that records another count closes it and opens one at the new count;
// zero addresses close it with none after.
func TestPublicIPPeriods(t *testing.T) {
	s, _ := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 1) // 1 address, registered 10:00
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	s.refreshPublicIP(ctx) // no price cached and none to fetch: nothing
	if got := publicIPRates(t, s, "nh"); len(got) != 0 {
		t.Fatalf("opened without a price: %v", got)
	}
	cacheTransfer(t, s)
	s.refreshPublicIP(ctx)
	got := publicIPRates(t, s, "nh")
	if len(got) != 1 || !got[0].From.Equal(at("10:00")) || got[0].To != nil || got[0].PerHour != "0.005" || got[0].Addresses != 1 {
		t.Fatalf("first period %+v", got)
	}
	s.refreshPublicIP(ctx)
	if n := len(publicIPRates(t, s, "nh")); n != 1 {
		t.Fatalf("refresh reopened: %d periods", n)
	}
	store := func(addresses int) {
		t.Helper()
		n := hostA()
		n.Zone, n.PublicIPv4 = "eu-north-1a", addresses
		if err := s.storeNetworkRefresh(ctx, "nh", "ec2", "eu-north-1", n, catalogEU(at("09:00"))); err != nil {
			t.Fatal(err)
		}
	}
	store(1) // the same count, other subnets: the period stays
	if got := publicIPRates(t, s, "nh"); len(got) != 1 || got[0].To != nil {
		t.Fatalf("unchanged count reopened: %+v", got)
	}
	store(3)
	got = publicIPRates(t, s, "nh")
	if len(got) != 2 || got[0].To == nil || !got[0].To.Equal(got[1].From) || got[1].To != nil || got[1].PerHour != "0.015" || got[1].Addresses != 3 {
		t.Fatalf("after 3 addresses: %+v", got)
	}
	store(0)
	got = publicIPRates(t, s, "nh")
	if len(got) != 2 || got[1].To == nil {
		t.Fatalf("after 0 addresses: %+v", got)
	}
	store(2)
	if got := publicIPRates(t, s, "nh"); len(got) != 3 || got[2].PerHour != "0.01" || got[2].To != nil {
		t.Fatalf("after 2 addresses: %+v", got)
	}
}

// A host whose address count was recorded before it registered (as #114
// recorded every host's network) and whose Runs and host hours were priced
// before its first public-ip period opened: the period opens from its
// registration, its earlier host hours are rebuilt with public-ip rows, and
// its final Runs are evaluated again and gain their share. Host-hour rows
// split public IPv4 by the same shares as compute, allocated + unallocated
// = the addresses' cost of each hour, allocated = the Runs' lines.
func TestPublicIPHostHours(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	s.cfg.Costs.Batch = 10
	s.cfg.Costs.Hourly = 48 * time.Hour
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ec2p', 't1', 'ec2p', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, provision_requested_at, registered_at, terminated_at, capacity, volumes, instance_type, market)
		VALUES ('hh', 't1', 'hh', 'ec2p', 'terminated', $1, $1, $2, '{"cpus":8,"memory":34359738368}', '[]', 'm7i.2xlarge', 'on-demand')`, hour, hour.Add(2*time.Hour))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('hh', $1, $2, 0.40, 'USD', 8, $3, 'ec2-pricing')`, hour, hour.Add(2*time.Hour), 32*gib)
	// A: 2 of 8 CPUs over the first hour; B: 6 of 8 over its second half.
	end1, end2 := hour.Add(time.Hour), hour.Add(time.Hour)
	placeRun(t, s, "t1", "A", StateStopping, "hh", placementWindow{CPUs: 2, Memory: 8 * gib, From: hour, To: &end1})
	placeRun(t, s, "t1", "B", StateStopping, "hh", placementWindow{CPUs: 6, Memory: 8 * gib, From: hour.Add(30 * time.Minute), To: &end2})
	finish(t, s, "t1", "A", StateSucceeded)
	finish(t, s, "t1", "B", StateSucceeded)
	drain(t, s)
	hostHours := func() []string {
		t.Helper()
		for range 3 {
			if err := s.updateHostHours(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var rows []string
		systemScan(t, s, `SELECT coalesce(array_agg(to_char(hour AT TIME ZONE 'UTC', 'HH24') || ' ' || trim_scale(allocated)::text || ' ' || trim_scale(unallocated)::text
			ORDER BY hour), '{}') FROM cost_hourly WHERE host_id = 'hh' AND run_id IS NULL AND family = 'public-ip'`, nil, &rows)
		return rows
	}
	if rows := hostHours(); len(rows) != 0 {
		t.Fatalf("public-ip rows before any period: %v", rows)
	}
	for _, run := range []string{"A", "B"} {
		if lines, src := familyLines(t, s, keys["t1"], run); len(lines) != 1 || src != "final" {
			t.Fatalf("%s before the period: %v %s", run, lines, src)
		}
	}
	execSQL(t, s, ctx, `UPDATE hosts SET network = '{"publicIPv4": 2}' WHERE id = 'hh'`)
	s.openPublicIP(ctx, publicIPHost{ID: "hh", Provider: "ec2"}, DataTransferPrices{Currency: "USD", PublicIPv4PerHour: "0.005"})
	if got := publicIPRates(t, s, "hh"); len(got) != 1 || !got[0].From.Equal(hour) {
		t.Fatalf("period %+v, want from registration %s", got, hour)
	}
	for _, run := range []string{"A", "B"} {
		if p := pending(t, s, run); p == "" {
			t.Errorf("final Run %s not re-queued", run)
		}
	}
	// 2 addresses × 0.005 = 0.01 an hour. First hour: A 1/4 of it, B 3/4 of
	// its half (0.00375); second hour: all unallocated.
	h := func(n int) string { return hour.Add(time.Duration(n) * time.Hour).Format("15") }
	rows := hostHours()
	want := []string{h(0) + " 0.00625 0.00375", h(1) + " 0 0.01"}
	if !slices.Equal(rows, want) {
		t.Errorf("public-ip host hours %v, want %v", rows, want)
	}
	for _, r := range rows {
		var hr string
		var a, u string
		fmt.Sscan(r, &hr, &a, &u)
		if sum := new(big.Rat).Add(mustRat(a), mustRat(u)); sum.Cmp(big.NewRat(1, 100)) != 0 {
			t.Errorf("%s: allocated + unallocated = %s, want the hour's 0.01", hr, sum.FloatString(9))
		}
	}
	drain(t, s)
	for run, want := range map[string]string{"A": "public-ip ipv4 0.0025 true", "B": "public-ip ipv4 0.00375 true"} {
		lines, src := familyLines(t, s, keys["t1"], run)
		if len(lines) != 2 || lines[1] != want || src != "final" {
			t.Errorf("%s: %v %s, want %s", run, lines, src, want)
		}
	}
}

// Addresses first recorded after the host registered (an address attached
// later, or a count of 0 that became 1) are charged from when the provider
// check recorded them, not from registration: the period opens then, at
// once (the price is cached), a Run that ended before is left final as it
// was, and a live one is evaluated again.
func TestPublicIPLateAddresses(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 0) // 0 addresses, registered 10:00
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	placeRun(t, s, "t1", "B", StateRunning, "nh", place("B", 2, 8, "10:30", ""))
	drain(t, s)
	before, _ := familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(before) != "[compute m7i.2xlarge 0.1 true]" {
		t.Fatalf("A before: %v", before)
	}
	execSQL(t, s, ctx, `DELETE FROM cost_pending`)
	recorded := time.Now()
	n := hostA()
	n.Zone, n.PublicIPv4 = "eu-north-1a", 1
	if err := s.storeNetworkRefresh(ctx, "nh", "ec2", "eu-north-1", n, catalogEU(at("09:00"))); err != nil {
		t.Fatal(err)
	}
	got := publicIPRates(t, s, "nh")
	if len(got) != 1 || got[0].From.Before(recorded) || got[0].From.After(time.Now()) || got[0].To != nil {
		t.Fatalf("period %+v, want from %s (recorded), not registration", got, recorded)
	}
	if p := pending(t, s, "B"); p == "" {
		t.Error("live Run B not re-queued")
	}
	if p := pending(t, s, "A"); p != "" {
		t.Errorf("A, ended before the addresses, re-queued: %q", p)
	}
	if lines, src := familyLines(t, s, keys["t1"], "A"); !slices.Equal(lines, before) || src != "final" {
		t.Errorf("A after: %v %s", lines, src)
	}
	drain(t, s)
	assertPublicIPSince(t, s, keys["t1"], "B")
}

// Where a provider host's first public-ip period starts, by the order in
// which its registration, its address count and its region's price became
// known: from registration only when the addresses were recorded by then,
// otherwise from the first recording of addresses, not a later count.
func TestPublicIPPeriodStart(t *testing.T) {
	refresh := func(t *testing.T, s *Server, addresses int) time.Time {
		t.Helper()
		n := hostA()
		n.Zone, n.PublicIPv4 = "eu-north-1a", addresses
		recorded := time.Now()
		if err := s.storeNetworkRefresh(context.Background(), "nh", "ec2", "eu-north-1", n, catalogEU(at("09:00"))); err != nil {
			t.Fatal(err)
		}
		return recorded
	}
	for _, c := range []struct {
		name string
		// run sets the host up and returns the expected period start's
		// bounds and address count.
		run func(t *testing.T, s *Server) (from, to time.Time, addresses int)
	}{
		{"network first recorded after registration", func(t *testing.T, s *Server) (time.Time, time.Time, int) {
			execSQL(t, s, context.Background(), `UPDATE hosts SET network = NULL WHERE id = 'nh'`)
			cacheTransfer(t, s)
			recorded := refresh(t, s, 1)
			return recorded, time.Now(), 1
		}},
		{"network recorded before registration", func(t *testing.T, s *Server) (time.Time, time.Time, int) {
			ctx := context.Background()
			execSQL(t, s, ctx, `UPDATE hosts SET network = NULL, registered_at = NULL WHERE id = 'nh'`)
			cacheTransfer(t, s)
			refresh(t, s, 1)
			registered := time.Now().Add(time.Second).UTC().Truncate(time.Microsecond)
			execSQL(t, s, ctx, `UPDATE hosts SET registered_at = $1 WHERE id = 'nh'`, registered)
			s.refreshPublicIP(ctx)
			return registered, registered, 1
		}},
		{"count changed before a price was cached", func(t *testing.T, s *Server) (time.Time, time.Time, int) {
			first := refresh(t, s, 1)
			firstDone := time.Now()
			time.Sleep(10 * time.Millisecond)
			refresh(t, s, 2)
			cacheTransfer(t, s)
			s.refreshPublicIP(context.Background())
			return first, firstDone, 2
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := costFixture(t)
			s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
			transferHost(t, s, false, 0)
			from, to, addresses := c.run(t, s)
			got := publicIPRates(t, s, "nh")
			if len(got) != 1 || got[0].From.Before(from) || got[0].From.After(to) || got[0].Addresses != addresses {
				t.Fatalf("periods %+v, want one from [%s, %s] with %d addresses", got, from.UTC(), to.UTC(), addresses)
			}
		})
	}
}

// assertPublicIPSince: live Run runID, placed weeks before its host's
// public-ip period opened a moment ago, is charged from the period's start
// only, so its public-ip line is under a nanodollar-scale bound rather
// than weeks of its share.
func assertPublicIPSince(t *testing.T, s *Server, key, runID string) {
	t.Helper()
	_, c := getCost(t, s, key, runID)
	for _, l := range c.Lines {
		if l.Family == familyPublicIP && mustRat(l.Amount).Cmp(big.NewRat(1, 1_000_000)) > 0 {
			t.Errorf("%s public-ip %s: charged from before the period opened", runID, l.Amount)
		}
	}
}

// A placement on a host with addresses gets a public-ip line from the same
// shares as compute; the Run is final only once it is priced.
func TestPublicIPRunLine(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 1)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "11:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	lines, src := familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 false]" || src != "incomplete" {
		t.Fatalf("before the period: %v %s", lines, src)
	}
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	s.refreshPublicIP(ctx) // re-queues A
	drain(t, s)
	// 1 address × 0.005 for an hour, a quarter share: 0.00125.
	lines, src = familyLines(t, s, keys["t1"], "A")
	if fmt.Sprint(lines) != "[compute m7i.2xlarge 0.1 true public-ip ipv4 0.00125 true]" || src != "final" {
		t.Errorf("priced: %v %s", lines, src)
	}
}

// A placement that started before its host's first public-ip period is
// priced from that first period (the next after its start), not from the
// latest: with periods from 11:00 (1 address) and 12:00 (3 addresses), a
// placement from 10:00 to 13:00 is charged from 11:00, at the first
// period's rate (the latest from 12:00 would be 0.015 × 1 h × ¼).
func TestPublicIPFirstPeriodAfterStart(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	transferHost(t, s, false, 2)
	cacheTransfer(t, s)
	s.cfg.Costs.Hourly = 100 * 365 * 24 * time.Hour
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, family, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source, details) VALUES
		('nh', 'public-ip', $1, $2, 0.005, 'USD', 8, $3, 'ec2-vpc-pricing', '{"addresses":1}'),
		('nh', 'public-ip', $2, NULL, 0.015, 'USD', 8, $3, 'ec2-vpc-pricing', '{"addresses":3}')`, at("11:00"), at("12:00"), 32*gib)
	placeRun(t, s, "t1", "A", StateStopping, "nh", place("A", 2, 8, "10:00", "13:00"))
	finish(t, s, "t1", "A", StateSucceeded)
	drain(t, s)
	_, c := getCost(t, s, keys["t1"], "A")
	var got []string
	for _, l := range c.Lines {
		if l.Family == familyPublicIP {
			got = append(got, l.Amount)
		}
	}
	// 0.005 × 2 h × a quarter share.
	if want := []string{"0.0025"}; !slices.Equal(got, want) {
		t.Errorf("public-ip lines %v, want %v", got, want)
	}
}
