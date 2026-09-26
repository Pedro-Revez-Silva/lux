package server

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

const gib = int64(1) << 30

func at(hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		panic(err)
	}
	return time.Date(2026, 9, 26, t.Hour(), t.Minute(), 0, 0, time.UTC)
}

func atp(hhmm string) *time.Time {
	t := at(hhmm)
	return &t
}

// usdRate is a USD period on the worked example's host (8 CPUs, 32 GiB).
func usdRate(from, to, perHour string) ratePeriod {
	r := ratePeriod{From: at(from), PerHour: perHour, Currency: "USD", CapCPUs: 8, CapMemory: 32 * gib, Source: "static"}
	if to != "" {
		r.To = atp(to)
	}
	return r
}

func place(run string, cpus float64, memGiB int64, from, to string) placementWindow {
	p := placementWindow{ID: "p-" + run, RunID: run, Epoch: 1, CPUs: cpus, Memory: memGiB * gib, From: at(from)}
	if to != "" {
		p.To = atp(to)
	}
	return p
}

// Section 2's worked example: A, B and C on one 8 CPU / 32 GiB host at
// $0.40/h over 10:00–11:00; and again with D (S = 1.25 in 10:30–10:45).
var workedExample = []placementWindow{
	place("A", 2, 8, "10:00", "10:30"),
	place("B", 1, 16, "10:15", "11:00"),
	place("C", 4, 4, "10:30", "10:45"),
}

func TestComputeCost(t *testing.T) {
	for _, c := range []struct {
		name       string
		in         hostCompute
		want       map[string]string // run → USD amount
		unalloc    string
		missing    []timeRange
		runMissing map[string][]timeRange
	}{{
		name: "worked example",
		in: hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{usdRate("10:00", "", "0.40")},
			Placements: workedExample},
		want:    map[string]string{"A": "0.05", "B": "0.15", "C": "0.05"},
		unalloc: "0.15",
	}, {
		name: "worked example with D",
		in: hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{usdRate("10:00", "", "0.40")},
			Placements: append(append([]placementWindow{}, workedExample...), place("D", 2, 4, "10:30", "10:45"))},
		// 10:30–10:45: B, C and D pay 0.5/1.25, 0.5/1.25, 0.25/1.25 of $0.10.
		want:    map[string]string{"A": "0.05", "B": "0.14", "C": "0.04", "D": "0.02"},
		unalloc: "0.15",
	}, {
		name: "rate change mid-placement",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Rates:      []ratePeriod{usdRate("10:00", "10:30", "0.40"), usdRate("10:30", "", "0.80")},
			Placements: []placementWindow{place("A", 4, 8, "10:15", "10:45")}},
		// A (share 0.5): 15 min at 0.40 and 15 min at 0.80, half each.
		want:    map[string]string{"A": "0.15"},
		unalloc: "0.45",
	}, {
		name: "gap with no rate is missing, not zero",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Rates:      []ratePeriod{usdRate("10:00", "10:15", "0.40"), usdRate("10:30", "", "0.40")},
			Placements: []placementWindow{place("A", 8, 32, "10:00", "11:00")}},
		want:       map[string]string{"A": "0.3"},
		unalloc:    "0",
		missing:    []timeRange{{at("10:15"), at("10:30")}},
		runMissing: map[string][]timeRange{"A": {{at("10:15"), at("10:30")}}},
	}, {
		name: "no rate at all",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Placements: []placementWindow{place("A", 2, 8, "10:00", "10:30")}},
		want:       map[string]string{"A": ""},
		unalloc:    "",
		missing:    []timeRange{{at("10:00"), at("11:00")}},
		runMissing: map[string][]timeRange{"A": {{at("10:00"), at("10:30")}}},
	}, {
		name: "live placement and host end at now",
		in: hostCompute{From: at("10:00"), Now: at("10:45"), Rates: []ratePeriod{usdRate("10:00", "", "0.40")},
			Placements: []placementWindow{place("A", 2, 8, "10:15", "")}},
		want:    map[string]string{"A": "0.05"},
		unalloc: "0.25",
	}, {
		name:    "no placements: all unallocated",
		in:      hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{usdRate("10:00", "", "0.40")}},
		want:    map[string]string{},
		unalloc: "0.4",
	}, {
		name: "capacity change opens a new period",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Rates: []ratePeriod{usdRate("10:00", "10:30", "0.40"),
				{From: at("10:30"), PerHour: "0.40", Currency: "USD", CapCPUs: 4, CapMemory: 16 * gib, Source: "static"}},
			Placements: []placementWindow{place("A", 2, 8, "10:00", "11:00")}},
		// Share 0.25 on 8 CPUs, then 0.5 on 4: 0.05 + 0.10.
		want:    map[string]string{"A": "0.15"},
		unalloc: "0.25",
	}, {
		name: "placement before and after the billed window is clipped",
		in: hostCompute{From: at("10:00"), To: atp("10:30"), Rates: []ratePeriod{usdRate("09:00", "", "0.40")},
			Placements: []placementWindow{place("A", 8, 32, "09:30", "11:00")}},
		want:    map[string]string{"A": "0.2"},
		unalloc: "0",
	}} {
		t.Run(c.name, func(t *testing.T) {
			res, err := computeCost(c.in)
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range c.in.Placements {
				got := ""
				if a := res.Placements[i].Amounts["USD"]; a != nil {
					got = moneyString(a)
				}
				if want := c.want[p.RunID]; got != want {
					t.Errorf("run %s: got %q, want %q", p.RunID, got, want)
				}
				if got, want := res.Placements[i].Missing, c.runMissing[p.RunID]; !rangesEqual(got, want) {
					t.Errorf("run %s missing: got %v, want %v", p.RunID, got, want)
				}
			}
			got := ""
			if u := res.Unallocated["USD"]; u != nil {
				got = moneyString(u)
			}
			if got != c.unalloc {
				t.Errorf("unallocated: got %q, want %q", got, c.unalloc)
			}
			if !rangesEqual(res.Missing, c.missing) {
				t.Errorf("missing: got %v, want %v", res.Missing, c.missing)
			}
			checkPieces(t, c.in, res)
		})
	}
}

// checkPieces: allocated + unallocated = host cost in every priced piece,
// exactly; and the whole priced window's total is what the rates say.
func checkPieces(t *testing.T, in hostCompute, res computeResult) {
	t.Helper()
	total := new(big.Rat)
	for _, p := range res.Pieces {
		if p.Rate == nil {
			if p.Host != nil || p.Charged != nil || p.Unallocated != nil {
				t.Errorf("piece %s–%s has no rate but was priced", p.From, p.To)
			}
			continue
		}
		sum := new(big.Rat).Set(p.Unallocated)
		for _, c := range p.Charged {
			sum.Add(sum, c)
		}
		if sum.Cmp(p.Host) != 0 {
			t.Errorf("piece %s–%s: allocated + unallocated = %s, host cost %s", p.From, p.To, sum.FloatString(12), p.Host.FloatString(12))
		}
		total.Add(total, p.Host)
	}
	u := res.Unallocated["USD"]
	if u == nil {
		if total.Sign() != 0 {
			t.Errorf("no unallocated amount, host cost %s", total.FloatString(12))
		}
		return
	}
	all := new(big.Rat).Set(u)
	for _, pc := range res.Placements {
		if a := pc.Amounts["USD"]; a != nil {
			all.Add(all, a)
		}
	}
	if all.Cmp(total) != 0 {
		t.Errorf("allocated + unallocated = %s, host cost %s", all.FloatString(12), total.FloatString(12))
	}
}

func rangesEqual(a, b []timeRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].From.Equal(b[i].From) || !a[i].To.Equal(b[i].To) {
			return false
		}
	}
	return true
}

// The worked example piece by piece, as section 2's table shows it.
func TestComputeCostWorkedExamplePieces(t *testing.T) {
	res, err := computeCost(hostCompute{From: at("10:00"), To: atp("11:00"),
		Rates: []ratePeriod{usdRate("10:00", "", "0.40")}, Placements: workedExample})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		from, s, unalloc string
		charged          []string
	}{
		{"10:00", "1/4", "0.075", []string{"0.025"}},
		{"10:15", "3/4", "0.025", []string{"0.025", "0.05"}},
		{"10:30", "1", "0", []string{"0.05", "0.05"}},
		{"10:45", "1/2", "0.05", []string{"0.05"}},
	}
	if len(res.Pieces) != len(want) {
		t.Fatalf("%d pieces, want %d", len(res.Pieces), len(want))
	}
	for i, w := range want {
		p := res.Pieces[i]
		var charged []string
		for _, c := range p.Charged {
			charged = append(charged, moneyString(c))
		}
		if !p.From.Equal(at(w.from)) || p.S.RatString() != w.s || moneyString(p.Unallocated) != w.unalloc || moneyString(p.Host) != "0.1" ||
			len(charged) != len(w.charged) || (len(charged) > 0 && charged[0] != w.charged[0]) || (len(charged) > 1 && charged[1] != w.charged[1]) {
			t.Errorf("piece %d: from %s S %s unalloc %s host %s charged %v; want %+v",
				i, p.From.Format("15:04"), p.S.RatString(), moneyString(p.Unallocated), moneyString(p.Host), charged, w)
		}
	}
}

// Overlapping periods are a bug in whatever wrote them: refused, never
// priced twice. A rate that is not a decimal is refused too.
func TestComputeCostRefusesBadRates(t *testing.T) {
	for _, rates := range [][]ratePeriod{
		{usdRate("10:00", "", "0.40"), usdRate("10:30", "", "0.40")},
		{usdRate("10:00", "", "abc")},
	} {
		if _, err := computeCost(hostCompute{From: at("10:00"), To: atp("11:00"), Rates: rates}); err == nil {
			t.Errorf("rates %+v were accepted", rates)
		}
	}
}

func TestMoneyString(t *testing.T) {
	for r, want := range map[string]string{"1/10": "0.1", "0": "0", "3": "3", "1/3": "0.333333333", "2/3": "0.666666667", "-1/20": "-0.05", "-1/10000000000": "0"} {
		v, _ := new(big.Rat).SetString(r)
		if got := moneyString(v); got != want {
			t.Errorf("%s: got %s, want %s", r, got, want)
		}
	}
}

// loadHostCompute reads the worked example back from Postgres: its rate
// periods, billed window and placements, live ones included; computeCost
// then gives the same amounts as from the literal input.
func TestLoadHostCompute(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at, terminated_at)
		VALUES ('h1', 't1', 'h1', 'terminated', $1, $2), ('h2', 't1', 'h2', 'ready', NULL, NULL)`, at("10:00"), at("11:00"))
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = $1 WHERE id = 'h2'`, at("10:20"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h1', $1, NULL, 0.40, 'USD', 8, $2, 'static')`, at("10:00"), 32*gib)
	for i, p := range append(append([]placementWindow{}, workedExample...), place("D", 2, 4, "10:30", "10:45"), place("E", 1, 1, "10:50", "")) {
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, 't1', '{}', 'running')`, p.RunID)
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
			VALUES ($1, 't1', $2, 'h1', 1, CASE WHEN $5::timestamptz IS NULL THEN 'running' ELSE 'exited' END,
				jsonb_build_object('cpus', $3::float8, 'memory', $4::int8), $6, $5)`,
			fmt.Sprint("p", i), p.RunID, p.CPUs, p.Memory, p.To, p.From)
	}

	var in hostCompute
	var err error
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		in, err = loadHostCompute(ctx, tx, "h1", at("00:00"), at("12:00"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !in.From.Equal(at("10:00")) || in.To == nil || !in.To.Equal(at("11:00")) || len(in.Rates) != 1 || len(in.Placements) != 5 ||
		in.Rates[0].PerHour != "0.400000000" || in.Rates[0].CapCPUs != 8 || in.Rates[0].CapMemory != 32*gib || in.Rates[0].To != nil ||
		in.Placements[1].CPUs != 1 || in.Placements[1].Memory != 16*gib || in.Placements[4].To != nil {
		t.Fatalf("loaded %+v", in)
	}
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	// E (share 1/8, from 10:50, still live) runs to the host's end.
	want := map[string]string{"A": "0.05", "B": "0.14", "C": "0.04", "D": "0.02", "E": "0.008333333"}
	for i, p := range in.Placements {
		if got := moneyString(res.Placements[i].Amounts["USD"]); got != want[p.RunID] {
			t.Errorf("run %s: %s, want %s", p.RunID, got, want[p.RunID])
		}
	}
	if got := moneyString(res.Unallocated["USD"]); got != "0.141666667" {
		t.Errorf("unallocated %s", got)
	}
	checkPieces(t, in, res)

	// A host that registered itself is billed from its first hello, and
	// with no period at all that whole window is missing.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		in, err = loadHostCompute(ctx, tx, "h2", at("00:00"), at("11:00"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res, err = computeCost(in); err != nil {
		t.Fatal(err)
	}
	if !rangesEqual(res.Missing, []timeRange{{at("10:20"), at("11:00")}}) || len(res.Unallocated) != 0 {
		t.Errorf("unpriced host: missing %v, unallocated %v", res.Missing, res.Unallocated)
	}
}

// A window reads only the periods and placements overlapping it, and clips
// the billed window and whatever straddles its edges to it.
func TestLoadHostComputeWindow(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provision_requested_at) VALUES ('h1', 't1', 'h1', 'ready', $1)`, at("10:00"))
	execSQL(t, s, ctx, `INSERT INTO host_rates (host_id, valid_from, valid_to, per_hour, currency, cap_cpus, cap_memory, source)
		VALUES ('h1', $1, $2, 0.40, 'USD', 8, $4, 'static'), ('h1', $2, $3, 0.80, 'USD', 8, $4, 'static'),
			('h1', $3, NULL, 1.20, 'USD', 8, $4, 'static')`, at("10:00"), at("10:30"), at("11:05"), 32*gib)
	for i, p := range []placementWindow{
		place("A", 4, 8, "10:00", "10:20"), // before the window
		place("B", 4, 8, "10:25", "10:50"), // straddles its start
		place("C", 2, 8, "10:45", "11:15"), // straddles its end
		place("D", 2, 8, "11:10", ""),      // after it, still live
	} {
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, 't1', '{}', 'running')`, p.RunID)
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at, ended_at)
			VALUES ($1, 't1', $2, 'h1', 1, CASE WHEN $5::timestamptz IS NULL THEN 'running' ELSE 'exited' END,
				jsonb_build_object('cpus', $3::float8, 'memory', $4::int8), $6, $5)`,
			fmt.Sprint("p", i), p.RunID, p.CPUs, p.Memory, p.To, p.From)
	}

	var in hostCompute
	var err error
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		in, err = loadHostCompute(ctx, tx, "h1", at("10:40"), at("11:00"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, p := range in.Placements {
		runs = append(runs, p.RunID)
	}
	if !in.From.Equal(at("10:40")) || in.To == nil || !in.To.Equal(at("11:00")) ||
		len(in.Rates) != 1 || in.Rates[0].PerHour != "0.800000000" || fmt.Sprint(runs) != "[B C]" {
		t.Fatalf("loaded window %s–%v, rates %+v, runs %v", in.From, in.To, in.Rates, runs)
	}
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	// At $0.80/h: B (share 1/2) for 10 minutes, C (share 1/4) for 15.
	want := map[string]string{"B": "0.066666667", "C": "0.05"}
	for i, p := range in.Placements {
		if got := moneyString(res.Placements[i].Amounts["USD"]); got != want[p.RunID] {
			t.Errorf("run %s: %s, want %s", p.RunID, got, want[p.RunID])
		}
	}
	if got := moneyString(res.Unallocated["USD"]); got != "0.15" || len(res.Missing) != 0 {
		t.Errorf("unallocated %s, missing %v", got, res.Missing)
	}
	checkPieces(t, in, res)
}

// Many sequential placements beside a long one: the sweep keeps this
// linear (the time is logged; see TestComputeCost for the formula).
func TestComputeCostManyPlacements(t *testing.T) {
	const n = 8000
	base := at("00:00")
	in := hostCompute{From: base, Now: base.Add((n + 1) * time.Minute), Rates: []ratePeriod{usdRate("00:00", "", "0.40")},
		Placements: []placementWindow{{ID: "long", RunID: "long", CPUs: 2, Memory: 8 * gib, From: base}}}
	for i := range n {
		from := base.Add(time.Duration(i)*time.Minute + 10*time.Second)
		to := from.Add(30 * time.Second)
		in.Placements = append(in.Placements, placementWindow{ID: fmt.Sprint(i), RunID: fmt.Sprint(i), CPUs: 1, Memory: 2 * gib, From: from, To: &to})
	}
	start := time.Now()
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d placements, %d pieces: %s", len(in.Placements), len(res.Pieces), time.Since(start))
	// long: 1/4 of $0.40/h for 8001 minutes; each short one 1/8 for 30 s.
	if got := moneyString(res.Placements[0].Amounts["USD"]); got != "13.335" {
		t.Errorf("long placement: %s, want 13.335", got)
	}
	for i := 1; i < len(res.Placements); i++ {
		if got := moneyString(res.Placements[i].Amounts["USD"]); got != "0.000416667" {
			t.Fatalf("placement %d: %s, want 0.000416667", i, got)
		}
	}
	if len(res.Pieces) != 2*n+1 {
		t.Errorf("%d pieces, want %d", len(res.Pieces), 2*n+1)
	}
	checkPieces(t, in, res)
}
