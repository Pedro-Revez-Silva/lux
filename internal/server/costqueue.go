package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// The cost queue (docs/costs.md, section 5): Runs whose costs are due wait
// in cost_pending, queued by their state changes (setRunState) and by the
// cost tick; every luxd drains it. Each claim evaluates compute and plugins.

// Cost defaults (luxd's [costs] every, drain_every, batch).
const (
	DefaultCostsEvery      = 2 * time.Minute
	DefaultCostsDrainEvery = 2 * time.Second
	DefaultCostsBatch      = 1000
)

// costChunk Runs are written per transaction (a variable for tests).
var costChunk = 100

const (
	// costClaim is how long a drainer holds the Runs it claimed: a luxd that
	// dies mid-drain leaves claims that expire, and another takes them.
	costClaim = 2 * time.Minute
	// costSettleWake: how long the drainer lets Run events gather after one
	// wakes it. Every Run event wakes it (activity, sessions, input), not
	// only the state changes that queue costs, so this is also the floor
	// between claims while events flow: about one a second per luxd, not
	// five, for a state change's costs shown a second later.
	costSettleWake = time.Second
	// costTickSlack: the tick is tried this long after its bucket starts
	// (at most a tenth of costs.every).
	costTickSlack = time.Second
	// Compute's price retry is independent of plugin retry and settlement.
	costRetryMax = time.Hour
	costGiveUp   = 7 * 24 * time.Hour
	// Spot history can arrive after multiple successful cost evaluations.
	costSpotSettle = 24 * time.Hour
)

// CostsConfig: the cost tick and drainer. Not Enabled: neither runs (state
// changes still queue their Runs, which wait for a luxd that has it on).
type CostsConfig struct {
	Enabled bool
	// Every is the tick: every live Run is queued once per bucket of it.
	Every time.Duration
	// DrainEvery is how often the queue is polled when no Run event wakes
	// the drainer first.
	DrainEvery time.Duration
	// Batch is how many Runs one drain claims.
	Batch int
	// ComputeEC2 enables provider price lookups; PricesRefresh controls the
	// on-demand cache lifetime. Prices is keyed by provider, not by host.
	ComputeEC2    bool
	PricesRefresh time.Duration
	Prices        map[string]PriceProvider
	Plugins       []CostPluginConfig
	Settle        []time.Duration
	SettleGiveUp  time.Duration
	Backoff       time.Duration
	BackoffMax    time.Duration
	DescribeEvery time.Duration
}

// costLoop ticks and drains, when costs are enabled. The drainer wakes at
// every Run event (a state change queues its Run in the same transaction
// that writes the event), and every drain_every in case one is missed.
func (s *Server) costLoop(ctx context.Context) {
	c := s.cfg.Costs
	if !c.Enabled {
		return
	}
	s.initCostPlugins()
	var nextTick time.Time
	if len(s.plugins) > 0 {
		go func() {
			for ctx.Err() == nil {
				s.describeCostPlugins(ctx)
				wait(ctx, nil, c.DescribeEvery)
			}
		}()
	}
	for ctx.Err() == nil {
		woken := s.wakeups.next("")
		if !time.Now().Before(nextTick) {
			nextTick = s.tryCostTick(ctx, nextTick)
		}
		if err := s.pollDueCostSources(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("costs: poll due sources", "err", err)
		}
		n, err := s.drainCosts(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("costs: drain", "err", err)
		}
		if err == nil && n == c.Batch {
			continue // more may be due
		}
		untilTick := time.Until(nextTick)
		if untilTick <= 0 {
			untilTick = c.DrainEvery // the tick failed: again after drain_every
		}
		wait(ctx, woken, min(c.DrainEvery, untilTick))
		wait(ctx, nil, min(c.DrainEvery, costSettleWake))
	}
}

// tryCostTick runs the tick, and returns when to try it next: at the next
// bucket, or, if it failed, at the next pass (the same nextTick), still
// within its bucket.
func (s *Server) tryCostTick(ctx context.Context, nextTick time.Time) time.Time {
	if _, err := s.costTick(ctx); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("costs: tick", "err", err)
		}
		return nextTick
	}
	return nextCostTick(time.Now(), s.cfg.Costs.Every)
}

// nextCostTick is when to try the tick after one at now: the start of the
// next bucket of every (the buckets cost_ticks counts, whole multiples
// since the Unix epoch), a little after it so that this luxd's clock
// running ahead of the database's doesn't land it in the bucket before.
// Timed from the tick itself, it would drift across buckets and skip one
// now and then.
func nextCostTick(now time.Time, every time.Duration) time.Time {
	epoch := time.Unix(0, 0)
	return epoch.Add(now.Sub(epoch).Truncate(every) + every + min(costTickSlack, every/10))
}

// costTick queues live Runs, due configured sources and terminal Runs missing
// a configured plugin source, once per costs.every bucket. It reports whether
// this luxd won the bucket.
func (s *Server) costTick(ctx context.Context) (bool, error) {
	plugins := make([]string, 0, len(s.cfg.Costs.Plugins))
	for _, p := range s.cfg.Costs.Plugins {
		plugins = append(plugins, p.Name)
	}
	var won bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO cost_ticks (tick_at)
				VALUES (to_timestamp(floor(extract(epoch FROM now())::float8 / $1::float8) * $1::float8))
			ON CONFLICT DO NOTHING RETURNING true`, s.cfg.Costs.Every.Seconds()).Scan(&won)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The Runs first, in id order, then their queue rows in the same
		// order: a state change holds its Run when it queues it, and a
		// drain write takes both in that order too. A Run that is busy is
		// skipped: it is changing state, which queues it.
		if _, err := tx.Exec(ctx, `WITH due AS (
				SELECT id FROM runs WHERE id IN (
					SELECT id FROM runs WHERE state IN ('scheduled', 'starting', 'running', 'stopping')
					UNION
					SELECT run_id FROM cost_sources WHERE status <> 'final' AND next_at <= now()
						AND (source = 'compute' OR source = ANY($1))
					UNION
					SELECT r.id FROM runs r CROSS JOIN unnest($1::text[]) AS plugin(source)
						WHERE r.state IN ('succeeded', 'failed', 'cancelled')
						AND NOT EXISTS (SELECT 1 FROM cost_sources c WHERE c.run_id = r.id AND c.source = plugin.source))
				ORDER BY id FOR KEY SHARE SKIP LOCKED)
			INSERT INTO cost_pending (run_id, due_at, reason)
				SELECT id, now(), 'tick' FROM due ORDER BY id
			ON CONFLICT (run_id) DO UPDATE SET due_at = least(cost_pending.due_at, EXCLUDED.due_at)`, plugins); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM cost_ticks WHERE tick_at < now() - interval '1 day'`)
		return err
	})
	return won && err == nil, err
}

// pollDueCostSources queues backoffs and settlement attempts between ticks.
// Removed plugins have no configured name, so their source rows stay quiet.
func (s *Server) pollDueCostSources(ctx context.Context) error {
	plugins := make([]string, 0, len(s.cfg.Costs.Plugins))
	for _, p := range s.cfg.Costs.Plugins {
		plugins = append(plugins, p.Name)
	}
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH due AS (
			SELECT id FROM runs WHERE id IN (
				SELECT run_id FROM cost_sources WHERE status <> 'final' AND next_at <= now()
					AND (source = 'compute' OR source = ANY($1)))
			ORDER BY id FOR KEY SHARE SKIP LOCKED)
			INSERT INTO cost_pending (run_id, due_at, reason)
				SELECT id, now(), 'retry' FROM due ORDER BY id
			ON CONFLICT (run_id) DO UPDATE SET due_at = least(cost_pending.due_at, EXCLUDED.due_at)`, plugins)
		return err
	})
}

// claimCosts claims up to costs.batch due Runs for this luxd, in a
// transaction of its own that commits at once: no row stays locked while
// the Runs are worked. A claim that expired is taken again.
func (s *Server) claimCosts(ctx context.Context) ([]string, error) {
	var runs []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE cost_pending SET claimed_by = $1, claimed_until = now() + $2::interval
			WHERE run_id IN (SELECT run_id FROM cost_pending
				WHERE due_at <= now() AND (claimed_until IS NULL OR claimed_until < now())
				ORDER BY due_at LIMIT $3 FOR UPDATE SKIP LOCKED)
			RETURNING run_id`, s.id, interval(costClaim), s.cfg.Costs.Batch)
		if err != nil {
			return err
		}
		runs, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return runs, err
}

// drainCosts claims the due Runs, works out their compute cost from one
// read, and writes each Run's lines and source state in short transactions,
// one per chunk. It returns how many Runs it claimed.
func (s *Server) drainCosts(ctx context.Context) (int, error) {
	runs, err := s.claimCosts(ctx)
	if err != nil || len(runs) == 0 {
		return 0, err
	}
	// Leave room before the lease expires for writes and for another drainer.
	ctx, cancel := context.WithTimeout(ctx, costClaim/2)
	defer cancel()
	slices.Sort(runs) // each chunk locks its Runs in id order
	var evals map[string]*computeEval
	var pluginRuns map[string]pluginRun
	var pluginDue map[string]map[string]bool
	var now time.Time
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		evals, now, err = evaluateCompute(ctx, tx, runs)
		if err == nil && len(s.cfg.Costs.Plugins) > 0 {
			pluginRuns, err = loadPluginRuns(ctx, tx, runs)
			if err == nil {
				pluginDue, err = loadPluginDue(ctx, tx, runs, now)
			}
		}
		return err
	})
	if err != nil {
		// Nothing was read: every claimed Run is tried again later.
		s.releaseCosts(context.WithoutCancel(ctx), runs)
		return len(runs), err
	}
	// Compute is committed before any external request. Plugin chunks write
	// independently, so one slow source cannot hold up another source.
	var first error
	valid := map[string]bool{}
	for chunk := range slices.Chunk(runs, costChunk) {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.writeComputeChunk(ctx, tx, chunk, evals, now)
		})
		if err != nil {
			s.releaseCosts(context.WithoutCancel(ctx), chunk)
			first = cmp.Or(first, err)
		} else {
			for _, id := range chunk {
				valid[id] = true
			}
		}
	}
	for id := range pluginRuns {
		if !valid[id] {
			delete(pluginRuns, id)
		}
	}
	first = cmp.Or(first, s.reportCostPlugins(ctx, pluginRuns, pluginDue, evals, now))
	if ctx.Err() != nil {
		s.releaseCosts(context.WithoutCancel(ctx), runs)
		return len(runs), cmp.Or(first, ctx.Err())
	}
	for chunk := range slices.Chunk(runs, costChunk) {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, chunk); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM cost_pending WHERE run_id = ANY($1) AND claimed_by = $2`, chunk, s.id)
			return err
		})
		if err != nil {
			s.releaseCosts(context.WithoutCancel(ctx), chunk)
			first = cmp.Or(first, err)
		}
	}
	return len(runs), first
}

func (s *Server) writeComputeChunk(ctx context.Context, tx pgx.Tx, runs []string, evals map[string]*computeEval, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, runs); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT run_id FROM cost_pending WHERE run_id = ANY($1) AND claimed_by = $2 ORDER BY run_id FOR UPDATE`, runs, s.id)
	if err != nil {
		return err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range held {
		if err := s.writeCompute(ctx, tx, id, evals[id], now); err != nil {
			return err
		}
	}
	return nil
}

// releaseCosts gives up this luxd's claims on runs after a failure, and
// moves them a tick later.
func (s *Server) releaseCosts(ctx context.Context, runs []string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE cost_pending SET claimed_by = NULL, claimed_until = NULL, due_at = now() + $3::interval, reason = 'retry'
			WHERE run_id = ANY($1) AND claimed_by = $2`, runs, s.id, interval(s.cfg.Costs.Every))
		return err
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("costs: release claims", "err", err)
	}
}

// computeEval is one Run's compute cost as evaluated from one read.
type computeEval struct {
	TenantID   string
	State      string
	FinishedAt *time.Time
	StateAt    time.Time
	Lines      []costReport
	// Missing: some of its time on a provider's host has no rate. (A static
	// host's time with no price is unbilled, not missing.)
	Missing []string
	// Open: a placement has not ended yet.
	Open bool
	// Spot: at least one placement ran on a spot host.
	Spot bool
	// SpotOpen: a spot host used by this Run is still alive.
	SpotOpen bool
	// Latest termination of a spot host used by this Run, if any.
	SpotEnded *time.Time
	// SpotChecked: earliest successful recent-history check across its spot hosts.
	SpotChecked   *time.Time
	SpotUnchecked bool
	// Err: a host it ran on could not be priced (overlapping periods).
	Err error
}

// final: nothing about the Run's compute can change any more. Only a
// terminal Run's; a stopped or lost one may still be resumed.
func (e *computeEval) final() bool {
	return terminal(e.State) && !e.Open && len(e.Missing) == 0 && e.Err == nil
}

// costHost is what a line says about the host a placement ran on.
type costHost struct {
	Provider bool // launched by a provider (it prices it); false: static
	Type     string
	Market   string
	Zone     string
}

// item names a line: the instance type, with :spot for spot. A static
// host has no instance type: its time is the item "static".
func (h costHost) item() string {
	switch {
	case !h.Provider:
		return "static"
	case h.Type == "":
		return "unknown"
	case h.Market == "spot":
		return h.Type + ":spot"
	}
	return h.Type
}

// evaluateCompute prices the claimed Runs' placements, in a read-only
// system transaction. Each host is loaded once, over the window its
// claimed placements span (to now while one is live), and priced with every
// placement on it in that window, since they share it.
func evaluateCompute(ctx context.Context, tx pgx.Tx, runs []string) (map[string]*computeEval, time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, now, err
	}
	evals := map[string]*computeEval{}
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, state, finished_at, updated_at FROM runs WHERE id = ANY($1)`, runs)
	if err != nil {
		return nil, now, err
	}
	var id string
	var e computeEval
	if _, err := pgx.ForEachRow(rows, []any{&id, &e.TenantID, &e.State, &e.FinishedAt, &e.StateAt}, func() error {
		c := e
		evals[id] = &c
		return nil
	}); err != nil {
		return nil, now, err
	}

	// The window each host is loaded over.
	type window struct{ from, to time.Time }
	windows := map[string]*window{}
	rows, err = tx.Query(ctx, `SELECT run_id, host_id, created_at, coalesce(ended_at, $2), ended_at IS NULL
		FROM placements WHERE run_id = ANY($1)`, runs, now)
	if err != nil {
		return nil, now, err
	}
	var host string
	var from, to time.Time
	var open bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &host, &from, &to, &open}, func() error {
		if e := evals[id]; e != nil && open {
			e.Open = true
		}
		if w := windows[host]; w == nil {
			windows[host] = &window{from, to}
		} else {
			w.from, w.to = minTime(w.from, from), maxTime(w.to, to)
		}
		return nil
	}); err != nil {
		return nil, now, err
	}
	hostIDs := slices.Sorted(maps.Keys(windows))
	hosts := map[string]costHost{}
	rows, err = tx.Query(ctx, `SELECT h.id, h.provision_requested_at IS NOT NULL,
			coalesce(h.instance_type, h.launch_template->>'instanceType', ''), coalesce(h.market, ''), coalesce(h.zone, ''),
			h.terminated_at, r.checked_at
		FROM hosts h LEFT JOIN spot_history_refresh r ON r.host_id = h.id WHERE h.id = ANY($1)`, hostIDs)
	if err != nil {
		return nil, now, err
	}
	var h costHost
	var hostEnded *time.Time
	var hostChecked *time.Time
	ended := map[string]*time.Time{}
	checked := map[string]*time.Time{}
	if _, err := pgx.ForEachRow(rows, []any{&host, &h.Provider, &h.Type, &h.Market, &h.Zone, &hostEnded, &hostChecked}, func() error {
		hosts[host] = h
		ended[host] = hostEnded
		checked[host] = hostChecked
		return nil
	}); err != nil {
		return nil, now, err
	}

	b := newComputeLines(now)
	for _, hostID := range hostIDs {
		w := windows[hostID]
		in, err := loadHostCompute(ctx, tx, hostID, w.from, w.to)
		if err != nil {
			return nil, now, err
		}
		if hosts[hostID].Market == "spot" {
			for _, p := range in.Placements {
				if e := evals[p.RunID]; e != nil {
					e.Spot = true
					if stop := ended[hostID]; stop == nil {
						e.SpotOpen = true
					} else if e.SpotEnded == nil || stop.After(*e.SpotEnded) {
						e.SpotEnded = stop
					}
					if at := checked[hostID]; at == nil {
						e.SpotUnchecked = true
					} else if e.SpotChecked == nil || at.Before(*e.SpotChecked) {
						e.SpotChecked = at
					}
				}
			}
		}
		res, err := computeCost(in)
		if err != nil {
			// A host that cannot be priced (overlapping periods): its Runs
			// are incomplete, the others go on.
			for _, p := range in.Placements {
				if e := evals[p.RunID]; e != nil {
					e.Err = err
				}
			}
			continue
		}
		b.add(evals, in, res, hosts[hostID])
	}
	for id, e := range evals {
		e.Lines = b.lines(id)
		slices.Sort(e.Missing)
	}
	return evals, now, nil
}

// computeLines gathers placements' amounts into one line per Run, item and
// currency.
type computeLines struct {
	now   time.Time
	byRun map[string]map[[2]string]*computeLine // run → (item, currency) →
}

type computeLine struct {
	amount     *big.Rat
	from, to   time.Time
	placements []map[string]any
	missing    bool
}

func newComputeLines(now time.Time) *computeLines {
	return &computeLines{now: now, byRun: map[string]map[[2]string]*computeLine{}}
}

// add takes the claimed Runs' placements out of one host's result.
func (b *computeLines) add(evals map[string]*computeEval, in hostCompute, res computeResult, h costHost) {
	// Each placement's last priced period, per currency: its details show
	// that period's rate and its share against it.
	last := make([]map[string]*ratePeriod, len(in.Placements))
	for _, piece := range res.Pieces {
		if piece.Rate == nil {
			continue
		}
		for _, i := range piece.Live {
			if last[i] == nil {
				last[i] = map[string]*ratePeriod{}
			}
			last[i][piece.Rate.Currency] = piece.Rate
		}
	}
	for i, p := range in.Placements {
		e := evals[p.RunID]
		if e == nil {
			continue // another Run's: priced only for its share of the host
		}
		pc := res.Placements[i]
		missing := h.Provider && len(pc.Missing) > 0
		if missing {
			for _, m := range pc.Missing {
				e.Missing = append(e.Missing, fmt.Sprintf("host %s has no rate from %s to %s",
					in.HostID, m.From.UTC().Format(time.RFC3339), m.To.UTC().Format(time.RFC3339)))
			}
		}
		to := b.now
		if p.To != nil {
			to = *p.To
		}
		for currency, amount := range pc.Amounts {
			r := last[i][currency]
			d := map[string]any{"epoch": p.Epoch, "hostId": in.HostID, "from": p.From, "to": p.To,
				"cpus": p.CPUs, "memory": p.Memory, "amount": moneyString(amount)}
			if r != nil {
				sh, _ := share(p, r).Float64()
				d["share"], d["ratePerHour"] = sh, moneyString(mustRat(r.PerHour))
			}
			if h.Market != "" {
				d["market"] = h.Market
			}
			if h.Zone != "" {
				d["zone"] = h.Zone
			}
			if missing {
				d["missingRate"] = true
			}
			if b.byRun[p.RunID] == nil {
				b.byRun[p.RunID] = map[[2]string]*computeLine{}
			}
			k := [2]string{h.item(), currency}
			l := b.byRun[p.RunID][k]
			if l == nil {
				l = &computeLine{amount: new(big.Rat), from: p.From, to: to}
				b.byRun[p.RunID][k] = l
			}
			l.amount.Add(l.amount, amount)
			l.from, l.to = minTime(l.from, p.From), maxTime(l.to, to)
			l.placements = append(l.placements, d)
			l.missing = l.missing || missing
		}
	}
}

// lines is one Run's lines, by item. An item priced in more than one
// currency (hosts priced differently) is one line per currency, the
// currency appended to its item, since a line's key is its item.
func (b *computeLines) lines(runID string) []costReport {
	perItem := map[string]int{}
	for k := range b.byRun[runID] {
		perItem[k[0]]++
	}
	var out []costReport
	for k, l := range b.byRun[runID] {
		item := k[0]
		if perItem[item] > 1 {
			item += ":" + k[1]
		}
		slices.SortFunc(l.placements, func(a, b map[string]any) int {
			return cmp.Or(a["from"].(time.Time).Compare(b["from"].(time.Time)), cmp.Compare(a["epoch"].(int), b["epoch"].(int)))
		})
		details := map[string]any{"placements": l.placements}
		if l.missing {
			details["missingRate"] = true
		}
		out = append(out, costReport{Family: "compute", Item: item, Amount: moneyString(l.amount), Currency: k[1],
			From: l.from, To: l.to, Details: details})
	}
	slices.SortFunc(out, func(a, b costReport) int { return strings.Compare(a.Item, b.Item) })
	return out
}

// writeCosts writes one chunk of claimed Runs, those whose claim this luxd
// still holds: a state change since (which frees the claim) makes a
// result stale, and the Run is evaluated again. It locks the chunk's Runs,
// then their queue rows, each in id order: the order a state change takes
// them in (it holds its Run's row when it queues it), so the two wait for
// each other rather than deadlock.
func (s *Server) writeCosts(ctx context.Context, tx pgx.Tx, runs []string, evals map[string]*computeEval, now time.Time, results ...map[string]map[string]pluginAnswer) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, runs); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT run_id FROM cost_pending WHERE run_id = ANY($1) AND claimed_by = $2
		ORDER BY run_id FOR UPDATE`, runs, s.id)
	if err != nil {
		return err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range held {
		if err := s.writeCompute(ctx, tx, id, evals[id], now); err != nil {
			return err
		}
		if len(results) > 0 {
			for _, p := range s.cfg.Costs.Plugins {
				answer, due := results[0][p.Name][id]
				if !due {
					continue
				}
				if err := s.writePluginCost(ctx, tx, p, id, evals[id], answer, now); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM cost_pending WHERE run_id = $1 AND claimed_by = $2`, id, s.id); err != nil {
			return err
		}
	}
	return nil
}

// writeCompute stores one claimed Run's evaluation. On an error, its
// earlier lines stay.
func (s *Server) writeCompute(ctx context.Context, tx pgx.Tx, runID string, e *computeEval, now time.Time) error {
	if e == nil {
		// Defensive: a queued Run always has its runs row (cost_pending's
		// foreign key, and Runs are never deleted), so it is evaluated.
		_, err := tx.Exec(ctx, `DELETE FROM cost_pending WHERE run_id = $1`, runID)
		return err
	}
	final := e.final()
	var attempts int
	if terminal(e.State) {
		err := tx.QueryRow(ctx, `SELECT attempts FROM cost_sources WHERE run_id = $1 AND source = 'compute'`, runID).
			Scan(&attempts)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if terminal(e.State) && e.Spot {
		settleFrom := e.FinishedAt
		if e.SpotEnded != nil && (settleFrom == nil || e.SpotEnded.After(*settleFrom)) {
			settleFrom = e.SpotEnded
		}
		if e.SpotOpen || settleFrom == nil || now.Before(settleFrom.Add(costSpotSettle)) ||
			e.SpotUnchecked || e.SpotChecked == nil || e.SpotChecked.Before(settleFrom.Add(costSpotSettle)) {
			final = false
		}
	}
	status, lastError := "ok", ""
	switch {
	case final:
		status = "final"
	case e.Err != nil:
		status, lastError = "incomplete", e.Err.Error()
	case len(e.Missing) > 0:
		status, lastError = "incomplete", strings.Join(e.Missing, "; ")
	}
	// A terminal Run not final yet is tried again, backing off. A live Run
	// is queued by every tick anyway; a stopped or lost one stays quiet
	// until it is resumed.
	var nextAt *time.Time
	if terminal(e.State) && !final {
		attempts++
		if e.Spot && (e.SpotOpen || e.final()) || e.FinishedAt == nil || now.Sub(*e.FinishedAt) < costGiveUp {
			t := now.Add(min(s.cfg.Costs.Every<<min(attempts-1, 16), costRetryMax))
			nextAt = &t
		}
	} else {
		attempts = 0
	}
	if e.Err == nil {
		for i := range e.Lines {
			e.Lines[i].Final = final
		}
		if err := replaceCostLines(ctx, tx, e.TenantID, runID, "compute", e.Lines); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, answered_at, attempts, next_at, settles_left, last_error)
			VALUES ($1, $2, 'compute', $3, CASE WHEN $4 THEN now() END, $5, $6, $7, $8)
		ON CONFLICT (run_id, source) DO UPDATE SET status = EXCLUDED.status,
			answered_at = coalesce(EXCLUDED.answered_at, cost_sources.answered_at),
			attempts = EXCLUDED.attempts, next_at = EXCLUDED.next_at,
			settles_left = EXCLUDED.settles_left, last_error = EXCLUDED.last_error`,
		runID, e.TenantID, status, e.Err == nil, attempts, nextAt, nil, lastError); err != nil {
		return err
	}
	return nil
}

// resetCostFinality: a resumed Run is active again. Its final sources go
// back to ok and its lines to estimates, and no retry is pending (the
// tick queues it while it is live), in the resume's transaction (a
// tenant's scope, from the API: these are its own rows).
func resetCostFinality(ctx context.Context, tx pgx.Tx, runID string) error {
	if _, err := tx.Exec(ctx, `UPDATE cost_sources SET status = CASE WHEN status = 'final' THEN 'ok' ELSE status END,
			next_at = NULL, attempts = 0, settles_left = NULL
		WHERE run_id = $1`, runID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE cost_lines SET final = false WHERE run_id = $1 AND final`, runID)
	return err
}

func mustRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("not a decimal: " + s)
	}
	return r
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
