package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Market data: what each candidate instance type costs and how likely EC2
// is to have spot capacity for it, per availability zone of each pool,
// sampled every MarketConfig.Every into market_samples (docs/telemetry.md).
// It is collected only; nothing in lux reads it.

// DefaultMarketEvery is how often a candidate type is sampled.
const DefaultMarketEvery = time.Hour

// MarketConfig is [market].
type MarketConfig struct {
	// Every: how often each (provider, region, instance type) is sampled;
	// 0 is off.
	Every time.Duration
	// Candidates: instance types sampled for every pool besides its
	// template's instanceType and fallbackInstanceTypes.
	Candidates []string
}

// MarketProvider is a Provider that can say what a pool's template
// launches into, and score spot capacity there.
type MarketProvider interface {
	// MarketScope is the template's region and its instance types, without
	// calling the provider.
	MarketScope(template json.RawMessage) (region string, instanceTypes []string, err error)
	// Zones are the availability zones the template launches into.
	Zones(ctx context.Context, template json.RawMessage) ([]Zone, error)
	// PlacementScores are the provider's spot placement scores (1-10) for
	// one instance of instanceType, per zone id of region.
	PlacementScores(ctx context.Context, region, instanceType string) (map[string]int, error)
}

// Zone is an availability zone: its account-specific name (us-east-1a) and
// its id (use1-az1), which placement scores are keyed by.
type Zone struct{ Name, ID string }

// marketPassTimeout bounds one pass: the SDK retries a throttled call with
// backoff, never past it.
const marketPassTimeout = 2 * time.Minute

// marketLoop samples the market while this luxd holds the provisioner
// lease (sampleMarket). It checks every minute which types are due.
func (s *Server) marketLoop(ctx context.Context) {
	if s.cfg.Market.Every <= 0 || len(s.cfg.Providers) == 0 {
		return
	}
	for ctx.Err() == nil {
		passCtx, cancel := context.WithTimeout(ctx, marketPassTimeout)
		if err := s.sampleMarketPass(passCtx); err != nil && ctx.Err() == nil {
			s.log.Warn("market: sample", "err", err)
		}
		cancel()
		wait(ctx, nil, min(s.cfg.Market.Every, time.Minute))
	}
}

// sampleMarketPass is one pass: the provider's refusals (throttling, a
// missing permission) are logged as one warning and do not fail it; the
// error is the database's or the context's.
func (s *Server) sampleMarketPass(ctx context.Context) error {
	problems, err := s.sampleMarket(ctx)
	if perr := problems.err(); perr != nil && ctx.Err() == nil {
		s.log.Warn("market: some figures not sampled; stored without them", "err", perr)
	}
	return err
}

// holdsProvisionLease: this luxd holds the provisioner lease now. Only
// read: the provisioner takes and renews it.
func (s *Server) holdsProvisionLease(ctx context.Context) (bool, error) {
	var held bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE name = 'provisioner' AND holder = $1 AND expires_at >= now())`,
			instanceID).Scan(&held)
	})
	return held, err
}

// marketTarget is one provider and region to sample: its candidate types,
// and the pools (ids and templates) whose zones it is sampled in.
type marketTarget struct {
	provider, region string
	mp               MarketProvider
	types            []string
	pools            []string
	templates        []json.RawMessage
	zones            []Zone
}

// marketProblems gathers a pass's failures by what failed, so a pass logs
// one warning however many calls failed.
type marketProblems map[string][]string

func (m marketProblems) add(what string, err error) {
	if err != nil {
		m[what] = append(m[what], err.Error())
	}
}

func (m marketProblems) err() error {
	if len(m) == 0 {
		return nil
	}
	var parts []string
	for _, what := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s: %d failed, first: %s", what, len(m[what]), m[what][0]))
	}
	return errors.New(strings.Join(parts, "; "))
}

// sampleMarket writes a market sample for every candidate type of every
// live pool of a MarketProvider that is due (marketDue), in each zone its
// pools launch into. Each figure is fetched on its own: one that fails is
// NULL in the row, never a reason to drop the others. A type is marked
// attempted just before its own calls, whatever their outcome, so a
// refused call is not repeated each minute and a type the pass never
// reached stays due. The lease is checked again before each write; once it
// is lost the pass stops.
func (s *Server) sampleMarket(ctx context.Context) (marketProblems, error) {
	problems := marketProblems{}
	if held, err := s.holdsProvisionLease(ctx); err != nil || !held {
		return problems, err
	}
	targets, err := s.marketTargets(ctx, problems)
	if err != nil {
		return problems, err
	}
	err = s.sampleMarketTargets(ctx, targets, problems)
	if errors.Is(err, errMarketLeaseLost) {
		err = nil
	}
	return problems, err
}

// errMarketLeaseLost stops a pass whose luxd no longer holds the
// provisioner lease.
var errMarketLeaseLost = errors.New("market: provisioner lease lost")

// requireProvisionLease is errMarketLeaseLost unless this luxd still holds
// the lease, checked before a pass writes what the provider answered.
func (s *Server) requireProvisionLease(ctx context.Context) error {
	held, err := s.holdsProvisionLease(ctx)
	if err == nil && !held {
		err = errMarketLeaseLost
	}
	return err
}

// sampleMarketTargets samples each target's due types; the error is the
// database's, the context's, or errMarketLeaseLost.
func (s *Server) sampleMarketTargets(ctx context.Context, targets []*marketTarget, problems marketProblems) error {
	for _, t := range targets {
		due, err := s.marketDue(ctx, t)
		if err != nil {
			return err
		}
		if len(due) == 0 {
			continue
		}
		// The catalog with the samples: a new candidate is described at
		// once, and a failing describe is retried no more often than the
		// due types are.
		if err := s.refreshCatalog(ctx, t, problems); err != nil {
			return err
		}
		if t.zones, err = s.marketZones(ctx, t, problems); err != nil {
			return err
		}
		for _, it := range due {
			// A pass past its deadline marks nothing more: a type whose
			// calls would all fail on the dead context stays due.
			if err := ctx.Err(); err != nil {
				return err
			}
			s.markMarketAttempt(t, it)
			if err := s.sampleMarketType(ctx, t, it, problems); err != nil {
				return err
			}
		}
	}
	return nil
}

// marketTargets are the live pools' providers and regions, with their
// candidate types: the templates' and Candidates.
func (s *Server) marketTargets(ctx context.Context, problems marketProblems) ([]*marketTarget, error) {
	type pool struct {
		ID, Provider string
		Template     json.RawMessage
	}
	var pools []pool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, provider, template FROM pools WHERE provider <> 'static' AND NOT retired ORDER BY id`)
		if err != nil {
			return err
		}
		pools, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pool])
		return err
	})
	if err != nil {
		return nil, err
	}
	byKey := map[[2]string]*marketTarget{}
	var out []*marketTarget
	for _, p := range pools {
		mp, ok := s.cfg.Providers[p.Provider].(MarketProvider)
		if !ok {
			continue
		}
		region, types, err := mp.MarketScope(p.Template)
		if err == nil && region == "" {
			err = errors.New("no region")
		}
		if err != nil {
			problems.add("template", fmt.Errorf("pool %s: %w", p.ID, err))
			continue
		}
		key := [2]string{p.Provider, region}
		t := byKey[key]
		if t == nil {
			t = &marketTarget{provider: p.Provider, region: region, mp: mp}
			byKey[key] = t
			out = append(out, t)
		}
		t.pools, t.templates = append(t.pools, p.ID), append(t.templates, p.Template)
		for _, it := range append(types, s.cfg.Market.Candidates...) {
			if it != "" && !slices.Contains(t.types, it) {
				t.types = append(t.types, it)
			}
		}
	}
	return out, nil
}

// marketDue are t's types this process has not attempted within Every and
// that have no sample within it (another luxd's, before a lease moved).
// A minute's slack keeps a pass a little early from skipping a whole Every.
func (s *Server) marketDue(ctx context.Context, t *marketTarget) ([]string, error) {
	within := max(s.cfg.Market.Every-time.Minute, time.Minute)
	recent, err := s.marketStrings(ctx, `SELECT DISTINCT instance_type FROM market_samples
			WHERE provider = $1 AND region = $2 AND instance_type = ANY($3) AND sampled_at > now() - $4::interval`,
		t.provider, t.region, t.types, interval(within))
	if err != nil {
		return nil, err
	}
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	var due []string
	for _, it := range t.types {
		at, tried := s.marketTried[[3]string{t.provider, t.region, it}]
		if !slices.Contains(recent, it) && (!tried || time.Since(at) >= within) {
			due = append(due, it)
		}
	}
	return due, nil
}

func (s *Server) markMarketAttempt(t *marketTarget, it string) {
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	if s.marketTried == nil {
		s.marketTried = map[[3]string]time.Time{}
	}
	s.marketTried[[3]string{t.provider, t.region, it}] = time.Now()
}

// marketZones are the zones t's pools launch into, as the provider lists
// them; when it cannot, the zones their hosts were launched in (by name
// only: without the zone id no placement score can be matched to it).
func (s *Server) marketZones(ctx context.Context, t *marketTarget, problems marketProblems) ([]Zone, error) {
	var zones []Zone
	failed := false
	for _, tmpl := range t.templates {
		zs, err := t.mp.Zones(ctx, tmpl)
		problems.add("zones", err)
		failed = failed || err != nil
		for _, z := range zs {
			if !slices.Contains(zones, z) {
				zones = append(zones, z)
			}
		}
	}
	if !failed {
		return zones, nil
	}
	seen, err := s.marketStrings(ctx, `SELECT DISTINCT zone FROM hosts WHERE pool_id = ANY($1) AND zone IS NOT NULL AND zone <> '' ORDER BY zone`, t.pools)
	for _, name := range seen {
		if !slices.ContainsFunc(zones, func(z Zone) bool { return z.Name == name }) {
			zones = append(zones, Zone{Name: name})
		}
	}
	return zones, err
}

// marketStrings runs a one-column text query in the system scope.
func (s *Server) marketStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	var out []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return out, err
}

// spotWindow is how far back a spot price is looked for: EC2 answers the
// prices in effect in the window, so a price unchanged for longer is the
// window's first point.
const spotWindow = 24 * time.Hour

// sampleMarketType writes one row per zone of t for instance type it; the
// error is the database's, or errMarketLeaseLost.
func (s *Server) sampleMarketType(ctx context.Context, t *marketTarget, it string, problems marketProblems) error {
	onDemand, err := s.onDemandPrice(ctx, t.provider, t.region, it)
	problems.add("on-demand price", err)
	scores, err := t.mp.PlacementScores(ctx, t.region, it)
	problems.add("placement score", err)
	prices := s.cfg.Costs.Prices[t.provider]
	now := time.Now()
	var rows []marketRow
	for _, z := range t.zones {
		var spot *SpotRate
		if prices != nil {
			history, err := prices.SpotHistory(ctx, z.Name, it, now.Add(-spotWindow), now)
			problems.add("spot price", err)
			spot = latestSpot(history, now)
		}
		row := marketRow{zone: z, instanceType: it}
		if onDemand.Currency != "" && validPrice(onDemand.PerHour, onDemand.Currency) == nil {
			row.onDemand, row.currency = &onDemand.PerHour, onDemand.Currency
		}
		if spot != nil && (row.currency == "" || row.currency == spot.Currency) {
			p := moneyString(mustRat(spot.PerHour))
			row.spot, row.currency = &p, spot.Currency
		}
		if sc, ok := scores[z.ID]; ok && z.ID != "" {
			row.score = &sc
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.requireProvisionLease(ctx); err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.insertMarketRow(ctx, t, row); err != nil {
			return err
		}
	}
	return nil
}

type marketRow struct {
	zone           Zone
	instanceType   string
	spot, onDemand *string
	currency       string
	score          *int
}

func (s *Server) insertMarketRow(ctx context.Context, t *marketTarget, r marketRow) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO market_samples (provider, region, zone, zone_id, instance_type, sampled_at,
				spot_per_hour, on_demand_per_hour, currency, placement_score)
			VALUES ($1, $2, $3, nullif($4, ''), $5, clock_timestamp(), $6, $7, nullif($8, ''), $9)
			ON CONFLICT DO NOTHING`,
			t.provider, t.region, r.zone.Name, r.zone.ID, r.instanceType, r.spot, r.onDemand, r.currency, r.score)
		return err
	})
}

// latestSpot is history's newest valid price at or before now; nil if none.
func latestSpot(history []SpotRate, now time.Time) *SpotRate {
	var latest *SpotRate
	for i := range history {
		r := &history[i]
		if validPrice(r.PerHour, r.Currency) != nil || r.Currency == "" || r.At.After(now) || r.At.IsZero() {
			continue
		}
		if latest == nil || r.At.After(latest.At) {
			latest = r
		}
	}
	return latest
}
