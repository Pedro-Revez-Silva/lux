package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
)

// CostsConfig: the cost tick and drainer. Not Enabled: neither runs (state
// changes still queue their Runs, which wait for a luxd that has it on).
type CostsConfig struct {
	Enabled bool
	Hourly  time.Duration
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
		if err := s.updateHostHours(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("costs: host hours", "err", err)
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

// costTick queues live Runs, due configured sources and settled Runs missing
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
				SELECT id FROM runs WHERE state IN ('scheduled', 'starting', 'running', 'stopping')
				ORDER BY id FOR KEY SHARE SKIP LOCKED)
			INSERT INTO cost_pending (run_id, due_at, reason)
				SELECT id, now(), 'tick' FROM due ORDER BY id
			ON CONFLICT (run_id) DO UPDATE SET due_at = least(cost_pending.due_at, EXCLUDED.due_at)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `WITH due AS (
				SELECT r.id FROM runs r WHERE r.id IN (
					SELECT run_id FROM (
						SELECT DISTINCT c.run_id FROM cost_sources c
						WHERE c.status <> 'final' AND c.next_at <= now()
							AND (c.source = 'compute' OR c.source = ANY($1))
							AND NOT EXISTS (SELECT 1 FROM cost_pending p WHERE p.run_id = c.run_id)
						ORDER BY c.run_id LIMIT $2) AS retries
					UNION
					SELECT id FROM (
						SELECT missing.id FROM runs missing
						WHERE missing.state IN ('succeeded', 'failed', 'cancelled', 'stopped', 'lost')
							AND (EXISTS (SELECT 1 FROM unnest($1::text[]) AS plugin(source)
								WHERE NOT EXISTS (SELECT 1 FROM cost_sources c WHERE c.run_id = missing.id AND c.source = plugin.source))
								OR (missing.state IN ('succeeded', 'failed', 'cancelled')
									AND NOT EXISTS (SELECT 1 FROM cost_sources c WHERE c.run_id = missing.id AND c.source = 'compute')))
							AND NOT EXISTS (SELECT 1 FROM cost_pending p WHERE p.run_id = missing.id)
						ORDER BY missing.id LIMIT $2) AS missing_sources)
				AND NOT EXISTS (SELECT 1 FROM cost_pending p WHERE p.run_id = r.id)
				ORDER BY r.id FOR KEY SHARE OF r SKIP LOCKED)
			INSERT INTO cost_pending (run_id, due_at, reason)
				SELECT id, now(), 'tick' FROM due ORDER BY id
			ON CONFLICT (run_id) DO NOTHING`, plugins, s.cfg.Costs.Batch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM cost_ticks WHERE tick_at < now() - interval '1 day'`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM cost_hourly WHERE hour < now() - $1::interval`, interval(s.cfg.Costs.Hourly))
		if err != nil {
			return err
		}
		return nil
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
			SELECT r.id FROM runs r WHERE r.id IN (
				SELECT c.run_id FROM cost_sources c WHERE c.status <> 'final' AND c.next_at <= now()
				AND (c.source = 'compute' OR c.source = ANY($1)))
				AND NOT EXISTS (SELECT 1 FROM cost_pending p WHERE p.run_id = r.id)
			ORDER BY r.id LIMIT $2 FOR KEY SHARE OF r SKIP LOCKED)
			INSERT INTO cost_pending (run_id, due_at, reason)
				SELECT id, now(), 'retry' FROM due ORDER BY id
			ON CONFLICT (run_id) DO NOTHING`, plugins, s.cfg.Costs.Batch)
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
		evals, now, err = loadComputeRuns(ctx, tx, runs)
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
	failed, pluginErr := s.reportCostPlugins(ctx, pluginRuns, pluginDue, evals, now)
	first = cmp.Or(first, pluginErr)
	if ctx.Err() != nil {
		s.releaseCosts(context.WithoutCancel(ctx), runs)
		return len(runs), cmp.Or(first, ctx.Err())
	}
	for chunk := range slices.Chunk(runs, costChunk) {
		var done []string
		var retry []string
		for _, id := range chunk {
			if valid[id] && !failed[id] {
				done = append(done, id)
			} else {
				retry = append(retry, id)
			}
		}
		if len(retry) > 0 {
			s.releaseCosts(context.WithoutCancel(ctx), retry)
		}
		if len(done) == 0 {
			continue
		}
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, done); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM cost_pending WHERE run_id = ANY($1) AND claimed_by = $2`, done, s.id)
			return err
		})
		if err != nil {
			s.releaseCosts(context.WithoutCancel(ctx), done)
			first = cmp.Or(first, err)
		}
	}
	return len(runs), first
}

// lockCostHost serializes placement-window changes with compute snapshots on
// the same host. Callers acquire Run locks first, then host locks in id order.
func lockCostHost(ctx context.Context, tx pgx.Tx, hostID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cost-host:' || $1, 0))`, hostID)
	return err
}

func lockCostHosts(ctx context.Context, tx pgx.Tx, runs []string) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT host_id FROM placements WHERE run_id = ANY($1) ORDER BY host_id`, runs)
	if err != nil {
		return err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if err := lockCostHost(ctx, tx, host); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) writeComputeChunk(ctx context.Context, tx pgx.Tx, runs []string, evals map[string]*computeEval, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runs); err != nil {
		return err
	}
	if err := lockCostHosts(ctx, tx, runs); err != nil {
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

// computeEval holds a Run's compute cost and plugin metadata.
type computeEval struct {
	TenantID   string
	Hours      []computeHour
	State      string
	FinishedAt *time.Time
	StateAt    time.Time
	Lines      []costReport
	// Missing: some of its time on a provider's host has no rate. (A static
	// host's time with no price is unbilled, not missing.)
	Missing []string
	// Open: a placement has not ended yet.
	Open bool
}

// final: nothing about the Run's compute can change any more. Only a
// terminal Run's; a stopped or lost one may still be resumed.
func (e *computeEval) final() bool {
	return terminal(e.State) && !e.Open && len(e.Missing) == 0
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

// loadComputeRuns loads plugin metadata; pricing is reloaded under the Run lock.
func loadComputeRuns(ctx context.Context, tx pgx.Tx, runs []string) (map[string]*computeEval, time.Time, error) {
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
	_, err = pgx.ForEachRow(rows, []any{&id, &e.TenantID, &e.State, &e.FinishedAt, &e.StateAt}, func() error {
		copy := e
		evals[id] = &copy
		return nil
	})
	return evals, now, err
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
// result stale, and the Run is evaluated again. It locks Runs, hosts and
// queue rows in that order, each in id order, as the placement writers do.
func (s *Server) writeCosts(ctx context.Context, tx pgx.Tx, runs []string, evals map[string]*computeEval, now time.Time, results ...map[string]map[string]pluginAnswer) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runs); err != nil {
		return err
	}
	if err := lockCostHosts(ctx, tx, runs); err != nil {
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

// writeCompute refreshes only unfrozen placements and rebuilds derived rows in
// the same transaction as the locked Run and claim.
func (s *Server) writeCompute(ctx context.Context, tx pgx.Tx, runID string, _ *computeEval, _ time.Time) error {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return err
	}
	var e computeEval
	if err := tx.QueryRow(ctx, `SELECT tenant_id, state, finished_at, updated_at FROM runs WHERE id = $1 FOR KEY SHARE`, runID).
		Scan(&e.TenantID, &e.State, &e.FinishedAt, &e.StateAt); err != nil {
		return err
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT source FROM cost_sources WHERE run_id = $1 AND source = 'compute' FOR UPDATE`, runID).Scan(&locked); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT p.id, p.run_id, p.epoch, coalesce((p.resources->>'cpus')::float8, 0),
		coalesce((p.resources->>'memory')::int8, 0), p.created_at, p.ended_at,
		h.id, h.provision_requested_at IS NOT NULL,
		coalesce(h.instance_type, h.launch_template->>'instanceType', ''), coalesce(h.market, ''), coalesce(h.zone, ''),
		s.rate_from, s.per_hour::text, s.currency, s.cap_cpus, s.cap_memory, s.amount::text, s.priced_to, coalesce(s.finalized, false)
		FROM placements p JOIN hosts h ON h.id = p.host_id
		LEFT JOIN cost_placement_snapshots s ON s.placement_id = p.id
		WHERE p.run_id = $1 ORDER BY p.created_at, p.id`, runID)
	if err != nil {
		return err
	}
	type entry struct {
		p                 placementWindow
		hostID            string
		h                 costHost
		rateFrom          *time.Time
		perHour, currency *string
		capCPUs           *float64
		capMemory         *int64
		amount            *string
		pricedTo          *time.Time
		final             bool
	}
	var entries []entry
	for rows.Next() {
		var v entry
		if err := rows.Scan(&v.p.ID, &v.p.RunID, &v.p.Epoch, &v.p.CPUs, &v.p.Memory, &v.p.From, &v.p.To,
			&v.hostID, &v.h.Provider, &v.h.Type, &v.h.Market, &v.h.Zone,
			&v.rateFrom, &v.perHour, &v.currency, &v.capCPUs, &v.capMemory, &v.amount, &v.pricedTo, &v.final); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	b := newComputeLines(now)
	for i := range entries {
		v := &entries[i]
		if v.final {
			continue
		}
		end := now
		if v.p.To != nil {
			end = *v.p.To
		} else {
			e.Open = true
		}
		rate, err := resolvePlacementRate(ctx, tx, v.hostID, v.p.From)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !hasCapacity(rate)) {
			e.Missing = append(e.Missing, fmt.Sprintf("placement %s on host %s has no rate", v.p.ID, v.hostID))
			if _, err := tx.Exec(ctx, `INSERT INTO cost_placement_snapshots (placement_id, run_id, tenant_id, host_id)
				VALUES ($1,$2,$3,$4) ON CONFLICT (placement_id) DO UPDATE SET
				rate_from=NULL, per_hour=NULL, currency=NULL, cap_cpus=NULL, cap_memory=NULL,
				amount=NULL, priced_to=NULL WHERE NOT cost_placement_snapshots.finalized`, v.p.ID, runID, e.TenantID, v.hostID); err != nil {
				return err
			}
			v.amount, v.perHour, v.currency = nil, nil, nil
			continue
		}
		if err != nil {
			return err
		}
		amount, err := pricePlacementSnapshot(ctx, tx, v.p, v.hostID, rate, end)
		if err != nil {
			return err
		}
		a := moneyString(amount)
		v.amount, v.perHour, v.currency = &a, &rate.PerHour, &rate.Currency
		v.capCPUs, v.capMemory, v.rateFrom, v.pricedTo = &rate.CapCPUs, &rate.CapMemory, &rate.From, &end
		v.final = v.p.To != nil
		if _, err := tx.Exec(ctx, `INSERT INTO cost_placement_snapshots
			(placement_id, run_id, tenant_id, host_id, rate_from, per_hour, currency, cap_cpus, cap_memory, amount, priced_to, finalized)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,$10::numeric,$11,$12)
			ON CONFLICT (placement_id) DO UPDATE SET rate_from=EXCLUDED.rate_from, per_hour=EXCLUDED.per_hour,
			currency=EXCLUDED.currency, cap_cpus=EXCLUDED.cap_cpus, cap_memory=EXCLUDED.cap_memory,
			amount=EXCLUDED.amount, priced_to=EXCLUDED.priced_to, finalized=EXCLUDED.finalized
			WHERE NOT cost_placement_snapshots.finalized`, v.p.ID, runID, e.TenantID, v.hostID,
			rate.From, rate.PerHour, rate.Currency, rate.CapCPUs, rate.CapMemory, a, end, v.final); err != nil {
			return err
		}
	}
	// Reconstruct hourly allocations using the frozen rate and the current
	// host occupancy; finalized amounts are never recalculated.
	for _, v := range entries {
		if v.p.To == nil {
			e.Open = true
		}
		if !v.final && (v.amount == nil || v.currency == nil) && !slices.ContainsFunc(e.Missing, func(m string) bool { return strings.Contains(m, v.p.ID) }) {
			e.Missing = append(e.Missing, fmt.Sprintf("placement %s on host %s has no rate", v.p.ID, v.hostID))
		}
		if !v.final && v.p.To != nil {
			e.Missing = append(e.Missing, fmt.Sprintf("placement %s on host %s is not finalized", v.p.ID, v.hostID))
		}
		if v.amount == nil || v.currency == nil {
			continue
		}
		end := now
		if v.pricedTo != nil {
			end = *v.pricedTo
		}
		amount := mustRat(*v.amount)
		r := ratePeriod{PerHour: *v.perHour, Currency: *v.currency, CapCPUs: *v.capCPUs, CapMemory: *v.capMemory}
		sh, _ := share(v.p, &r).Float64()
		d := map[string]any{"epoch": v.p.Epoch, "hostId": v.hostID, "from": v.p.From, "to": v.p.To,
			"cpus": v.p.CPUs, "memory": v.p.Memory, "amount": *v.amount,
			"share": sh, "ratePerHour": *v.perHour, "finalized": v.final}
		if v.h.Market != "" {
			d["market"] = v.h.Market
		}
		if v.h.Zone != "" {
			d["zone"] = v.h.Zone
		}
		if b.byRun[runID] == nil {
			b.byRun[runID] = map[[2]string]*computeLine{}
		}
		k := [2]string{v.h.item(), *v.currency}
		l := b.byRun[runID][k]
		if l == nil {
			l = &computeLine{amount: new(big.Rat), from: v.p.From, to: end}
			b.byRun[runID][k] = l
		}
		l.amount.Add(l.amount, amount)
		l.from, l.to = minTime(l.from, v.p.From), maxTime(l.to, end)
		l.placements = append(l.placements, d)
		allocateCostHours(v.p.From, end, amount, func(hour time.Time, allocated *big.Rat) {
			e.Hours = append(e.Hours, computeHour{hour, v.hostID, *v.currency, allocated})
		})
	}
	e.Lines = b.lines(runID)
	if len(e.Missing) > 0 {
		for i := range e.Lines {
			e.Lines[i].Details["missingRate"] = true
		}
	}
	final := e.final()
	var attempts int
	if terminal(e.State) {
		if err := tx.QueryRow(ctx, `SELECT attempts FROM cost_sources WHERE run_id = $1 AND source = 'compute'`, runID).Scan(&attempts); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	status, lastError := "ok", ""
	if final {
		status = "final"
	} else if len(e.Missing) > 0 {
		status, lastError = "incomplete", strings.Join(e.Missing, "; ")
	}
	var nextAt *time.Time
	if terminal(e.State) && !final {
		attempts++
		t := now.Add(min(s.cfg.Costs.Every<<min(attempts-1, 16), costRetryMax))
		nextAt = &t
	} else {
		attempts = 0
	}
	for i := range e.Lines {
		e.Lines[i].Final = final
	}
	if err := replaceCostLines(ctx, tx, e.TenantID, runID, "compute", e.Lines); err != nil {
		return err
	}
	if err := replaceComputeHours(ctx, tx, e.TenantID, runID, e.Hours, s.cfg.Costs.Hourly); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO cost_sources (run_id, tenant_id, source, status, answered_at, attempts, next_at, settles_left, last_error)
		VALUES ($1,$2,'compute',$3,now(),$4,$5,NULL,$6)
		ON CONFLICT (run_id, source) DO UPDATE SET status=EXCLUDED.status, answered_at=EXCLUDED.answered_at,
		attempts=EXCLUDED.attempts, next_at=EXCLUDED.next_at, settles_left=NULL, last_error=EXCLUDED.last_error`,
		runID, e.TenantID, status, attempts, nextAt, lastError)
	return err
}

// resolvePlacementRate prefers an observation covering the placement start,
// then the best known matching price. Public prices can cross tenant boundaries;
// capacity always belongs to the placement's host.
func resolvePlacementRate(ctx context.Context, tx pgx.Tx, hostID string, start time.Time) (ratePeriod, error) {
	var rate ratePeriod
	err := tx.QueryRow(ctx, `WITH target AS (
			SELECT h.id, p.provider IS NOT NULL AS has_pool,
				coalesce(p.provider, CASE WHEN h.provision_requested_at IS NULL THEN 'static' ELSE 'ec2' END) AS provider,
				coalesce(h.market, '') AS market, coalesce(h.instance_type, h.launch_template->>'instanceType', '') AS kind,
				coalesce(h.zone, '') AS zone, coalesce(h.launch_template->>'region', '') AS region,
				coalesce((h.capacity->>'cpus')::float8, 0) AS cpus,
				coalesce((h.capacity->>'memory')::int8, 0) AS memory
			FROM hosts h LEFT JOIN pools p ON p.name = h.pool AND p.tenant_id IS NOT DISTINCT FROM h.tenant_id
			WHERE h.id = $1
		), own_capacity AS (
			SELECT r.cap_cpus, r.cap_memory FROM host_rates r JOIN target t ON r.host_id = t.id
			WHERE r.cap_cpus > 0 OR r.cap_memory > 0
			ORDER BY (r.valid_from <= $2) DESC, r.valid_from DESC LIMIT 1
		), candidates AS (
			SELECT r.valid_from, r.valid_to, r.per_hour, r.currency, r.source,
				CASE WHEN h.id = t.id THEN r.cap_cpus ELSE coalesce(c.cap_cpus, t.cpus) END AS cap_cpus,
				CASE WHEN h.id = t.id THEN r.cap_memory ELSE coalesce(c.cap_memory, t.memory) END AS cap_memory,
				h.id = t.id AS own, r.valid_from <= $2 AND (r.valid_to IS NULL OR r.valid_to > $2) AS covering
			FROM target t JOIN hosts h ON true
			LEFT JOIN pools p ON p.name = h.pool AND p.tenant_id IS NOT DISTINCT FROM h.tenant_id
			JOIN host_rates r ON r.host_id = h.id
			LEFT JOIN own_capacity c ON true
			WHERE (h.id = t.id AND t.provider = 'static' AND r.source = 'static')
				OR (t.provider <> 'static' AND t.kind <> '' AND
					(p.provider = t.provider OR (h.id = t.id AND NOT t.has_pool))
					AND h.provision_requested_at IS NOT NULL
					AND coalesce(h.instance_type, h.launch_template->>'instanceType', '') = t.kind
					AND coalesce(h.market, '') = t.market
					AND ((t.market = 'spot' AND (h.id = t.id OR (t.zone <> '' AND h.zone = t.zone)) AND r.source LIKE '%-spot-history')
						OR (t.market = 'on-demand' AND (h.id = t.id OR (t.region <> '' AND h.launch_template->>'region' = t.region)) AND r.source LIKE '%-pricing')))
			UNION ALL
			SELECT pc.fetched_at, NULL::timestamptz, pc.per_hour, pc.currency, 'price-cache',
				coalesce(c.cap_cpus, t.cpus), coalesce(c.cap_memory, t.memory), false, false
			FROM target t JOIN price_cache pc ON pc.provider = t.provider AND pc.region = t.region
				AND pc.instance_type = t.kind AND pc.os = 'Linux'
			LEFT JOIN own_capacity c ON true
			WHERE t.provider <> 'static' AND t.market = 'on-demand' AND t.kind <> '' AND t.region <> ''
		)
		SELECT valid_from, valid_to, per_hour::text, currency, cap_cpus, cap_memory, source
		FROM candidates WHERE cap_cpus > 0 OR cap_memory > 0
		ORDER BY covering DESC, own DESC, valid_from DESC LIMIT 1`, hostID, start).
		Scan(&rate.From, &rate.To, &rate.PerHour, &rate.Currency, &rate.CapCPUs, &rate.CapMemory, &rate.Source)
	return rate, err
}

func pricePlacementSnapshot(ctx context.Context, tx pgx.Tx, p placementWindow, hostID string, rate ratePeriod, end time.Time) (*big.Rat, error) {
	rows, err := tx.Query(ctx, `SELECT id, run_id, epoch, coalesce((resources->>'cpus')::float8, 0),
		coalesce((resources->>'memory')::int8, 0), created_at, ended_at
		FROM placements WHERE host_id = $1 AND created_at < $3 AND (ended_at IS NULL OR ended_at > $2)
		ORDER BY created_at, id`, hostID, p.From, end)
	if err != nil {
		return nil, err
	}
	placements, err := pgx.CollectRows(rows, pgx.RowToStructByPos[placementWindow])
	if err != nil {
		return nil, err
	}
	from := p.From
	rate.From, rate.To = from, &end
	res, err := computeCost(hostCompute{HostID: hostID, From: from, To: &end, Now: end,
		Rates: []ratePeriod{rate}, Placements: placements})
	if err != nil {
		return nil, err
	}
	for i, other := range placements {
		if other.ID == p.ID {
			if amount := res.Placements[i].Amounts[rate.Currency]; amount != nil {
				return amount, nil
			}
			return new(big.Rat), nil
		}
	}
	return new(big.Rat), nil
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
