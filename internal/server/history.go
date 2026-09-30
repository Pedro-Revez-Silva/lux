package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// History: resource use and system state over time (docs/telemetry.md).
//
// Raw samples (res 0) come from heartbeats (hosts, placements) and from a
// system tick (sampleSystem). The reaper rolls each resolution up into the
// next — raw into minutes (res 60), minutes into hours (res 3600) — and
// deletes what is older than its resolution's retention. A read picks the
// finest resolution that still covers its range.

// Retention defaults (LUX_HISTORY_RAW, LUX_HISTORY_MINUTES, LUX_HISTORY_HOURS).
const (
	DefaultHistoryRaw     = 48 * time.Hour
	DefaultHistoryMinutes = 30 * 24 * time.Hour
	DefaultHistoryHours   = 400 * 24 * time.Hour
)

// resolution: seconds per sample (0: raw), and how long they are kept.
type resolution struct {
	res  int
	keep time.Duration
}

// resolutions, finest first.
func (s *Server) resolutions() []resolution {
	return []resolution{{0, s.cfg.HistoryRaw}, {60, s.cfg.HistoryMinutes}, {3600, s.cfg.HistoryHours}}
}

// sampleHost records a host's heartbeat: its usage, its runner's, and what
// its live placements hold.
func sampleHost(ctx context.Context, tx pgx.Tx, hostID string, u *proto.HostUsage, r *proto.ProcessUsage) error {
	var cpu *float64
	var mem, disk *int64
	if u != nil {
		cpu, mem, disk = &u.CPUSeconds, &u.MemoryBytes, &u.DiskBytes
	}
	_, err := tx.Exec(ctx, insertHostSample, append([]any{hostID, cpu, mem, disk}, procValues(r)...)...)
	return err
}

// historyLoop samples the system every SampleEvery, and rolls up and
// expires history every minute.
func (s *Server) historyLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SampleEvery)
	defer t.Stop()
	var lastRollup time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.sampleSystem(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("history: sample", "err", err)
		}
		if time.Since(lastRollup) >= time.Minute {
			lastRollup = time.Now()
			if err := s.rollupHistory(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("history: rollup", "err", err)
			}
		}
	}
}

// sampleSystem writes one system sample per tenant with anything to count,
// and one for the whole system (an empty tenant id). Runs and starts are the
// tenant's; hosts and capacity are the tenant's own hosts plus the
// platform's (what it can use), and every host for the whole system.
//
// Starts and finishes are counted in a window a minute behind (from the
// last sample's window end to now - 1m), so one written by a transaction
// still open at sampling time is counted by a later sample, and none twice.
//
// The control host (control.go) is sampled in the same transaction, so its
// rows share the whole system's `at`.
func (s *Server) sampleSystem(ctx context.Context) error {
	host := s.readControlHost(ctx)
	var stored bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var from time.Time
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(window_end), now() - interval '1 minute' - $1::interval)
			FROM system_samples WHERE res = 0 AND tenant_id = ''`, interval(s.cfg.SampleEvery)).Scan(&from); err != nil {
			return err
		}
		// Each table is read once: GROUPING SETS give each tenant's rows
		// and, with the empty set (tenant ''), the whole system's.
		_, err := tx.Exec(ctx, `
			WITH w AS (SELECT $1::timestamptz AS since, now() - interval '1 minute' AS until),
			by_state AS (
				SELECT coalesce(tenant_id, '') AS id, state, count(*) AS n,
					count(*) FILTER (WHERE state = 'running' AND activity = 'busy') AS busy,
					count(*) FILTER (WHERE state = 'running' AND activity = 'idle') AS idle
				FROM runs WHERE state NOT IN ('succeeded', 'failed', 'cancelled')
				GROUP BY GROUPING SETS ((tenant_id, state), (state))
			),
			runs_by AS (
				SELECT id, jsonb_object_agg(state, n) AS runs, sum(busy)::int AS busy, sum(idle)::int AS idle,
					coalesce(sum(n) FILTER (WHERE state IN `+queuedRunStates+`), 0)::int AS queued
				FROM by_state GROUP BY id
			),
			flow AS (
				SELECT coalesce(r.tenant_id, '') AS id,
					count(*) FILTER (WHERE r.first_started_at > w.since AND r.first_started_at <= w.until) AS started,
					count(*) FILTER (WHERE r.finished_at > w.since AND r.finished_at <= w.until) AS finished,
					percentile_cont(ARRAY[0.5, 0.95]) WITHIN GROUP (ORDER BY extract(epoch FROM r.first_started_at - r.created_at))
						FILTER (WHERE r.first_started_at > w.since AND r.first_started_at <= w.until) AS pct
				FROM runs r, w
				WHERE r.first_started_at > w.since AND r.first_started_at <= w.until OR r.finished_at > w.since AND r.finished_at <= w.until
				GROUP BY GROUPING SETS ((r.tenant_id), ())
			),
			alloc AS (
				SELECT coalesce(pl.tenant_id, '') AS id, coalesce(sum((pl.resources->>'cpus')::float8), 0) AS cpus,
					coalesce(sum((pl.resources->>'memory')::int8), 0)::bigint AS mem
				FROM placements pl WHERE pl.state IN `+livePlacementStates+`
				GROUP BY GROUPING SETS ((pl.tenant_id), ())
			),
			tenants AS (
				SELECT '' AS id
				UNION SELECT id FROM runs_by UNION SELECT id FROM flow
				UNION SELECT tenant_id FROM hosts WHERE tenant_id IS NOT NULL AND state <> 'terminated'
			),
			-- Hosts a tenant may use: its own and the platform's; all for ''.
			hosts_by AS (
				SELECT t.id, jsonb_object_agg(x.state, x.n) AS hosts, sum(x.cpus) AS cpus, sum(x.mem)::bigint AS mem
				FROM tenants t CROSS JOIN LATERAL (
					SELECT h.state, count(*) AS n,
						coalesce(sum((h.capacity->>'cpus')::float8) FILTER (WHERE h.state IN ('ready', 'draining')), 0) AS cpus,
						coalesce(sum((h.capacity->>'memory')::int8) FILTER (WHERE h.state IN ('ready', 'draining')), 0) AS mem
					FROM hosts h WHERE h.state <> 'terminated' AND (t.id = '' OR h.tenant_id = t.id OR h.tenant_id IS NULL)
					GROUP BY h.state) x
				GROUP BY t.id
			)
			INSERT INTO system_samples (tenant_id, res, at, window_end, runs, busy, idle, queued, started, finished, start_p50, start_p95,
				hosts, cap_cpus, cap_mem, alloc_cpus, alloc_mem)
			SELECT t.id, 0, now(), (SELECT until FROM w), coalesce(r.runs, '{}'), coalesce(r.busy, 0), coalesce(r.idle, 0), coalesce(r.queued, 0),
				coalesce(f.started, 0), coalesce(f.finished, 0), f.pct[1], f.pct[2], coalesce(h.hosts, '{}'), coalesce(h.cpus, 0), coalesce(h.mem, 0),
				coalesce(a.cpus, 0), coalesce(a.mem, 0)
			FROM tenants t LEFT JOIN runs_by r USING (id) LEFT JOIN flow f USING (id) LEFT JOIN alloc a USING (id) LEFT JOIN hosts_by h USING (id)
			ON CONFLICT DO NOTHING`, from)
		if err != nil {
			return err
		}
		if err := samplePools(ctx, tx, from); err != nil {
			return err
		}
		stored, err = s.sampleControl(ctx, tx, host)
		return err
	})
	// Committed with its row: the peak RSS it carries is recorded. Else
	// that peak stays pending for the next sample.
	if err == nil && stored {
		s.proc.Stored()
	}
	return err
}

// samplePools writes, in sampleSystem's transaction and at its instant and
// window, one sample per pool (tenant ”) and one per tenant with Runs or
// placements on it. Each table is read once, grouped by pool (and tenant):
// never a query per pool, host or Run. A pool is sampled while it is live,
// or retired with hosts or Runs still on it.
func samplePools(ctx context.Context, tx pgx.Tx, from time.Time) error {
	_, err := tx.Exec(ctx, `
		WITH w AS (SELECT $1::timestamptz AS since, now() - interval '1 minute' AS until),
		host_state AS (
			SELECT pool_id, jsonb_object_agg(state, n) AS hosts, sum(cpus) AS cpus, sum(mem)::bigint AS mem FROM (
				SELECT h.pool_id, h.state, count(*) AS n,
					coalesce(sum((h.capacity->>'cpus')::float8) FILTER (WHERE h.state IN ('ready', 'draining')), 0) AS cpus,
					coalesce(sum((h.capacity->>'memory')::int8) FILTER (WHERE h.state IN ('ready', 'draining')), 0) AS mem
				FROM hosts h WHERE h.pool_id IS NOT NULL AND h.state <> 'terminated' GROUP BY h.pool_id, h.state) x
			GROUP BY pool_id
		),
		alloc AS (
			SELECT h.pool_id, coalesce(pl.tenant_id, '') AS id, coalesce(sum((pl.resources->>'cpus')::float8), 0) AS cpus,
				coalesce(sum((pl.resources->>'memory')::int8), 0)::bigint AS mem
			FROM placements pl JOIN hosts h ON h.id = pl.host_id
			WHERE pl.state IN `+livePlacementStates+` AND h.pool_id IS NOT NULL
			GROUP BY GROUPING SETS ((h.pool_id, pl.tenant_id), (h.pool_id))
		),
		runs_by AS (
			SELECT pool_id, coalesce(tenant_id, '') AS id, count(*) FILTER (WHERE state = 'running') AS running,
				count(*) FILTER (WHERE state IN `+queuedRunStates+`) AS queued
			FROM runs WHERE pool_id IS NOT NULL AND (state = 'running' OR state IN `+queuedRunStates+`)
			GROUP BY GROUPING SETS ((pool_id, tenant_id), (pool_id))
		),
		flow AS (
			SELECT r.pool_id, coalesce(r.tenant_id, '') AS id,
				count(*) FILTER (WHERE r.first_started_at > w.since AND r.first_started_at <= w.until) AS started,
				count(*) FILTER (WHERE r.finished_at > w.since AND r.finished_at <= w.until) AS finished
			FROM runs r, w
			WHERE r.pool_id IS NOT NULL AND (r.first_started_at > w.since AND r.first_started_at <= w.until OR r.finished_at > w.since AND r.finished_at <= w.until)
			GROUP BY GROUPING SETS ((r.pool_id, r.tenant_id), (r.pool_id))
		),
		launch AS (
			SELECT h.pool_id,
				count(*) FILTER (WHERE h.provision_requested_at > w.since AND h.provision_requested_at <= w.until) AS launches,
				count(*) FILTER (WHERE h.launch_outcome = 'failed' AND h.launch_finished_at > w.since AND h.launch_finished_at <= w.until) AS failures
			FROM hosts h, w
			WHERE h.pool_id IS NOT NULL AND h.provision_requested_at > w.since - interval '1 day'
			GROUP BY h.pool_id
		),
		keys AS (
			SELECT p.id AS pool_id, '' AS id FROM pools p
			WHERE NOT p.retired OR EXISTS (SELECT 1 FROM host_state hs WHERE hs.pool_id = p.id) OR EXISTS (SELECT 1 FROM runs_by rb WHERE rb.pool_id = p.id)
			UNION SELECT pool_id, id FROM alloc UNION SELECT pool_id, id FROM runs_by UNION SELECT pool_id, id FROM flow
		)
		INSERT INTO pool_samples (pool_id, tenant_id, res, at, window_end, hosts, cap_cpus, cap_mem, alloc_cpus, alloc_mem,
			running, queued, started, finished, launches, launch_failures)
		SELECT k.pool_id, k.id, 0, now(), (SELECT until FROM w),
			CASE WHEN k.id = '' THEN coalesce(hs.hosts, '{}') ELSE '{}' END,
			CASE WHEN k.id = '' THEN coalesce(hs.cpus, 0) ELSE 0 END, CASE WHEN k.id = '' THEN coalesce(hs.mem, 0) ELSE 0 END,
			coalesce(a.cpus, 0), coalesce(a.mem, 0), coalesce(rb.running, 0), coalesce(rb.queued, 0),
			coalesce(f.started, 0), coalesce(f.finished, 0),
			CASE WHEN k.id = '' THEN coalesce(l.launches, 0) ELSE 0 END, CASE WHEN k.id = '' THEN coalesce(l.failures, 0) ELSE 0 END
		FROM keys k
		LEFT JOIN host_state hs ON hs.pool_id = k.pool_id
		LEFT JOIN alloc a ON a.pool_id = k.pool_id AND a.id = k.id
		LEFT JOIN runs_by rb ON rb.pool_id = k.pool_id AND rb.id = k.id
		LEFT JOIN flow f ON f.pool_id = k.pool_id AND f.id = k.id
		LEFT JOIN launch l ON l.pool_id = k.pool_id
		ON CONFLICT DO NOTHING`, from)
	return err
}

// rollupHistory folds each resolution's complete buckets into the next,
// once, and deletes what has outlived its retention.
func (s *Server) rollupHistory(ctx context.Context) error {
	rs := s.resolutions()
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for i := 1; i < len(rs); i++ {
			from, to := rs[i-1].res, rs[i].res
			for _, q := range []string{rollupHosts, rollupPlacements, rollupSystem, rollupPools, rollupControl, rollupControlDisks} {
				if _, err := tx.Exec(ctx, q, from, to); err != nil {
					return err
				}
			}
		}
		for _, r := range rs {
			for _, t := range []string{"host_samples", "placement_samples", "system_samples", "pool_samples", "control_samples", "control_disk_samples"} {
				if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE res = $1::int AND at < now() - $2::interval`, r.res, interval(r.keep)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// The rollups: for each key, the buckets of $2 seconds that are complete
// and past the newest bucket already rolled up. A bucket is complete a
// minute after it ends: a sample is stamped when its transaction starts,
// and may commit a little later. Levels are averaged, peaks and counters
// take the maximum (counters only grow), flows are summed. A process's
// counter is the exception: it starts again when the process does, so a
// bucket keeps its last reading, and that reading's start (procRow.columns).
const (
	rollupBucket = `to_timestamp(floor(extract(epoch FROM at) / $2::int) * $2::int)`
	rollupSince  = `at >= coalesce((SELECT max(d.at) + make_interval(secs => $2::int) FROM %s d WHERE d.res = $2::int AND %s), '-infinity')
		AND at < to_timestamp(floor(extract(epoch FROM now() - interval '1 minute') / $2::int) * $2::int)`
)

var (
	rollupHosts = `INSERT INTO host_samples (host_id, res, at, cpu_seconds, mem_bytes, disk_bytes, placements, alloc_cpus, alloc_mem, ` + procCols + `)
		SELECT host_id, $2::int, ` + rollupBucket + `, max(cpu_seconds), avg(mem_bytes)::bigint, avg(disk_bytes)::bigint,
			round(avg(placements))::int, avg(alloc_cpus), avg(alloc_mem)::bigint, ` + rollupProc + `
		FROM host_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "host_samples", "d.host_id = s.host_id") + `
		GROUP BY host_id, 3 ON CONFLICT DO NOTHING`
	rollupPlacements = `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds, mem_bytes, disk_bytes, pids, net_rx, net_tx)
		SELECT run_id, epoch, tenant_id, $2::int, ` + rollupBucket + `, max(cpu_seconds), avg(mem_bytes)::bigint, max(disk_bytes),
			round(avg(pids))::int, max(net_rx), max(net_tx)
		FROM placement_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "placement_samples", "d.run_id = s.run_id AND d.epoch = s.epoch") + `
		GROUP BY run_id, epoch, tenant_id, 5 ON CONFLICT DO NOTHING`
	// Runs and hosts by state are the bucket's last sample.
	rollupSystem = `INSERT INTO system_samples (tenant_id, res, at, runs, busy, idle, queued, started, finished, start_p50, start_p95,
			hosts, cap_cpus, cap_mem, alloc_cpus, alloc_mem)
		SELECT tenant_id, $2::int, ` + rollupBucket + `, (array_agg(runs ORDER BY at DESC))[1], round(avg(busy))::int, round(avg(idle))::int,
			round(avg(queued))::int, sum(started)::int, sum(finished)::int, avg(start_p50), max(start_p95),
			(array_agg(hosts ORDER BY at DESC))[1], avg(cap_cpus), avg(cap_mem)::bigint, avg(alloc_cpus), avg(alloc_mem)::bigint
		FROM system_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "system_samples", "d.tenant_id = s.tenant_id") + `
		GROUP BY tenant_id, 3 ON CONFLICT DO NOTHING`
	// A pool's levels are averaged, its hosts by state the bucket's last,
	// its flows (starts, finishes, launches) summed.
	rollupPools = `INSERT INTO pool_samples (pool_id, tenant_id, res, at, hosts, cap_cpus, cap_mem, alloc_cpus, alloc_mem, running, queued,
			started, finished, launches, launch_failures)
		SELECT pool_id, tenant_id, $2::int, ` + rollupBucket + `, (array_agg(hosts ORDER BY at DESC))[1], avg(cap_cpus), avg(cap_mem)::bigint,
			avg(alloc_cpus), avg(alloc_mem)::bigint, round(avg(running))::int, round(avg(queued))::int,
			sum(started)::int, sum(finished)::int, sum(launches)::int, sum(launch_failures)::int
		FROM pool_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "pool_samples", "d.pool_id = s.pool_id AND d.tenant_id = s.tenant_id") + `
		GROUP BY pool_id, tenant_id, 4 ON CONFLICT DO NOTHING`
	// The control host, per luxd process (on its machine): CPU a counter,
	// memory and connections levels, totals (cores, memory, disk) their
	// maximum; the database size and disk use the bucket's mean; luxd's
	// process as the hosts' runner.
	rollupControl = `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections, ` + procCols + `)
		SELECT instance, hostname, $2::int, ` + rollupBucket + `, max(cpu_seconds), max(cpus), avg(mem_bytes)::bigint, max(mem_total),
			avg(db_bytes)::bigint, round(avg(db_connections))::int, ` + rollupProc + `
		FROM control_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "control_samples", "d.instance = s.instance") + `
		GROUP BY instance, hostname, 4 ON CONFLICT DO NOTHING`
	rollupControlDisks = `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
		SELECT instance, path, $2::int, ` + rollupBucket + `, avg(used_bytes)::bigint, avg(free_bytes)::bigint, max(total_bytes)
		FROM control_disk_samples s WHERE res = $1::int AND ` + fmt.Sprintf(rollupSince, "control_disk_samples", "d.instance = s.instance AND d.path = s.path") + `
		GROUP BY instance, path, 4 ON CONFLICT DO NOTHING`
)

// Sample is one point of history. Which fields are set depends on what it
// is a sample of; rates are per second over the interval since the
// previous point (the first point of a series has none).
type Sample struct {
	At time.Time `json:"at"`
	// Hosts and placements.
	CPUCores    *float64 `json:"cpuCores,omitempty"`
	MemoryBytes *int64   `json:"memoryBytes,omitempty"`
	DiskBytes   *int64   `json:"diskBytes,omitempty"`
	// Hosts: live placements and what they hold.
	Placements *int     `json:"placements,omitempty"`
	AllocCPUs  *float64 `json:"allocCpus,omitempty"`
	AllocMem   *int64   `json:"allocMemory,omitempty"`
	// Hosts: the runner process (absent before the runner reported it).
	Runner *ProcessSample `json:"runner,omitempty" doc:"The runner process itself (not the podman and conmon processes it starts)."`
	// Placements.
	Pids      *int     `json:"pids,omitempty"`
	NetRxRate *float64 `json:"netRxRate,omitempty"`
	NetTxRate *float64 `json:"netTxRate,omitempty"`
	Epoch     *int     `json:"epoch,omitempty"`
	// The system.
	Runs      map[string]int `json:"runs,omitempty"`
	Busy      *int           `json:"busy,omitempty"`
	Idle      *int           `json:"idle,omitempty"`
	Queued    *int           `json:"queued,omitempty"`
	Started   *int           `json:"started,omitempty"`
	Finished  *int           `json:"finished,omitempty"`
	StartP50  *float64       `json:"startP50,omitempty"`
	StartP95  *float64       `json:"startP95,omitempty"`
	Hosts     map[string]int `json:"hosts,omitempty"`
	CapCPUs   *float64       `json:"capacityCpus,omitempty"`
	CapMem    *int64         `json:"capacityMemory,omitempty"`
	SysAllocC *float64       `json:"allocatedCpus,omitempty"`
	SysAllocM *int64         `json:"allocatedMemory,omitempty"`
}

// ProcessSample is one of lux's own processes: a host's runner, or luxd.
type ProcessSample struct {
	Started      time.Time `json:"started" doc:"When the process started: a change is a restart."`
	CPUCores     *float64  `json:"cpuCores,omitempty" doc:"CPU in use, in cores: a rate over the previous point of the same process."`
	RSSBytes     *int64    `json:"rssBytes,omitempty"`
	PeakRSSBytes *int64    `json:"peakRssBytes,omitempty" doc:"The highest RSS since the previous sample (a rollup: in its interval)."`
	HeapBytes    *int64    `json:"heapBytes,omitempty" doc:"Go heap objects, live or not yet swept."`
	Goroutines   *int64    `json:"goroutines,omitempty"`
}

// procRow is a row's process columns. Its columns are the one list of
// them: names, rollups, insert values and scan destinations all come from
// it, so none can fall out of step with another.
type procRow struct {
	started         *time.Time
	cpu             *float64
	rss, peak, heap *int64
	goroutines      *int64
}

// procColumn is one process column: its name, how a rollup bucket folds
// it, and the procRow field it is read into and written from.
type procColumn struct {
	name, rollup string
	field        any
}

// lastReading is a bucket's last reading of a process column: the counter
// and its start go together, and a restart starts the counter again.
func lastReading(col string) string {
	return `(array_agg(` + col + ` ORDER BY at DESC) FILTER (WHERE proc_started IS NOT NULL))[1]`
}

func (p *procRow) columns() []procColumn {
	return []procColumn{
		{"proc_started", lastReading("proc_started"), &p.started},
		{"proc_cpu_seconds", lastReading("proc_cpu_seconds"), &p.cpu},
		{"proc_rss", "avg(proc_rss)::bigint", &p.rss},
		{"proc_peak_rss", "max(proc_peak_rss)", &p.peak},
		{"proc_heap", "avg(proc_heap)::bigint", &p.heap},
		{"goroutines", "round(avg(goroutines))::bigint", &p.goroutines},
	}
}

// dest are the row's scan destinations, in columns order. They are also a
// row's insert values: pgx writes a nil pointer as NULL.
func (p *procRow) dest() []any {
	var d []any
	for _, c := range p.columns() {
		d = append(d, c.field)
	}
	return d
}

// procColumnSQL is procCols, rollupProc and a placeholder list, from
// procRow.columns.
func procColumnSQL(f func(i int, c procColumn) string) string {
	var out []string
	for i, c := range (&procRow{}).columns() {
		out = append(out, f(i, c))
	}
	return strings.Join(out, ", ")
}

var (
	procCols   = procColumnSQL(func(_ int, c procColumn) string { return c.name })
	rollupProc = procColumnSQL(func(_ int, c procColumn) string { return c.rollup })
	// The raw sample inserts, with the process columns' placeholders.
	insertHostSample = `INSERT INTO host_samples (host_id, res, at, cpu_seconds, mem_bytes, disk_bytes, placements, alloc_cpus, alloc_mem, ` + procCols + `)
		SELECT $1, 0, now(), $2, $3, $4, count(*), coalesce(sum((pl.resources->>'cpus')::float8), 0), coalesce(sum((pl.resources->>'memory')::int8), 0),
			` + procParams(5) + `
		FROM placements pl WHERE pl.host_id = $1 AND pl.state IN ` + livePlacementStates + `
		ON CONFLICT DO NOTHING`
	insertControlSample = `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections, ` + procCols + `)
		VALUES ($1, $2, 0, now(), $3, $4, $5, $6, $7, $8, ` + procParams(9) + `)
		ON CONFLICT DO NOTHING`
)

// procParams are the placeholders for a row's process columns, from $n.
func procParams(n int) string {
	return procColumnSQL(func(i int, _ procColumn) string { return fmt.Sprintf("$%d", n+i) })
}

// procValues are a reading's column values, in columns order; all NULL
// without one.
func procValues(u *proto.ProcessUsage) []any {
	var p procRow
	if u != nil {
		p = procRow{&u.Started, &u.CPUSeconds, &u.RSSBytes, u.PeakRSSBytes, &u.HeapBytes, &u.Goroutines}
	}
	return p.dest()
}

// sample is the row's process, its CPU a rate against the previous row of
// the same start; nil when the row has none (and the rate skips it).
func (p *procRow) sample(r *procRate, at time.Time) *ProcessSample {
	if p.started == nil {
		return nil
	}
	return &ProcessSample{Started: *p.started, CPUCores: r.next(*p.started, at, p.cpu), RSSBytes: p.rss, PeakRSSBytes: p.peak,
		HeapBytes: p.heap, Goroutines: p.goroutines}
}

// procRate is a rate over a process's counter, which a restart starts
// again: a new start begins a new series.
type procRate struct {
	started time.Time
	rate
}

func (p *procRate) next(started, at time.Time, v *float64) *float64 {
	if !started.Equal(p.started) {
		p.started, p.rate = started, rate{}
	}
	return p.rate.next(at, v)
}

// History is a series over [From, To] at Resolution seconds (0: raw).
type History struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Resolution int       `json:"resolution"`
	Samples    []Sample  `json:"samples"`
	// The control host: only for an operator reading the whole system.
	Control *Control `json:"control,omitempty" doc:"The machines luxd runs on, its Postgres, and each luxd process. Only an operator key reading the whole system (no tenant) gets it."`
}

// pickResolution is the finest resolution still kept for all of [from, to]
// with at most ~2000 points (raw samples come every ~10s).
func (s *Server) pickResolution(from, to time.Time) int {
	span := to.Sub(from)
	for _, r := range s.resolutions() {
		step := time.Duration(r.res) * time.Second
		if r.res == 0 {
			step = 10 * time.Second
		}
		if time.Since(from) <= r.keep && span/step <= 2000 {
			return r.res
		}
	}
	return 3600
}

// HistoryQuery is the range of a history read.
type HistoryQuery struct {
	TenantQuery
	Since string `query:"since" doc:"How far back from now: a Go duration or a number of days (7d). Default 1h." example:"24h"`
	From  string `query:"from" doc:"The start (RFC 3339), instead of since."`
	To    string `query:"to" doc:"The end (RFC 3339); default now."`
	Res   string `query:"res" doc:"Resolution in seconds: 0 (raw), 60 or 3600. Default: the finest kept for the range."`
}

// historyRange resolves a HistoryQuery: since (default 1h) or from/to, and
// the resolution asked for or the finest kept for the range.
func (s *Server) historyRange(q HistoryQuery) (from, to time.Time, res int, err error) {
	to = time.Now()
	if v := q.To; v != "" {
		if to, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return from, to, 0, errf(http.StatusBadRequest, "bad_request", "to: %v", err)
		}
	}
	since := time.Hour
	if v := q.Since; v != "" {
		if since, err = parseSince(v); err != nil {
			return from, to, 0, errf(http.StatusBadRequest, "bad_request", "since: %v", err)
		}
	}
	from = to.Add(-since)
	if v := q.From; v != "" {
		if from, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return from, to, 0, errf(http.StatusBadRequest, "bad_request", "from: %v", err)
		}
	}
	res = s.pickResolution(from, to)
	switch v := q.Res; v {
	case "":
	case "0", "60", "3600":
		fmt.Sscan(v, &res)
	default:
		return from, to, 0, errf(http.StatusBadRequest, "bad_request", "res must be 0, 60 or 3600")
	}
	return from, to, res, nil
}

// parseSince is a Go duration, or a number of days ("7d").
func parseSince(v string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(v, "d"); ok {
		var n int
		if _, err := fmt.Sscan(d, &n); err != nil || n <= 0 {
			return 0, fmt.Errorf("bad duration %q", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q", v)
	}
	return d, nil
}

// rate turns a cumulative counter into a per-second rate against the
// previous point.
type rate struct {
	at time.Time
	v  *float64
}

func (p *rate) next(at time.Time, v *float64) *float64 {
	defer func() { p.at, p.v = at, v }()
	if v == nil || p.v == nil || !at.After(p.at) || *v < *p.v {
		return nil
	}
	r := (*v - *p.v) / at.Sub(p.at).Seconds()
	return &r
}

// Float is a number as a *float64, for chart series; nil stays nil.
func Float[T ~int | ~int64](v *T) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

type hostHistoryInput struct {
	HostPath
	HistoryQuery
}

type historyOutput struct {
	Body History
}

// hostHistory is GET /v1/hosts/{id}/history. Only operators and the
// host's own tenant see it: a platform host's use is every tenant's.
func (s *Server) hostHistory(ctx context.Context, in *hostHistoryInput) (*historyOutput, error) {
	p := principal(ctx)
	from, to, res, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	h := History{From: from, To: to, Resolution: res, Samples: []Sample{}}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, true)
		if err != nil {
			return err
		}
		if !p.Operator {
			var own bool
			if err := tx.QueryRow(ctx, `SELECT tenant_id IS NOT DISTINCT FROM $2 FROM hosts WHERE id = $1`, id, p.TenantID).Scan(&own); err != nil {
				return err
			}
			if !own {
				return errf(http.StatusForbidden, "forbidden", "a platform host's history is the operators'")
			}
		}
		rows, err := tx.Query(ctx, `SELECT at, cpu_seconds, mem_bytes, disk_bytes, placements, alloc_cpus, alloc_mem, `+procCols+`
			FROM host_samples WHERE host_id = $1 AND res = $2 AND at BETWEEN $3 AND $4 ORDER BY at`, id, res, from, to)
		if err != nil {
			return err
		}
		var cpu rate
		var runnerCPU procRate
		var at time.Time
		var cpuS *float64
		var mem, disk, allocM *int64
		var n *int
		var allocC *float64
		var proc procRow
		_, err = pgx.ForEachRow(rows, append([]any{&at, &cpuS, &mem, &disk, &n, &allocC, &allocM}, proc.dest()...), func() error {
			h.Samples = append(h.Samples, Sample{At: at, CPUCores: cpu.next(at, cpuS), MemoryBytes: mem, DiskBytes: disk,
				Placements: n, AllocCPUs: allocC, AllocMem: allocM, Runner: proc.sample(&runnerCPU, at)})
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &historyOutput{h}, nil
}

type runHistoryInput struct {
	RunPath
	HistoryQuery
}

// runHistory is GET /v1/runs/{id}/history: every placement's samples, in
// time order (each carries its epoch; rates restart with each placement).
func (s *Server) runHistory(ctx context.Context, in *runHistoryInput) (*historyOutput, error) {
	p := principal(ctx)
	from, to, res, err := s.historyRange(in.HistoryQuery)
	if err != nil {
		return nil, err
	}
	h := History{From: from, To: to, Resolution: res, Samples: []Sample{}}
	err = s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT epoch, at, cpu_seconds, mem_bytes, disk_bytes, pids, net_rx, net_tx
			FROM placement_samples WHERE run_id = $1 AND res = $2 AND at BETWEEN $3 AND $4 ORDER BY epoch, at`,
			in.ID, res, from, to)
		if err != nil {
			return err
		}
		var cpu, rx, tx_ rate
		var epoch, lastEpoch int
		var at time.Time
		var cpuS *float64
		var mem, disk, nrx, ntx *int64
		var pids *int
		_, err = pgx.ForEachRow(rows, []any{&epoch, &at, &cpuS, &mem, &disk, &pids, &nrx, &ntx}, func() error {
			if epoch != lastEpoch {
				cpu, rx, tx_, lastEpoch = rate{}, rate{}, rate{}, epoch
			}
			ep := epoch
			h.Samples = append(h.Samples, Sample{At: at, Epoch: &ep, CPUCores: cpu.next(at, cpuS), MemoryBytes: mem, DiskBytes: disk,
				Pids: pids, NetRxRate: rx.next(at, Float(nrx)), NetTxRate: tx_.next(at, Float(ntx))})
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &historyOutput{h}, nil
}

// systemHistory is GET /v1/history: the system's samples, or a tenant's
// (its own key's, or an operator's ?tenant=). The whole system's, read by
// an operator, also carry the control host.
func (s *Server) systemHistory(ctx context.Context, in *HistoryQuery) (*historyOutput, error) {
	p := principal(ctx)
	from, to, res, err := s.historyRange(*in)
	if err != nil {
		return nil, err
	}
	h := History{From: from, To: to, Resolution: res, Samples: []Sample{}}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT at, runs, busy, idle, queued, started, finished, start_p50, start_p95,
				hosts, cap_cpus, cap_mem, alloc_cpus, alloc_mem
			FROM system_samples WHERE tenant_id = $1 AND res = $2 AND at BETWEEN $3 AND $4 ORDER BY at`, p.TenantID, res, from, to)
		if err != nil {
			return err
		}
		h.Samples, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Sample, error) {
			var sm Sample
			err := row.Scan(&sm.At, &sm.Runs, &sm.Busy, &sm.Idle, &sm.Queued, &sm.Started, &sm.Finished, &sm.StartP50, &sm.StartP95,
				&sm.Hosts, &sm.CapCPUs, &sm.CapMem, &sm.SysAllocC, &sm.SysAllocM)
			return sm, err
		})
		if err != nil || !controlVisible(p) {
			return err
		}
		h.Control, err = readControl(ctx, tx, res, from, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &historyOutput{h}, nil
}
