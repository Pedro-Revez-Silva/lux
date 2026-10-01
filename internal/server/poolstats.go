package server

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Pool analytics (the console's Pools list and pool page): what a pool
// holds now, its history from pool_samples, and its cost from cost_hourly,
// always by the pool's immutable id, resolved from its name and owner as
// every other pool read does (resolveNamedPool). A tenant sees a shared
// platform pool's hosts and capacity, as GET /v1/hosts shows them, and only
// its own Runs, allocation and cost on it: tenant rows of pool_samples, and
// cost_hourly under its RLS scope.

func (s *Server) poolRoutes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "poolStats", Method: http.MethodGet, Path: "/v1/pools/stats", Tags: []string{"pools"},
		Summary: "Every pool's figures, in one read",
		Description: "Per pool the caller sees (as GET /v1/pools): hosts by state, CPU allocated of capacity (ready and draining hosts), Runs first started in the range with an hourly count, launches the provider refused in the range, " +
			"and the cost of the Runs on it in the range, per currency (list prices; never summed across currencies; absent: none reported, not zero). " +
			"A tenant's figures on a shared platform pool are its own Runs', allocation and cost; hosts and capacity are the pool's.",
		Errors: []int{http.StatusBadRequest},
	}, "read", s.poolStats)
	register(s, api, huma.Operation{
		OperationID: "poolMetrics", Method: http.MethodGet, Path: "/v1/pools/{name}/metrics", Tags: []string{"pools", "history"},
		Summary: "A pool now, and over time",
		Description: "Now: hosts by state, capacity and what live placements hold, Runs running and queued, launch failures in the range. " +
			"Over time: the pool's samples (by its id: a rename keeps them, and a pool removed and set again under its name is the same pool, whose history continues) of the same, with starts, finishes and launches per sample. " +
			"Samples begin when luxd started taking them (historyFrom); earlier is a gap, not zero. A tenant's Runs and allocation are its own.",
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
	}, "read", s.poolMetrics)
	register(s, api, huma.Operation{
		OperationID: "poolCost", Method: http.MethodGet, Path: "/v1/pools/{name}/cost", Tags: []string{"pools", "costs"},
		Summary: "What a pool cost",
		Description: "List prices, per currency, never summed across currencies. `series`: the cost of the Runs on it by family per hour (day for ranges over 7 days): compute on its hosts, and other families of Runs bound to it. " +
			"`topRuns`: its costliest Runs in the range. Operators not narrowed to a tenant also get the pool's host time: `hosts` (per host, allocated to Runs and idle) and `hostSeries`; idle time is apart from the Runs' cost, never added to it. " +
			"A tenant sees only its own Runs' cost.",
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
	}, "read", s.poolCost)
}

// PoolRef names a pool by name and owner.
type PoolRef struct {
	TenantQuery
	Name  string `path:"name" doc:"The pool's name."`
	Owner string `query:"owner" enum:"platform,tenant," doc:"Which pool of that name (as for its events)."`
}

type MoneyAmount struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

type PoolStats struct {
	ID             string         `json:"id"`
	Hosts          map[string]int `json:"hosts" doc:"Hosts not terminated, by state."`
	CapacityCPUs   float64        `json:"capacityCpus" doc:"Cores of its ready and draining hosts."`
	AllocatedCPUs  float64        `json:"allocatedCpus" doc:"Cores its live placements hold (a tenant: its own)."`
	RunsStarted    int            `json:"runsStarted" doc:"Runs first started in the range (a tenant: its own)."`
	RunsHourly     []int          `json:"runsHourly" doc:"The same per hour of the range, oldest first."`
	LaunchFailures int            `json:"launchFailures" doc:"Launches the provider refused in the range."`
	Cost           []MoneyAmount  `json:"cost" doc:"The cost of the Runs on it in the range, per currency; empty: none reported."`
}

type poolStatsInput struct {
	TenantQuery
	Since string `query:"since" doc:"The range, back from now: a Go duration or days (7d). Default 24h." example:"24h"`
}

type poolStatsOutput struct {
	Body struct {
		From  time.Time   `json:"from"`
		To    time.Time   `json:"to"`
		Basis string      `json:"basis"`
		Pools []PoolStats `json:"pools"`
	} `nameHint:"PoolStatsList"`
}

// poolStats is every visible pool's figures in a few grouped reads (never
// one per pool): hosts and launches as the system, Runs, allocation and
// cost under the caller's scope.
func (s *Server) poolStats(ctx context.Context, in *poolStatsInput) (*poolStatsOutput, error) {
	p := principal(ctx)
	since := 24 * time.Hour
	if in.Since != "" {
		d, err := parseSince(in.Since)
		if err != nil || d > costMaxRange {
			return nil, errf(http.StatusBadRequest, "bad_request", "since: a duration up to 90 days")
		}
		since = d
	}
	out := &poolStatsOutput{}
	out.Body.Basis = "list"
	out.Body.Pools = []PoolStats{}
	byID := map[string]*PoolStats{}
	var ids []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT now() - $1::interval, now()`, interval(since)).Scan(&out.Body.From, &out.Body.To); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.id FROM pools p WHERE ($1 = '' OR p.tenant_id = $1 OR p.tenant_id IS NULL) AND NOT p.retired ORDER BY p.id`, p.TenantID)
		if err != nil {
			return err
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		hours := int(since / time.Hour)
		if since%time.Hour != 0 {
			hours++
		}
		for _, id := range ids {
			ps := &PoolStats{ID: id, Hosts: map[string]int{}, RunsHourly: make([]int, max(hours, 1)), Cost: []MoneyAmount{}}
			byID[id] = ps
		}
		// Hosts as GET /v1/hosts shows them to the caller.
		rows, err = tx.Query(ctx, `SELECT h.pool_id, h.state, count(*),
				coalesce(sum((h.capacity->>'cpus')::float8) FILTER (WHERE h.state IN ('ready', 'draining')), 0)
			FROM hosts h WHERE h.pool_id = ANY($2) AND h.state <> 'terminated' AND `+visibleHosts+`
			GROUP BY h.pool_id, h.state`, p.TenantID, ids)
		if err != nil {
			return err
		}
		var pool, state string
		var n int
		var cpus float64
		if _, err := pgx.ForEachRow(rows, []any{&pool, &state, &n, &cpus}, func() error {
			byID[pool].Hosts[state] = n
			byID[pool].CapacityCPUs += cpus
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT h.pool_id, count(*) FROM hosts h
			WHERE h.pool_id = ANY($2) AND h.launch_outcome = 'failed' AND h.launch_finished_at > $3 AND `+visibleHosts+`
			GROUP BY h.pool_id`, p.TenantID, ids, out.Body.From)
		if err != nil {
			return err
		}
		_, err = pgx.ForEachRow(rows, []any{&pool, &n}, func() error {
			byID[pool].LaunchFailures = n
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.pool_id, coalesce(sum((pl.resources->>'cpus')::float8), 0)
			FROM placements pl JOIN hosts h ON h.id = pl.host_id
			WHERE h.pool_id = ANY($1) AND pl.state IN `+livePlacementStates+` GROUP BY h.pool_id`, ids)
		if err != nil {
			return err
		}
		var pool string
		var cpus float64
		if _, err := pgx.ForEachRow(rows, []any{&pool, &cpus}, func() error {
			byID[pool].AllocatedCPUs = cpus
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT pool_id, floor(extract(epoch FROM first_started_at - $2) / 3600)::int, count(*)
			FROM runs WHERE pool_id = ANY($1) AND first_started_at > $2 AND first_started_at <= $3
			GROUP BY 1, 2`, ids, out.Body.From, out.Body.To)
		if err != nil {
			return err
		}
		var bucket, n int
		if _, err := pgx.ForEachRow(rows, []any{&pool, &bucket, &n}, func() error {
			ps := byID[pool]
			ps.RunsStarted += n
			if bucket >= 0 && bucket < len(ps.RunsHourly) {
				ps.RunsHourly[bucket] += n
			}
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT c.pool, c.currency, trim_scale(sum(c.amount))::text
			FROM `+poolRunCost("= ANY($1)", "date_trunc('hour', $2::timestamptz)", "$3")+`
			GROUP BY 1, 2 ORDER BY 1, 2`, ids, out.Body.From, out.Body.To)
		if err != nil {
			return err
		}
		var m MoneyAmount
		_, err = pgx.ForEachRow(rows, []any{&pool, &m.Currency, &m.Amount}, func() error {
			byID[pool].Cost = append(byID[pool].Cost, m)
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out.Body.Pools = append(out.Body.Pools, *byID[id])
	}
	return out, nil
}

type PoolSample struct {
	At             time.Time      `json:"at"`
	Hosts          map[string]int `json:"hosts,omitempty" doc:"Hosts by state."`
	CapacityCPUs   float64        `json:"capacityCpus"`
	CapacityMemory int64          `json:"capacityMemory"`
	AllocatedCPUs  float64        `json:"allocatedCpus"`
	AllocatedMem   int64          `json:"allocatedMemory"`
	Running        int            `json:"running"`
	Queued         int            `json:"queued"`
	Started        int            `json:"started"`
	Finished       int            `json:"finished"`
	Launches       int            `json:"launches"`
	LaunchFailures int            `json:"launchFailures"`
}

type PoolNow struct {
	Hosts             map[string]int `json:"hosts"`
	CapacityCPUs      float64        `json:"capacityCpus"`
	CapacityMemory    int64          `json:"capacityMemory"`
	AllocatedCPUs     float64        `json:"allocatedCpus"`
	AllocatedMem      int64          `json:"allocatedMemory"`
	Running           int            `json:"running"`
	Queued            int            `json:"queued"`
	OldestQueuedAt    *time.Time     `json:"oldestQueuedAt,omitempty"`
	LaunchFailures    int            `json:"launchFailures" doc:"Launches the provider refused in the range."`
	LastLaunchFailure *time.Time     `json:"lastLaunchFailure,omitempty"`
	LastLaunchError   string         `json:"lastLaunchError,omitempty"`
}

type poolMetricsInput struct {
	PoolRef
	HistoryQuery
}

type poolMetricsOutput struct {
	Body struct {
		PoolID      string       `json:"poolId"`
		From        time.Time    `json:"from"`
		To          time.Time    `json:"to"`
		Resolution  int          `json:"resolution"`
		HistoryFrom *time.Time   `json:"historyFrom,omitempty" doc:"The pool's first sample at this resolution: before it, no history (a gap, not zero)."`
		Now         PoolNow      `json:"now"`
		Samples     []PoolSample `json:"samples"`
	} `nameHint:"PoolMetrics"`
}

func (s *Server) poolMetrics(ctx context.Context, in *poolMetricsInput) (*poolMetricsOutput, error) {
	p := principal(ctx)
	from, to, res, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	out := &poolMetricsOutput{}
	out.Body.From, out.Body.To, out.Body.Resolution = from, to, res
	out.Body.Samples = []PoolSample{}
	now := &out.Body.Now
	now.Hosts = map[string]int{}
	var pool namedPool
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if pool, err = resolveNamedPool(ctx, tx, p, in.Name, in.Owner); err != nil {
			return err
		}
		out.Body.PoolID = pool.ID
		rows, err := tx.Query(ctx, `SELECT h.state, count(*),
				coalesce(sum((h.capacity->>'cpus')::float8) FILTER (WHERE h.state IN ('ready', 'draining')), 0),
				coalesce(sum((h.capacity->>'memory')::int8) FILTER (WHERE h.state IN ('ready', 'draining')), 0)::bigint
			FROM hosts h WHERE h.pool_id = $2 AND h.state <> 'terminated' AND `+visibleHosts+` GROUP BY h.state`, p.TenantID, pool.ID)
		if err != nil {
			return err
		}
		var state string
		var n int
		var cpus float64
		var mem int64
		if _, err := pgx.ForEachRow(rows, []any{&state, &n, &cpus, &mem}, func() error {
			now.Hosts[state], now.CapacityCPUs, now.CapacityMemory = n, now.CapacityCPUs+cpus, now.CapacityMemory+mem
			return nil
		}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), max(h.launch_finished_at),
				coalesce((SELECT `+hostLaunchError+` FROM hosts h WHERE h.pool_id = $2 AND h.launch_outcome = 'failed' AND `+visibleHosts+`
					ORDER BY h.launch_finished_at DESC NULLS LAST LIMIT 1), '')
			FROM hosts h WHERE h.pool_id = $2 AND h.launch_outcome = 'failed' AND h.launch_finished_at > $3 AND `+visibleHosts,
			p.TenantID, pool.ID, from).Scan(&now.LaunchFailures, &now.LastLaunchFailure, &now.LastLaunchError); err != nil {
			return err
		}
		// Whole-pool samples; a tenant's Runs and allocation come from its
		// own rows, none at an instant being nothing of its there.
		own := func(col string) string {
			return `CASE WHEN $5 = '' THEN w.` + col + ` ELSE coalesce(t.` + col + `, 0) END`
		}
		rows, err = tx.Query(ctx, `SELECT w.at, w.hosts, w.cap_cpus, w.cap_mem, w.launches, w.launch_failures,
				`+own("alloc_cpus")+`, `+own("alloc_mem")+`, `+own("running")+`, `+own("queued")+`, `+own("started")+`, `+own("finished")+`
			FROM pool_samples w
			LEFT JOIN pool_samples t ON $5 <> '' AND t.pool_id = w.pool_id AND t.tenant_id = $5 AND t.res = w.res AND t.at = w.at
			WHERE w.pool_id = $1 AND w.tenant_id = '' AND w.res = $2 AND w.at BETWEEN $3 AND $4 ORDER BY w.at`,
			pool.ID, res, from, to, p.TenantID)
		if err != nil {
			return err
		}
		var sm PoolSample
		if _, err := pgx.ForEachRow(rows, []any{&sm.At, &sm.Hosts, &sm.CapacityCPUs, &sm.CapacityMemory, &sm.Launches, &sm.LaunchFailures,
			&sm.AllocatedCPUs, &sm.AllocatedMem, &sm.Running, &sm.Queued, &sm.Started, &sm.Finished}, func() error {
			out.Body.Samples = append(out.Body.Samples, sm)
			return nil
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT min(at) FROM pool_samples WHERE pool_id = $1 AND tenant_id = '' AND res = $2`, pool.ID, res).Scan(&out.Body.HistoryFrom)
	})
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum((pl.resources->>'cpus')::float8), 0), coalesce(sum((pl.resources->>'memory')::int8), 0)::bigint
			FROM placements pl JOIN hosts h ON h.id = pl.host_id WHERE h.pool_id = $1 AND pl.state IN `+livePlacementStates, pool.ID).
			Scan(&now.AllocatedCPUs, &now.AllocatedMem); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'running'), count(*) FILTER (WHERE state IN `+queuedRunStates+`),
				min(coalesce(needs_host_since, updated_at)) FILTER (WHERE state IN `+queuedRunStates+`)
			FROM runs WHERE pool_id = $1 AND (state = 'running' OR state IN `+queuedRunStates+`)`, pool.ID).Scan(&now.Running, &now.Queued, &now.OldestQueuedAt)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type PoolCostPoint struct {
	At       time.Time `json:"at"`
	Family   string    `json:"family"`
	Currency string    `json:"currency"`
	Amount   string    `json:"amount"`
}

type PoolHostTime struct {
	At          *time.Time `json:"at,omitempty"`
	HostID      string     `json:"hostId,omitempty"`
	HostName    string     `json:"hostName,omitempty"`
	Currency    string     `json:"currency"`
	Allocated   string     `json:"allocated" doc:"Host time reserved by Runs (their compute cost comes from it)."`
	Unallocated string     `json:"unallocated" doc:"Host time no Run reserved: idle."`
}

type PoolTopRun struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	Estimate bool   `json:"estimate" doc:"Part of it may still change (estimate lines)."`
}

type poolCostInput struct {
	PoolRef
	HistoryQuery
	Interval string `query:"interval" enum:"hour,day," doc:"Buckets of series: hour (default) or day."`
}

type poolCostOutput struct {
	Body struct {
		PoolID     string           `json:"poolId"`
		From       time.Time        `json:"from"`
		To         time.Time        `json:"to"`
		Basis      string           `json:"basis"`
		Interval   string           `json:"interval"`
		Totals     []MoneyAmount    `json:"totals" doc:"The Runs' cost on the pool in the range, per currency."`
		Series     []PoolCostPoint  `json:"series" doc:"The same by family and bucket; a bucket without a row is no cost reported, not zero."`
		Families   []CostFamilyInfo `json:"families,omitempty"`
		TopRuns    []PoolTopRun     `json:"topRuns" doc:"The costliest Runs, up to 10 per currency, costliest first."`
		Idle       []MoneyAmount    `json:"idle,omitempty" doc:"Operators: host time no Run reserved, per currency. Not part of totals."`
		HostSeries []PoolHostTime   `json:"hostSeries,omitempty" doc:"Operators: host time per bucket, allocated and idle."`
		Hosts      []PoolHostTime   `json:"hosts,omitempty" doc:"Operators: host time per host in the range, allocated and idle."`
	} `nameHint:"PoolCost"`
}

// poolRunCost is a FROM item c of the Runs' cost_hourly rows on the pools
// pool (SQL comparing a pool id: "= $1", "= ANY($1)") in [from, to): c.pool,
// c.hour, c.run_id, c.family, c.currency, c.amount. A Run's cost is its
// pool's: compute by the host it ran on (the row's pool_id), other families
// (no pool_id) by the pool the Run is bound to. The two are read apart,
// which is coalesce(c.pool_id, r.pool_id) pool, so the first is served by
// cost_hourly_pool_hour instead of a join to runs for every row in range.
func poolRunCost(pool, from, to string) string {
	return `(SELECT c.pool_id AS pool, c.hour, c.run_id, c.family, c.currency, c.amount FROM cost_hourly c
			WHERE c.run_id IS NOT NULL AND c.pool_id ` + pool + ` AND c.hour >= ` + from + ` AND c.hour < ` + to + `
		UNION ALL
		SELECT r.pool_id, c.hour, c.run_id, c.family, c.currency, c.amount FROM cost_hourly c JOIN runs r ON r.id = c.run_id
			WHERE c.run_id IS NOT NULL AND c.pool_id IS NULL AND r.pool_id ` + pool + ` AND c.hour >= ` + from + ` AND c.hour < ` + to + `) c`
}

// poolCost reads cost_hourly by the pool's id: Runs' cost under the
// caller's scope (RLS gives a tenant its own rows only), and, for an
// operator over every tenant, the pool's host time. Buckets are whole
// hours, as GET /v1/costs has them.
func (s *Server) poolCost(ctx context.Context, in *poolCostInput) (*poolCostOutput, error) {
	p := principal(ctx)
	from, to, _, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	if from, to, err = costRange(from, to); err != nil {
		return nil, err
	}
	iv := in.Interval
	if iv == "" {
		iv = "hour"
	}
	bucket := map[string]string{"hour": "c.hour", "day": "date_trunc('day', c.hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"}[iv]
	if bucket == "" {
		return nil, errf(http.StatusBadRequest, "bad_request", "interval must be hour or day")
	}
	out := &poolCostOutput{}
	b := &out.Body
	b.From, b.To, b.Basis, b.Interval = from, to, "list", iv
	b.Totals, b.Series, b.TopRuns = []MoneyAmount{}, []PoolCostPoint{}, []PoolTopRun{}
	var pool namedPool
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		pool, err = resolveNamedPool(ctx, tx, p, in.Name, in.Owner)
		return err
	}); err != nil {
		return nil, err
	}
	b.PoolID = pool.ID
	onPool := poolRunCost("= $1", "$2", "$3")
	err = s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.currency, trim_scale(sum(c.amount))::text FROM `+onPool+` GROUP BY 1 ORDER BY 1`, pool.ID, from, to)
		if err != nil {
			return err
		}
		if b.Totals, err = pgx.CollectRows(rows, pgx.RowToStructByPos[MoneyAmount]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+bucket+`, c.family, c.currency, trim_scale(sum(c.amount))::text FROM `+onPool+`
			GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, pool.ID, from, to)
		if err != nil {
			return err
		}
		if b.Series, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PoolCostPoint]); err != nil {
			return err
		}
		if err := costRowLimit(len(b.Series)); err != nil {
			return err
		}
		// Ranked by run id and currency first; runs and cost_lines are read
		// for the 10 per currency kept, not for every Run in range.
		rows, err = tx.Query(ctx, `SELECT r.id, r.name, x.currency, x.amount::text,
				EXISTS (SELECT 1 FROM cost_lines l WHERE l.run_id = x.run_id AND NOT l.final)
			FROM (SELECT c.run_id, c.currency, trim_scale(sum(c.amount)) AS amount,
					row_number() OVER (PARTITION BY c.currency ORDER BY sum(c.amount) DESC, c.run_id) AS rank
				FROM `+onPool+` GROUP BY c.run_id, c.currency) x
			JOIN runs r ON r.id = x.run_id
			WHERE x.rank <= 10 ORDER BY x.currency, x.rank`, pool.ID, from, to)
		if err != nil {
			return err
		}
		b.TopRuns, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PoolTopRun])
		return err
	})
	if err != nil {
		return nil, err
	}
	families := map[string]bool{}
	for _, pt := range b.Series {
		families[pt.Family] = true
	}
	meta := s.costFamilyMetadata()
	for _, f := range slices.Sorted(maps.Keys(families)) {
		name, color := meta.lookup(f)
		b.Families = append(b.Families, CostFamilyInfo{Family: f, DisplayName: name, Color: color})
	}
	if !p.Operator || p.TenantID != "" {
		return out, nil
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		const hostRows = `c.run_id IS NULL AND c.pool_id = $1 AND c.hour >= $2 AND c.hour < $3`
		rows, err := tx.Query(ctx, `SELECT c.currency, trim_scale(sum(c.unallocated))::text FROM cost_hourly c WHERE `+hostRows+` GROUP BY 1 ORDER BY 1`, pool.ID, from, to)
		if err != nil {
			return err
		}
		if b.Idle, err = pgx.CollectRows(rows, pgx.RowToStructByPos[MoneyAmount]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+bucket+`, '', '', c.currency, trim_scale(sum(c.allocated))::text, trim_scale(sum(c.unallocated))::text
			FROM cost_hourly c WHERE `+hostRows+` GROUP BY 1, 4 ORDER BY 1, 4`, pool.ID, from, to)
		if err != nil {
			return err
		}
		if b.HostSeries, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PoolHostTime]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT NULL::timestamptz, c.host_id, h.name, c.currency, trim_scale(sum(c.allocated))::text, trim_scale(sum(c.unallocated))::text
			FROM cost_hourly c JOIN hosts h ON h.id = c.host_id WHERE `+hostRows+` GROUP BY 2, 3, 4 ORDER BY 4, sum(c.allocated) + sum(c.unallocated) DESC, 2`, pool.ID, from, to)
		if err != nil {
			return err
		}
		b.Hosts, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PoolHostTime])
		if err == nil {
			err = costRowLimit(len(b.HostSeries) + len(b.Hosts))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
