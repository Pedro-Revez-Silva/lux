package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Backfilling block storage (docs/costs.md, section 5): hosts launched
// before luxd recorded volumes have none, so their block storage is
// missing. An operator states the launch template's disk; luxd records it
// on the pool's hosts whose volumes are unknown, marked assumed, opens
// their block-storage periods at the current list price, and re-evaluates
// their Runs: compute snapshots stay frozen as they are, block-storage
// snapshots are added and frozen. Only a family that did not exist is
// added; no frozen amount is revised.

// BackfillVolumes is what luxd admin costs backfill-volumes asks for.
type BackfillVolumes struct {
	Pool    string
	Tenant  string // id or name; empty: the platform's pool
	Volumes []HostVolume
	DryRun  bool
}

// BackfillReport is what a backfill did, or would do (DryRun).
type BackfillReport struct {
	DryRun bool           `json:"dryRun"`
	PoolID string         `json:"poolId"`
	Hosts  []BackfillHost `json:"hosts"`
	// Hours: host-hours within cost_hourly retention whose rows are rebuilt.
	Hours int `json:"hours"`
	// Runs: Runs re-evaluated (placed on those hosts).
	Runs int `json:"runs"`
	// Skipped: Runs placed on those hosts that are not re-evaluated.
	Skipped []BackfillSkipped `json:"skipped"`
}

type BackfillSkipped struct {
	Run    string `json:"run"`
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// skipLegacyCompute is why a Run is not re-evaluated (legacyComputeRuns).
const skipLegacyCompute = "its compute is final without a finalized compute snapshot for every placement (it went final before migration 027); re-evaluating it would reprice that compute"

// BackfillHost is one host a backfill records volumes on: its billed window,
// the period's hourly price, its host-hours within retention, and the Runs
// re-evaluated.
type BackfillHost struct {
	ID      string     `json:"id"`
	From    time.Time  `json:"from"`
	To      *time.Time `json:"to,omitempty"`
	PerHour string     `json:"perHour"`
	Hours   int        `json:"hours"`
	Runs    []string   `json:"runs"`
}

// backfillBatch hosts are read per query; each host is written in a
// transaction of its own.
const backfillBatch = 100

// ParseVolume reads type=gp3,size=100,iops=3000,throughput=125 (size in
// GiB, throughput in MiB/s; iops and throughput optional).
func ParseVolume(s string) (HostVolume, error) {
	var v HostVolume
	for _, part := range strings.Split(s, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return v, fmt.Errorf("volume %q: want key=value pairs", s)
		}
		var n int64
		if k != "type" {
			if _, err := fmt.Sscan(val, &n); err != nil || n < 0 {
				return v, fmt.Errorf("volume %q: %s must be a non-negative integer", s, k)
			}
		}
		switch k {
		case "type":
			v.Type = val
		case "size":
			v.SizeGiB = n
		case "iops":
			v.IOPS = n
		case "throughput":
			v.ThroughputMiBps = n
		default:
			return v, fmt.Errorf("volume %q: unknown key %q (type, size, iops, throughput)", s, k)
		}
	}
	if _, ok := ebsTypes[v.Type]; !ok {
		return v, fmt.Errorf("volume %q: unknown volume type %q", s, v.Type)
	}
	if v.SizeGiB <= 0 {
		return v, fmt.Errorf("volume %q: size is required", s)
	}
	v.Assumed = true
	return v, nil
}

// BackfillVolumes records req.Volumes on the pool's provider hosts whose
// volumes are unknown, opens their block-storage periods, re-queues their
// Runs and rewinds their host-hour refresh. Idempotent: a host it recorded
// has volumes, so a second run finds nothing to do.
func (s *Server) BackfillVolumes(ctx context.Context, req BackfillVolumes) (BackfillReport, error) {
	rep := BackfillReport{DryRun: req.DryRun, Hosts: []BackfillHost{}, Skipped: []BackfillSkipped{}}
	if len(req.Volumes) == 0 {
		return rep, errors.New("at least one --volume is required")
	}
	vols := sortedVolumes(req.Volumes)
	for i := range vols {
		vols[i].Assumed = true
	}
	var provider string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT p.id, p.provider FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id
			WHERE p.name = $1 AND (($2 = '' AND p.tenant_id IS NULL) OR t.id = $2 OR t.name = $2)`, req.Pool, req.Tenant).Scan(&rep.PoolID, &provider)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no pool %q for tenant %q", req.Pool, req.Tenant)
		}
		return err
	})
	if err != nil {
		return rep, err
	}
	if provider == "static" {
		return rep, fmt.Errorf("pool %q is static: its hosts have no provider volumes", req.Pool)
	}
	prices := map[string]map[string]BlockStoragePrice{} // region → type →
	oldest := time.Now().UTC().Add(-s.cfg.Costs.Hourly).Truncate(time.Hour).Add(time.Hour)
	runs := map[string]bool{}
	after := ""
	for {
		type candidate struct {
			ID, Region string
			From       time.Time
			To         *time.Time
		}
		var batch []candidate
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id, coalesce(launch_template->>'region', ''), provision_requested_at, terminated_at
				FROM hosts WHERE pool_id = $1 AND volumes IS NULL AND provider_id IS NOT NULL AND provision_requested_at IS NOT NULL
					AND id > $2 ORDER BY id LIMIT $3`, rep.PoolID, after, backfillBatch)
			if err != nil {
				return err
			}
			batch, err = pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
			return err
		})
		if err != nil {
			return rep, err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].ID
		for _, h := range batch {
			if prices[h.Region] == nil {
				prices[h.Region] = map[string]BlockStoragePrice{}
				for _, v := range vols {
					if _, ok := prices[h.Region][v.Type]; ok {
						continue
					}
					p, err := s.blockStoragePrice(ctx, provider, h.Region, v.Type)
					if err != nil {
						return rep, fmt.Errorf("host %s: %w", h.ID, err)
					}
					prices[h.Region][v.Type] = p
				}
			}
			hourly, _, err := blockStorageHourly(vols, prices[h.Region])
			if err != nil {
				return rep, fmt.Errorf("host %s: %w", h.ID, err)
			}
			bh := BackfillHost{ID: h.ID, From: h.From, To: h.To, PerHour: moneyString(hourly), Runs: []string{}}
			end := time.Now().UTC()
			if h.To != nil {
				end = *h.To
			}
			for at := maxTime(h.From.UTC().Truncate(time.Hour), oldest); at.Before(end); at = at.Add(time.Hour) {
				bh.Hours++
			}
			var skipped []string
			err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				if !req.DryRun {
					var err error
					bh.Runs, skipped, err = s.backfillHost(ctx, tx, h.ID, vols, prices[h.Region], provider)
					return err
				}
				runs, err := hostRuns(ctx, tx, h.ID)
				if err != nil {
					return err
				}
				if skipped, err = legacyComputeRuns(ctx, tx, runs); err != nil {
					return err
				}
				bh.Runs = without(runs, skipped)
				return nil
			})
			if err != nil {
				return rep, fmt.Errorf("host %s: %w", h.ID, err)
			}
			if bh.Runs == nil {
				bh.Runs = []string{}
			}
			for _, r := range bh.Runs {
				runs[r] = true
			}
			for _, r := range skipped {
				rep.Skipped = append(rep.Skipped, BackfillSkipped{Run: r, Host: h.ID, Reason: skipLegacyCompute})
			}
			rep.Hours += bh.Hours
			rep.Hosts = append(rep.Hosts, bh)
		}
	}
	rep.Runs = len(runs)
	return rep, nil
}

// backfillHost writes one host's backfill in tx, in the documented lock
// order (lockBlockStorageHost): its Runs, its cost-host lock, then the host.
// It returns the Runs it re-queues and those it skips (legacyComputeRuns).
func (s *Server) backfillHost(ctx context.Context, tx pgx.Tx, hostID string, vols []HostVolume, prices map[string]BlockStoragePrice, provider string) (requeued, skipped []string, err error) {
	runs, err := lockBlockStorageHost(ctx, tx, hostID)
	if err != nil {
		return nil, nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE hosts SET volumes = $2 WHERE id = $1 AND volumes IS NULL`, hostID, vols)
	if err != nil || tag.RowsAffected() == 0 {
		return nil, nil, err
	}
	opened, err := openBlockStorageRate(ctx, tx, hostID, prices, provider+"-ebs-pricing", s.cfg.Costs.Hourly)
	if err != nil || !opened {
		// Not opened: the host never registered (no capacity to share, as
		// for its compute), so it has no placement to re-evaluate.
		return nil, nil, err
	}
	skipped, err = requeueBlockStorageRuns(ctx, tx, runs)
	return without(runs, skipped), skipped, err
}

// lockBlockStorageHost takes, before hostID's row is locked, what opening its
// block-storage period needs held: the Runs placed on it (FOR UPDATE, id
// order), then its cost-host lock (infraevents.go's order). It returns
// those Runs.
func lockBlockStorageHost(ctx context.Context, tx pgx.Tx, hostID string) ([]string, error) {
	runs, err := hostRuns(ctx, tx, hostID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runs); err != nil {
		return nil, err
	}
	return runs, lockCostHost(ctx, tx, hostID)
}

// hostRuns is the Runs ever placed on hostID, in id order.
func hostRuns(ctx context.Context, tx pgx.Tx, hostID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT run_id FROM placements WHERE host_id = $1 ORDER BY run_id`, hostID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// requeueBlockStorageRuns has runs, placed on a host whose block-storage
// period just opened, evaluated again: a final Run's compute source and
// lines go back to ok and not final, and each is queued. Its frozen compute
// snapshots are kept; the new block-storage ones are written and frozen.
// A Run legacyComputeRuns names is left as it is, and returned. The caller
// holds the Runs' locks.
func requeueBlockStorageRuns(ctx context.Context, tx pgx.Tx, runs []string) (skipped []string, err error) {
	if len(runs) == 0 {
		return nil, nil
	}
	if skipped, err = legacyComputeRuns(ctx, tx, runs); err != nil {
		return nil, err
	}
	runs = without(runs, skipped)
	if _, err := tx.Exec(ctx, `UPDATE cost_sources SET status = 'ok', next_at = NULL, attempts = 0
		WHERE run_id = ANY($1) AND source = 'compute' AND status = 'final'`, runs); err != nil {
		return skipped, err
	}
	if _, err := tx.Exec(ctx, `UPDATE cost_lines SET final = false WHERE run_id = ANY($1) AND source = 'compute' AND final`, runs); err != nil {
		return skipped, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO cost_pending (run_id, due_at, reason)
		SELECT id, now(), 'retry' FROM runs WHERE id = ANY($1) ORDER BY id
		ON CONFLICT (run_id) DO UPDATE SET due_at = least(cost_pending.due_at, EXCLUDED.due_at),
			reason = EXCLUDED.reason, claimed_by = NULL, claimed_until = NULL`, runs)
	return skipped, err
}

// legacyComputeRuns are those of runs whose compute is final while some
// placement has no finalized compute snapshot: Runs that went final before
// snapshots existed (migration 027). Evaluating one again would price that
// compute anew (writeFamilySnapshots), so it is never re-queued for block
// storage.
func legacyComputeRuns(ctx context.Context, tx pgx.Tx, runs []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT src.run_id FROM cost_sources src
		WHERE src.run_id = ANY($1) AND src.source = 'compute' AND src.status = 'final'
			AND EXISTS (SELECT 1 FROM placements p
				LEFT JOIN cost_placement_snapshots s ON s.placement_id = p.id AND s.family = 'compute'
				WHERE p.run_id = src.run_id AND NOT coalesce(s.finalized, false))
		ORDER BY src.run_id`, runs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// without is runs less those in drop, in runs' order.
func without(runs, drop []string) []string {
	out := []string{}
	for _, r := range runs {
		if !slices.Contains(drop, r) {
			out = append(out, r)
		}
	}
	return out
}
