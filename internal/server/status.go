package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Tenant is a tenant as operators see it: its limits and what it uses.
type Tenant struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	RetentionDays     int       `json:"retentionDays" doc:"Days a terminated Run keeps its snapshots and output after it was terminated. Artifacts are kept until deleted."`
	ExpireAfterDays   int       `json:"expireAfterDays" doc:"Days a stopped, lost, failed or succeeded Run may rest before lux terminates it; 0: never."`
	MaxConcurrentRuns *int      `json:"maxConcurrentRuns,omitempty"`
	MaxHosts          *int      `json:"maxHosts,omitempty"`
	MaxStorageBytes   *int64    `json:"maxStorageBytes,omitempty"`
	ActiveRuns        int       `json:"activeRuns"`
	Runs              int       `json:"runs"`
	Hosts             int       `json:"hosts"`
	StoredBytes       int64     `json:"storedBytes"`
	CreatedAt         time.Time `json:"createdAt"`
}

// listTenants is for operators: every tenant, or the one ?tenant= names.
type listTenantsOutput struct {
	Body struct {
		Tenants []Tenant `json:"tenants"`
	} `nameHint:"TenantList"`
}

func (s *Server) listTenants(ctx context.Context, _ *TenantQuery) (*listTenantsOutput, error) {
	p := principal(ctx)
	tenants := []Tenant{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id, t.name, t.retention_days, t.expire_after_days, t.max_concurrent_runs, t.max_hosts, t.max_storage_bytes,
				(SELECT count(*) FROM runs WHERE tenant_id = t.id AND state NOT IN `+inactiveRunStates+`),
				(SELECT count(*) FROM runs WHERE tenant_id = t.id),
				(SELECT count(*) FROM hosts WHERE tenant_id = t.id AND state <> 'terminated'),
				(SELECT coalesce(sum(size), 0) FROM blobs WHERE tenant_id = t.id AND location <> 'deleted'),
				t.created_at
			FROM tenants t WHERE $1 = '' OR t.id = $1 ORDER BY t.name`, p.TenantID)
		if err != nil {
			return err
		}
		tenants, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tenant, error) {
			var t Tenant
			err := row.Scan(&t.ID, &t.Name, &t.RetentionDays, &t.ExpireAfterDays, &t.MaxConcurrentRuns, &t.MaxHosts, &t.MaxStorageBytes,
				&t.ActiveRuns, &t.Runs, &t.Hosts, &t.StoredBytes, &t.CreatedAt)
			return t, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &listTenantsOutput{}
	out.Body.Tenants = tenants
	return out, nil
}

// Status is the state of the system (or of one tenant's part of it) now.
type Status struct {
	// Runs by state; Busy and Idle split the running ones by activity.
	Runs map[string]int `json:"runs"`
	Busy int            `json:"busy"`
	Idle int            `json:"idle"`
	// Queued: Runs waiting for a host; OldestQueuedAt, since when.
	Queued         int        `json:"queued"`
	OldestQueuedAt *time.Time `json:"oldestQueuedAt,omitempty"`
	// Seconds from submission to first start, for Runs first started in
	// the last hour.
	StartLatency Percentiles `json:"startLatency"`
	// Hosts by state, over the hosts the caller sees.
	Hosts map[string]int `json:"hosts"`
	// Capacity of ready and draining hosts, and what live placements hold
	// (a tenant: its own placements only).
	Capacity  Totals `json:"capacity"`
	Allocated Totals `json:"allocated"`
}

type Percentiles struct {
	N   int      `json:"n"`
	P50 *float64 `json:"p50,omitempty"`
	P95 *float64 `json:"p95,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

type Totals struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk"`
	Runs   int     `json:"runs"`
}

// status is GET /v1/status: a tenant's view of its Runs and the hosts it
// can use, or an operator's of everything (?tenant= narrows it).
type statusNowOutput struct {
	Body Status
}

func (s *Server) status(ctx context.Context, _ *TenantQuery) (*statusNowOutput, error) {
	p := principal(ctx)
	st := Status{Runs: map[string]int{}, Hosts: map[string]int{}}
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT state, activity, count(*) FROM runs GROUP BY state, activity`)
		if err != nil {
			return err
		}
		var state, activity string
		var n int
		_, err = pgx.ForEachRow(rows, []any{&state, &activity, &n}, func() error {
			st.Runs[state] += n
			if state == StateRunning {
				switch activity {
				case "busy":
					st.Busy += n
				case "idle":
					st.Idle += n
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), min(updated_at) FROM runs
			WHERE state IN `+queuedRunStates).Scan(&st.Queued, &st.OldestQueuedAt); err != nil {
			return err
		}
		l := &st.StartLatency
		return tx.QueryRow(ctx, `SELECT count(*),
				percentile_cont(0.5) WITHIN GROUP (ORDER BY d), percentile_cont(0.95) WITHIN GROUP (ORDER BY d), max(d)
			FROM (SELECT extract(epoch FROM first_started_at - created_at)::float8 AS d FROM runs
				WHERE first_started_at > now() - interval '1 hour') x`).Scan(&l.N, &l.P50, &l.P95, &l.Max)
	})
	if err != nil {
		return nil, err
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT h.state, count(*),
				coalesce(sum((h.capacity->>'cpus')::float8), 0), coalesce(sum((h.capacity->>'memory')::int8), 0),
				coalesce(sum((h.capacity->>'disk')::int8), 0), coalesce(sum((h.capacity->>'runs')::int), 0)
			FROM hosts h WHERE `+visibleHosts+` AND h.state <> 'terminated' GROUP BY h.state`, p.TenantID)
		if err != nil {
			return err
		}
		var state string
		var n int
		var c Totals
		_, err = pgx.ForEachRow(rows, []any{&state, &n, &c.CPUs, &c.Memory, &c.Disk, &c.Runs}, func() error {
			st.Hosts[state] = n
			if state == "ready" || state == "draining" {
				st.Capacity.CPUs += c.CPUs
				st.Capacity.Memory += c.Memory
				st.Capacity.Disk += c.Disk
				st.Capacity.Runs += c.Runs
			}
			return nil
		})
		if err != nil {
			return err
		}
		a := &st.Allocated
		return tx.QueryRow(ctx, `SELECT coalesce(sum((pl.resources->>'cpus')::float8), 0), coalesce(sum((pl.resources->>'memory')::int8), 0),
				coalesce(sum((pl.resources->>'disk')::int8), 0), count(*)
			FROM placements pl JOIN hosts h ON h.id = pl.host_id
			WHERE `+visibleHosts+` AND `+visiblePlacements+` AND pl.state IN `+livePlacementStates, p.TenantID).Scan(&a.CPUs, &a.Memory, &a.Disk, &a.Runs)
	})
	if err != nil {
		return nil, err
	}
	return &statusNowOutput{st}, nil
}
