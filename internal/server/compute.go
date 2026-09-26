package server

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Compute cost (docs/costs.md, section 2): a host's billed window split at
// every placement start and end and every rate period boundary. In each
// piece, with S the sum of the live placements' shares:
//
//	share(p)   = max(p.cpus / cap.cpus, p.memory / cap.memory)
//	charged(p) = rate × dt × share(p) / max(1, S)
//	unalloc    = rate × dt × max(0, 1 − S)
//
// Money is exact: big.Rat throughout, never float64. A piece with no rate
// period is missing, never priced at zero.

// ratePeriod is one host_rates row. To nil: still current.
type ratePeriod struct {
	From      time.Time
	To        *time.Time
	PerHour   string // decimal, as numeric(24, 9) holds it
	Currency  string
	CapCPUs   float64
	CapMemory int64
	Source    string
}

// placementWindow is what a placement reserved on the host, and when. To
// nil: still live.
type placementWindow struct {
	ID     string
	RunID  string
	Epoch  int
	CPUs   float64
	Memory int64
	From   time.Time
	To     *time.Time
}

// hostCompute is computeCost's input: a host's rate periods, its billed
// window (To nil: still billed) and its placements. Now closes whatever is
// still open.
type hostCompute struct {
	HostID     string
	From       time.Time
	To         *time.Time
	Rates      []ratePeriod
	Placements []placementWindow
	Now        time.Time
}

// timeRange is [From, To).
type timeRange struct {
	From, To time.Time
}

// costPiece is one stretch of the billed window in which neither the rate
// nor the live placements change.
type costPiece struct {
	timeRange
	Rate *ratePeriod // nil: no rate period covers it (missing)
	// Live are indexes into hostCompute.Placements.
	Live []int
	S    *big.Rat
	// Host is rate × dt; Charged (by index into Live) and Unallocated add
	// up to it exactly. All nil when Rate is.
	Host        *big.Rat
	Charged     []*big.Rat
	Unallocated *big.Rat
}

// placementCost is one placement's compute cost, per currency (a host's
// periods may in principle change currency; amounts are never converted).
type placementCost struct {
	Amounts map[string]*big.Rat
	// Missing: parts of its window (inside the billed window) with no rate.
	Missing []timeRange
}

type computeResult struct {
	Pieces []costPiece
	// Placements is parallel to hostCompute.Placements.
	Placements  []placementCost
	Unallocated map[string]*big.Rat
	// Missing: parts of the billed window with no rate period.
	Missing []timeRange
}

var nanosPerHour = big.NewRat(int64(time.Hour), 1)

// computeCost prices a host's billed window. It reads nothing: the loader
// (loadHostCompute) gathers its input.
//
// One sweep over the cut instants: placements and rate periods join the
// live sets as the sweep reaches their start and leave at their end, so
// each piece costs only its live placements, and a placement's share is
// worked out once per rate period rather than once per piece.
func computeCost(in hostCompute) (computeResult, error) {
	from, to := in.From, in.Now
	if in.To != nil {
		to = *in.To
	}
	// Whatever is still open (a period, a live placement) runs to the end
	// of the billed window.
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
	cuts := make([]time.Time, 0, 2+2*len(in.Rates)+2*len(in.Placements))
	cuts = append(cuts, from, to)
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
		rates[i] = v
		cuts = append(cuts, clip(r.From), clip(end(r.To)))
	}
	for _, p := range in.Placements {
		cuts = append(cuts, clip(p.From), clip(end(p.To)))
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)

	// The sweep's events: indexes into in.Rates and in.Placements by start
	// and by end. Live sets hold indexes in ascending order.
	byStart := func(n int, at func(int) time.Time) []int {
		o := make([]int, n)
		for i := range o {
			o[i] = i
		}
		slices.SortStableFunc(o, func(a, b int) int { return at(a).Compare(at(b)) })
		return o
	}
	rateStarts := byStart(len(in.Rates), func(i int) time.Time { return in.Rates[i].From })
	rateEnds := byStart(len(in.Rates), func(i int) time.Time { return end(in.Rates[i].To) })
	pStarts := byStart(len(in.Placements), func(i int) time.Time { return in.Placements[i].From })
	pEnds := byStart(len(in.Placements), func(i int) time.Time { return end(in.Placements[i].To) })
	var liveRates, live []int
	isLive := make([]bool, len(in.Placements))
	var nrs, nre, nps, npe int
	// shares[i] is placement i's share against rate period shareOf[i].
	shares := make([]*big.Rat, len(in.Placements))
	shareOf := make([]int, len(in.Placements))

	for k := 0; k+1 < len(cuts); k++ {
		a, b := cuts[k], cuts[k+1]
		// Live at a: started at or before a, ending after it.
		for ; nrs < len(rateStarts) && !in.Rates[rateStarts[nrs]].From.After(a); nrs++ {
			if i := rateStarts[nrs]; end(in.Rates[i].To).After(a) {
				liveRates = insertSorted(liveRates, i)
			}
		}
		for ; nre < len(rateEnds) && !end(in.Rates[rateEnds[nre]].To).After(a); nre++ {
			liveRates = removeSorted(liveRates, rateEnds[nre])
		}
		for ; nps < len(pStarts) && !in.Placements[pStarts[nps]].From.After(a); nps++ {
			if i := pStarts[nps]; end(in.Placements[i].To).After(a) {
				live, isLive[i] = insertSorted(live, i), true
			}
		}
		for ; npe < len(pEnds) && !end(in.Placements[pEnds[npe]].To).After(a); npe++ {
			if i := pEnds[npe]; isLive[i] {
				live, isLive[i] = removeSorted(live, i), false
			}
		}
		if len(liveRates) > 1 {
			return res, fmt.Errorf("host %s: rate periods from %s and %s overlap", in.HostID, in.Rates[liveRates[0]].From, in.Rates[liveRates[1]].From)
		}
		piece := costPiece{timeRange: timeRange{a, b}}
		if len(live) > 0 {
			piece.Live = slices.Clone(live)
		}
		if len(liveRates) == 0 {
			res.Missing = appendRange(res.Missing, piece.timeRange)
			for _, i := range piece.Live {
				res.Placements[i].Missing = appendRange(res.Placements[i].Missing, piece.timeRange)
			}
			res.Pieces = append(res.Pieces, piece)
			continue
		}
		ri := liveRates[0]
		r := &in.Rates[ri]
		piece.Rate = r
		piece.S = new(big.Rat)
		for _, i := range piece.Live {
			if shares[i] == nil || shareOf[i] != ri {
				shares[i], shareOf[i] = share(in.Placements[i], r), ri
			}
			piece.S.Add(piece.S, shares[i])
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
			c := new(big.Rat).Mul(piece.Host, shares[i])
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

// insertSorted adds i to the ascending s; removeSorted takes it out.
func insertSorted(s []int, i int) []int {
	k, _ := slices.BinarySearch(s, i)
	return slices.Insert(s, k, i)
}

func removeSorted(s []int, i int) []int {
	if k, ok := slices.BinarySearch(s, i); ok {
		return slices.Delete(s, k, k+1)
	}
	return s
}

// share is max(cpu share, memory share) against the period's capacity. A
// capacity of zero in one resource leaves that resource out.
func share(p placementWindow, r *ratePeriod) *big.Rat {
	s := new(big.Rat)
	if r.CapCPUs > 0 {
		s.Quo(floatRat(p.CPUs), floatRat(r.CapCPUs))
	}
	if r.CapMemory > 0 {
		if m := big.NewRat(p.Memory, r.CapMemory); m.Cmp(s) > 0 {
			s = m
		}
	}
	return s
}

// floatRat is f as the decimal it prints as (0.1 is 1/10, not the nearest
// binary fraction), so shares of decimal cpus are exact.
func floatRat(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	return r
}

func addTo(m map[string]*big.Rat, currency string, v *big.Rat) {
	if m[currency] == nil {
		m[currency] = new(big.Rat)
	}
	m[currency].Add(m[currency], v)
}

// appendRange adds r, merged with the last range when they touch.
func appendRange(rs []timeRange, r timeRange) []timeRange {
	if n := len(rs); n > 0 && rs[n-1].To.Equal(r.From) {
		rs[n-1].To = r.To
		return rs
	}
	return append(rs, r)
}

// moneyString is r as a decimal numeric(24, 9) stores: rounded to 9
// fractional digits (halves away from zero), trailing zeros trimmed.
func moneyString(r *big.Rat) string {
	s := strings.TrimSuffix(strings.TrimRight(r.FloatString(9), "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}

// loadHostCompute reads what computeCost needs to price a host over the
// window [from, to), in the caller's (system) transaction: the rate periods
// and placements overlapping it, and the host's billed window clipped to
// it. to is at most now: whatever is still open ends there. The billed
// window runs from when luxd asked the provider for the host (a host that
// registered itself: its first hello) to when it was terminated, or is
// still open. Whatever of it no period covers comes back from computeCost
// as missing.
func loadHostCompute(ctx context.Context, tx pgx.Tx, hostID string, from, to time.Time) (hostCompute, error) {
	in := hostCompute{HostID: hostID, Now: to}
	if err := tx.QueryRow(ctx, `SELECT greatest(coalesce(provision_requested_at, registered_at, created_at), $2),
			least(terminated_at, $3)
			FROM hosts WHERE id = $1`, hostID, from, to).Scan(&in.From, &in.To); err != nil {
		return in, err
	}
	rows, err := tx.Query(ctx, `SELECT valid_from, valid_to, per_hour::text, currency, cap_cpus, cap_memory, source
		FROM host_rates WHERE host_id = $1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
		ORDER BY valid_from`, hostID, from, to)
	if err != nil {
		return in, err
	}
	if in.Rates, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ratePeriod]); err != nil {
		return in, err
	}
	rows, err = tx.Query(ctx, `SELECT id, run_id, epoch, coalesce((resources->>'cpus')::float8, 0),
			coalesce((resources->>'memory')::int8, 0), created_at, ended_at
		FROM placements WHERE host_id = $1 AND created_at < $3 AND (ended_at IS NULL OR ended_at > $2)
		ORDER BY created_at, id`, hostID, from, to)
	if err != nil {
		return in, err
	}
	in.Placements, err = pgx.CollectRows(rows, pgx.RowToStructByPos[placementWindow])
	return in, err
}
