package server

import (
	"context"
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
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
	}, {
		name: "no memory capacity: shared by cpus alone",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Rates:      []ratePeriod{{From: at("10:00"), PerHour: "0.40", Currency: "USD", CapCPUs: 8, Source: "static"}},
			Placements: []placementWindow{place("A", 4, 30, "10:00", "11:00")}},
		// S = 4/8; the 30 GiB it reserved count for nothing.
		want:    map[string]string{"A": "0.2"},
		unalloc: "0.2",
	}, {
		name: "no cpu capacity: shared by memory alone",
		in: hostCompute{From: at("10:00"), To: atp("11:00"),
			Rates:      []ratePeriod{{From: at("10:00"), PerHour: "0.40", Currency: "USD", CapMemory: 32 * gib, Source: "static"}},
			Placements: []placementWindow{place("A", 6, 16, "10:00", "11:00")}},
		// S = 16/32; the 6 cpus it reserved count for nothing.
		want:    map[string]string{"A": "0.2"},
		unalloc: "0.2",
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
	total := map[string]*big.Rat{}
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
		addTo(total, p.Rate.Currency, p.Host)
	}
	// Per currency: amounts in one are never added to another's.
	all := map[string]*big.Rat{}
	for currency, u := range res.Unallocated {
		addTo(all, currency, u)
	}
	for _, pc := range res.Placements {
		for currency, a := range pc.Amounts {
			addTo(all, currency, a)
		}
	}
	for currency, v := range total {
		if a := all[currency]; a == nil || a.Cmp(v) != 0 {
			t.Errorf("%s: allocated + unallocated = %v, host cost %s", currency, a, v.FloatString(12))
		}
	}
	for currency, a := range all {
		if total[currency] == nil && a.Sign() != 0 {
			t.Errorf("%s: %s allocated and unallocated, no host cost", currency, a.FloatString(12))
		}
	}
}

// A period in another currency is priced in it, never added to the
// first's; a fractional cpus share is the exact decimal ratio.
func TestComputeCostCurrenciesAndFractions(t *testing.T) {
	eur := usdRate("10:30", "", "0.60")
	eur.Currency = "EUR"
	in := hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{usdRate("10:00", "10:30", "0.40"), eur},
		Placements: []placementWindow{place("A", 4, 8, "10:15", "10:45")}}
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	// A (share 1/2): 15 minutes at $0.40, then 15 at €0.60.
	for currency, want := range map[string][2]string{"USD": {"0.05", "0.15"}, "EUR": {"0.075", "0.225"}} {
		a, u := res.Placements[0].Amounts[currency], res.Unallocated[currency]
		if a == nil || u == nil || moneyString(a) != want[0] || moneyString(u) != want[1] {
			t.Errorf("%s: A %v, unallocated %v; want %s and %s", currency, a, u, want[0], want[1])
		}
	}
	if len(res.Placements[0].Amounts) != 2 || len(res.Unallocated) != 2 {
		t.Errorf("currencies: A %v, unallocated %v", res.Placements[0].Amounts, res.Unallocated)
	}
	checkPieces(t, in, res)

	in = hostCompute{From: at("10:00"), To: atp("11:00"),
		Rates:      []ratePeriod{{From: at("10:00"), PerHour: "0.30", Currency: "USD", CapCPUs: 0.3, CapMemory: 32 * gib, Source: "static"}},
		Placements: []placementWindow{place("A", 0.1, 1, "10:00", "11:00")}}
	if res, err = computeCost(in); err != nil {
		t.Fatal(err)
	}
	if s := res.Pieces[0].S.RatString(); s != "1/3" {
		t.Errorf("S of 0.1 cpus on 0.3: %s, want 1/3", s)
	}
	if a, u := res.Placements[0].Amounts["USD"], res.Unallocated["USD"]; a.Cmp(big.NewRat(1, 10)) != 0 || u.Cmp(big.NewRat(2, 10)) != 0 {
		t.Errorf("A %s, unallocated %s; want exactly 1/10 and 1/5", a.RatString(), u.RatString())
	}
	checkPieces(t, in, res)
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
	in := hostCompute{From: at("10:00"), To: atp("11:00"), Rates: []ratePeriod{usdRate("10:00", "", "0.40")}, Placements: workedExample}
	res, err := computeCost(in)
	if err != nil {
		t.Fatal(err)
	}
	// Live: A, B and C are placements 0, 1 and 2.
	want := []struct {
		from, s, unalloc string
		live             []int
		charged          []string
	}{
		{"10:00", "1/4", "0.075", []int{0}, []string{"0.025"}},
		{"10:15", "3/4", "0.025", []int{0, 1}, []string{"0.025", "0.05"}},
		{"10:30", "1", "0", []int{1, 2}, []string{"0.05", "0.05"}},
		{"10:45", "1/2", "0.05", []int{1}, []string{"0.05"}},
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
		if !p.From.Equal(at(w.from)) || p.Rate != &in.Rates[0] || !slices.Equal(p.Live, w.live) || p.S.RatString() != w.s ||
			moneyString(p.Unallocated) != w.unalloc || moneyString(p.Host) != "0.1" || !slices.Equal(charged, w.charged) {
			t.Errorf("piece %d: from %s rate %p live %v S %s unalloc %s host %s charged %v; want %+v",
				i, p.From.Format("15:04"), p.Rate, p.Live, p.S.RatString(), moneyString(p.Unallocated), moneyString(p.Host), charged, w)
		}
	}
}

// computeCost against naiveComputeCost, which works out every piece from
// scratch, on random hosts drawn from a coarse grid of instants, so that
// touching, coincident, zero-length and inverted ranges, open ends,
// overlapping periods and a second currency all come up often. Every piece,
// amount, missing range and error must be the same.
func TestComputeCostMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	grid := func() time.Time { return at("10:00").Add(time.Duration(rng.IntN(13)) * 5 * time.Minute) }
	gridp := func() *time.Time {
		if rng.IntN(4) == 0 {
			return nil
		}
		t := grid()
		return &t
	}
	for n := range 5000 {
		// Mostly a billed window of 5 to 60 minutes, open or closed; now and
		// then an empty or inverted one.
		in := hostCompute{HostID: "h", From: grid()}
		in.Now = in.From.Add(time.Duration(1+rng.IntN(12)) * 5 * time.Minute)
		switch rng.IntN(8) {
		case 0:
			in.Now = grid()
		case 1:
			in.To = gridp()
		case 2, 3:
		default:
			in.To = &in.Now
		}
		for range rng.IntN(4) {
			in.Rates = append(in.Rates, ratePeriod{From: grid(), To: gridp(),
				PerHour: []string{"0", "0.40", "1.3"}[rng.IntN(3)], Currency: []string{"USD", "EUR"}[rng.IntN(2)],
				CapCPUs: []float64{0, 4, 8}[rng.IntN(3)], CapMemory: []int64{0, 16, 32}[rng.IntN(3)] * gib})
		}
		for i := range rng.IntN(9) {
			in.Placements = append(in.Placements, placementWindow{ID: fmt.Sprint(i),
				CPUs: []float64{0, 1, 2.5, 4}[rng.IntN(4)], Memory: []int64{0, 4, 16}[rng.IntN(3)] * gib, From: grid(), To: gridp()})
		}
		res, err := computeCost(in)
		want, wantErr := naiveComputeCost(in)
		if got, want := dumpCost(in, res, err), dumpCost(in, want, wantErr); got != want {
			t.Fatalf("input %d %+v:\ngot\n%s\nwant\n%s", n, in, got, want)
		}
	}
}

// dumpCost is everything computeCost answers, as text.
func dumpCost(in hostCompute, res computeResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	rat := func(r *big.Rat) string {
		if r == nil {
			return "nil"
		}
		return r.RatString()
	}
	amounts := func(m map[string]*big.Rat) string {
		var s []string
		for c, v := range m {
			s = append(s, c+" "+rat(v))
		}
		slices.Sort(s)
		return fmt.Sprint(s)
	}
	for _, p := range res.Pieces {
		ri := -1
		for i := range in.Rates {
			if p.Rate == &in.Rates[i] {
				ri = i
			}
		}
		var charged []string
		for _, c := range p.Charged {
			charged = append(charged, rat(c))
		}
		fmt.Fprintf(&b, "piece %s–%s rate %d live %v S %s host %s charged %v unallocated %s\n",
			p.From.Format("15:04"), p.To.Format("15:04"), ri, p.Live, rat(p.S), rat(p.Host), charged, rat(p.Unallocated))
	}
	for i, pc := range res.Placements {
		fmt.Fprintf(&b, "placement %d %s missing %v\n", i, amounts(pc.Amounts), pc.Missing)
	}
	fmt.Fprintf(&b, "unallocated %s missing %v\n", amounts(res.Unallocated), res.Missing)
	return b.String()
}

// naiveComputeCost is computeCost as it was before the sweep: every piece
// scans every rate period and placement.
func naiveComputeCost(in hostCompute) (computeResult, error) {
	from, to := in.From, in.Now
	if in.To != nil {
		to = *in.To
	}
	end := func(t *time.Time) time.Time {
		if t == nil {
			return to
		}
		return *t
	}
	res := computeResult{Placements: make([]placementCost, len(in.Placements)), Unallocated: map[string]*big.Rat{}}
	for i := range res.Placements {
		res.Placements[i].Amounts = map[string]*big.Rat{}
	}
	if !to.After(from) {
		return res, nil
	}
	rates := make([]*big.Rat, len(in.Rates))
	cuts := []time.Time{from, to}
	clip := func(t time.Time) time.Time {
		switch {
		case t.Before(from):
			return from
		case t.After(to):
			return to
		}
		return t
	}
	for i, r := range in.Rates {
		v, ok := new(big.Rat).SetString(r.PerHour)
		if !ok {
			return res, fmt.Errorf("host %s: rate %q from %s is not a decimal", in.HostID, r.PerHour, r.From)
		}
		if r.CapCPUs <= 0 && r.CapMemory <= 0 {
			return res, fmt.Errorf("host %s: rate period from %s has no capacity to share", in.HostID, r.From)
		}
		rates[i] = v
		cuts = append(cuts, clip(r.From), clip(end(r.To)))
	}
	for _, p := range in.Placements {
		cuts = append(cuts, clip(p.From), clip(end(p.To)))
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)

	for k := 0; k+1 < len(cuts); k++ {
		a, b := cuts[k], cuts[k+1]
		piece := costPiece{timeRange: timeRange{a, b}}
		for i, p := range in.Placements {
			if !p.From.After(a) && end(p.To).After(a) {
				piece.Live = append(piece.Live, i)
			}
		}
		ri := -1
		for i, r := range in.Rates {
			if !r.From.After(a) && end(r.To).After(a) {
				if ri >= 0 {
					return res, fmt.Errorf("host %s: rate periods from %s and %s overlap", in.HostID, in.Rates[ri].From, r.From)
				}
				ri = i
			}
		}
		if ri < 0 {
			res.Missing = appendRange(res.Missing, piece.timeRange)
			for _, i := range piece.Live {
				res.Placements[i].Missing = appendRange(res.Placements[i].Missing, piece.timeRange)
			}
			res.Pieces = append(res.Pieces, piece)
			continue
		}
		r := &in.Rates[ri]
		piece.Rate = r
		shares := make([]*big.Rat, len(piece.Live))
		piece.S = new(big.Rat)
		for j, i := range piece.Live {
			shares[j] = share(in.Placements[i], r)
			piece.S.Add(piece.S, shares[j])
		}
		dt := new(big.Rat).SetFrac64(int64(b.Sub(a)), 1)
		piece.Host = new(big.Rat).Mul(rates[ri], dt)
		piece.Host.Quo(piece.Host, nanosPerHour)
		scale := big.NewRat(1, 1)
		if piece.S.Cmp(scale) > 0 {
			scale.Set(piece.S)
		}
		piece.Charged = make([]*big.Rat, len(piece.Live))
		for j, i := range piece.Live {
			c := new(big.Rat).Mul(piece.Host, shares[j])
			c.Quo(c, scale)
			piece.Charged[j] = c
			addTo(res.Placements[i].Amounts, r.Currency, c)
		}
		piece.Unallocated = new(big.Rat)
		if free := new(big.Rat).Sub(big.NewRat(1, 1), piece.S); free.Sign() > 0 {
			piece.Unallocated.Mul(piece.Host, free)
		}
		addTo(res.Unallocated, r.Currency, piece.Unallocated)
		res.Pieces = append(res.Pieces, piece)
	}
	return res, nil
}

// Overlapping periods are a bug in whatever wrote them: refused, never
// priced twice. A rate that is not a decimal is refused too, and so is a
// period with neither cpus nor memory, which would price every Run at zero.
func TestComputeCostRefusesBadRates(t *testing.T) {
	for _, rates := range [][]ratePeriod{
		{usdRate("10:00", "", "0.40"), usdRate("10:30", "", "0.40")},
		{usdRate("10:00", "", "abc")},
		{{From: at("10:00"), PerHour: "0.40", Currency: "USD", Source: "static"}}, // no capacity at all
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
	// h1 was launched by its provider at 10:00 and registered at 10:05: it is
	// billed from the launch.
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = $1 WHERE id = 'h2'`, at("10:20"))
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = $1 WHERE id = 'h1'`, at("10:05"))
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
