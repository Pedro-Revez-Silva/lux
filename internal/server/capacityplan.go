package server

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
)

// capacityPlan is a transient reservation simulation, never a placement promise.
type capacityPlan struct {
	Ready            int    `json:"ready"`
	Future           int    `json:"future"` // Runs covered by existing starts only.
	Planned          int    `json:"planned"`
	Unmet            int    `json:"unmet"`   // Capacity-eligible Runs not covered by ready, existing-start, or planned capacity.
	Blocked          int    `json:"blocked"` // Runs excluded before capacity simulation; they never trigger launches.
	Unknown          string `json:"unknown,omitempty"`
	hostDecisions    map[string]map[string]any
	NewHosts         int              `json:"newHosts"`
	Expected         *hostExpectation `json:"expected"`
	Deficits         []planDeficit    `json:"deficits,omitempty"`  // Prerequisite and new-host blockers.
	Exhausted        []planDeficit    `json:"exhausted,omitempty"` // Fit blockers on actual ready or starting hosts.
	Omitted          int              `json:"omitted,omitempty"`   // Evidence entries beyond planSampleSize per list.
	Ineligible       []ineligibleHost `json:"ineligible,omitempty"`
	reserved         map[string]bool
	reservedIdle     int
	reservedStarting int
}

type hostExpectation struct {
	Capacity     proto.Capacity    `json:"capacity"`
	Labels       map[string]string `json:"-"`
	Observations int               `json:"observations"`
}

const planSampleSize = 8

type ineligibleHost struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// ineligibleReadyHosts samples the pool's ready hosts the scheduler would not
// consider (draining or past LeaseDuration without a heartbeat), so absent
// ready capacity has a stated cause.
func (s *Server) ineligibleReadyHosts(ctx context.Context, tx pgx.Tx, pl poolRow, plan *capacityPlan) error {
	rows, err := tx.Query(ctx, `SELECT id, CASE WHEN draining THEN 'draining'
			WHEN last_heartbeat IS NULL THEN 'no heartbeat' ELSE 'heartbeat stale' END
		FROM hosts WHERE pool_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::text AND state = 'ready'
		  AND (draining OR last_heartbeat IS NULL OR last_heartbeat <= now() - $3::interval)
		ORDER BY id LIMIT $4`, pl.ID, pl.TenantID, interval(s.cfg.LeaseDuration), planSampleSize)
	if err != nil {
		return err
	}
	plan.Ineligible, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ineligibleHost])
	for _, h := range plan.Ineligible {
		plan.hostDecisions[h.Host] = map[string]any{"stage": "ready", "decision": "ineligible", "reason": h.Reason}
	}
	return err
}

type planDeficit struct {
	Run      string        `json:"run"`
	Host     string        `json:"host,omitempty"`
	Stage    string        `json:"stage,omitempty"`
	Blockers []planBlocker `json:"blockers"`
}

// planBlocker is fitBlocker with resource numbers kept when zero (available 0
// is the usual exhaustion value) and without label values.
type planBlocker struct {
	Resource  string   `json:"resource,omitempty"`
	Requested *float64 `json:"requested,omitempty"`
	Used      *float64 `json:"used,omitempty"`
	Capacity  *float64 `json:"capacity,omitempty"`
	Available *float64 `json:"available,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

func diagnosticBlockers(blockers []fitBlocker) []planBlocker {
	out := make([]planBlocker, 0, len(blockers))
	for _, b := range blockers {
		if b.Resource != "" {
			out = append(out, planBlocker{Resource: b.Resource, Requested: &b.Requested, Used: &b.Used, Capacity: &b.Capacity, Available: &b.Available})
			continue
		}
		reason := b.Reason
		if strings.HasPrefix(reason, "requires label ") {
			reason = "required labels do not match"
		}
		out = append(out, planBlocker{Reason: reason})
	}
	return out
}

func finiteMinimum[T ~int | ~int64 | ~float64](a, b T) T {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	return min(a, b)
}

func (s *Server) hostExpectation(ctx context.Context, tx pgx.Tx, pl poolRow) (*hostExpectation, error) {
	rows, err := tx.Query(ctx, `SELECT capacity, labels FROM hosts
		WHERE pool_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::text
		  AND provision_requested_at IS NOT NULL AND registered_at IS NOT NULL
		  AND launch_template = $3::jsonb ORDER BY id`, pl.ID, pl.TenantID, pl.Template)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expected *hostExpectation
	for rows.Next() {
		var capacity proto.Capacity
		var labels map[string]string
		if err := rows.Scan(&capacity, &labels); err != nil {
			return nil, err
		}
		if expected == nil {
			expected = &hostExpectation{Capacity: capacity, Labels: labels}
		} else {
			c := &expected.Capacity
			c.CPUs = finiteMinimum(c.CPUs, capacity.CPUs)
			c.Memory = finiteMinimum(c.Memory, capacity.Memory)
			c.Disk = finiteMinimum(c.Disk, capacity.Disk)
			c.Runs = finiteMinimum(c.Runs, capacity.Runs)
			for k, v := range expected.Labels {
				if other, ok := labels[k]; !ok || other != v {
					delete(expected.Labels, k)
				}
			}
		}
		expected.Observations++
	}
	return expected, rows.Err()
}

func (s *Server) planCapacity(ctx context.Context, tx pgx.Tx, pl poolRow) (capacityPlan, error) {
	plan := capacityPlan{reserved: map[string]bool{}, hostDecisions: map[string]map[string]any{}}
	blockedHosts, reservedOn := map[string]bool{}, map[string]int{}
	evidence := func(r pendingRun, host, stage string, fit []fitBlocker) {
		blockers := diagnosticBlockers(fit)
		if host == "" {
			if len(plan.Deficits) < planSampleSize {
				plan.Deficits = append(plan.Deficits, planDeficit{Run: r.ID, Stage: stage, Blockers: blockers})
			} else {
				plan.Omitted++
			}
			return
		}
		// The first blocking Run per host represents it: one entry and one decision per host.
		if _, seen := blockedHosts[host]; seen {
			return
		}
		blockedHosts[host] = true
		if len(plan.Exhausted) < planSampleSize {
			plan.Exhausted = append(plan.Exhausted, planDeficit{Run: r.ID, Host: host, Stage: stage, Blockers: blockers})
		} else {
			plan.Omitted++
		}
		decision := "blocked"
		if reservedOn[host] > 0 {
			decision = "exhausted"
		}
		plan.hostDecisions[host] = map[string]any{"stage": stage, "decision": decision, "blockers": blockers}
	}
	unsatisfied := func(r pendingRun, reason string) {
		plan.Blocked++
		evidence(r, "", "prerequisite", []fitBlocker{{Reason: reason}})
	}
	var err error
	plan.Expected, err = s.hostExpectation(ctx, tx, pl)
	if err != nil {
		return plan, err
	}
	if plan.Expected == nil {
		plan.Unknown = "no registered host observations for current template"
	}
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, state, spec, snapshot_id,
		jsonb_array_length(secrets) > 0, coalesce(place_on, ''), coalesce(avoid_host, ''), pool_id
		FROM runs WHERE pool_id = $1 AND state = 'provisioning' AND NOT cancel_requested
		ORDER BY updated_at, id`, pl.ID)
	if err != nil {
		return plan, err
	}
	var runs []pendingRun
	for rows.Next() {
		var r pendingRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.State, &r.Spec, &r.SnapshotID, &r.HasSecrets, &r.PlaceOn, &r.AvoidHost, &r.PoolID); err != nil {
			rows.Close()
			return plan, err
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}
	pools, tenants, chosen := make([]*string, 0, len(runs)), make([]string, 0, len(runs)), make([]string, 0, len(runs))
	for _, r := range runs {
		pools = append(pools, r.PoolID)
		tenants = append(tenants, r.TenantID)
		chosen = append(chosen, r.PlaceOn)
	}
	ids, err := s.eligibleHostIDs(ctx, tx, pools, tenants, chosen)
	if err != nil {
		return plan, err
	}
	ready, err := s.candidateHosts(ctx, tx, ids)
	if err != nil {
		return plan, err
	}
	// The hub only knows runners connected to this luxd; a fresh heartbeat
	// (required by candidateHosts) is the cluster-wide liveness signal.
	for _, h := range ready {
		h.Connected = true
	}
	slices.SortFunc(ready, func(a, b *candidateHost) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	if len(runs) > 0 {
		if err := s.ineligibleReadyHosts(ctx, tx, pl, &plan); err != nil {
			return plan, err
		}
	}
	// Only current-template, non-expired starts represent future capacity.
	rows, err = tx.Query(ctx, `SELECT id FROM hosts WHERE pool_id = $1
		AND tenant_id IS NOT DISTINCT FROM $2::text AND state = 'provisioning' AND NOT draining
		AND provision_requested_at > now() - $3::interval AND launch_template = $4::jsonb ORDER BY id`,
		pl.ID, pl.TenantID, interval(s.cfg.LaunchTimeout), pl.Template)
	if err != nil {
		return plan, err
	}
	startingIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return plan, err
	}
	var shared bool
	if err := tx.QueryRow(ctx, `SELECT shared AND tenant_id IS NULL FROM pools WHERE id = $1`, pl.ID).Scan(&shared); err != nil {
		return plan, err
	}
	virtual := func(id string) *candidateHost {
		h := &candidateHost{ID: id, TenantID: pl.TenantID, PoolID: pl.ID, Pool: pl.Name, Shared: shared, Retired: pl.Retired, Connected: true}
		if plan.Expected != nil {
			h.Capacity = plan.Expected.Capacity
			h.Labels = plan.Expected.Labels
		}
		return h
	}
	var future []*candidateHost
	if plan.Expected != nil {
		for _, id := range startingIDs {
			future = append(future, virtual(id))
		}
	}
	usedStarting := map[string]bool{}
	reserve := func(hosts []*candidateHost, r pendingRun, isReady bool) bool {
		for _, h := range hosts {
			stage := "starting"
			if isReady {
				stage = "ready"
			} else if h.ID == "" {
				stage = "planned"
			}
			if blockers := hostFit(r, h); len(blockers) != 0 {
				// Another host's chosen-host wait is not evidence about this host.
				if stage != "planned" && (r.PlaceOn == "" || r.PlaceOn == h.ID) {
					evidence(r, h.ID, stage, blockers)
				}
				continue
			}
			reserveHost(h, r)
			if h.ID != "" {
				reservedOn[h.ID]++
				if !blockedHosts[h.ID] {
					plan.hostDecisions[h.ID] = map[string]any{"stage": stage, "decision": "reserved"}
				}
			}
			if isReady {
				plan.Ready++
				plan.reserved[h.ID] = true
			} else if h.ID != "" {
				plan.Future++
				usedStarting[h.ID] = true
			} else {
				plan.Planned++
			}
			return true
		}
		return false
	}
	for _, r := range runs {
		if _, ok := s.secrets.get(r.ID); r.HasSecrets && !ok {
			unsatisfied(r, "run secrets unavailable")
			continue
		}
		if r.SnapshotID != nil {
			var available, uploaded bool
			if err := tx.QueryRow(ctx, `SELECT available, uploaded FROM snapshots WHERE id = $1`, *r.SnapshotID).Scan(&available, &uploaded); errors.Is(err, pgx.ErrNoRows) {
				unsatisfied(r, "snapshot missing")
				continue
			} else if err != nil {
				return plan, err
			}
			if !available || !uploaded {
				reason := "snapshot upload pending"
				if !available {
					reason = "snapshot unavailable"
				}
				unsatisfied(r, reason)
				continue
			}
			if _, err := restoreManifest(ctx, tx, r.ID, *r.SnapshotID); err != nil {
				var foreign *foreignBlobError
				if errors.As(err, &foreign) {
					unsatisfied(r, "snapshot manifest references unavailable blobs")
					continue
				}
				return plan, err
			}
		}
		if reserve(ready, r, true) {
			continue
		}
		// A chosen-host wait must not trigger launches on unrelated hosts.
		if r.PlaceOn != "" {
			plan.Blocked++
			evidence(r, "", "prerequisite", []fitBlocker{{Reason: "waiting for its chosen host"}})
			continue
		}
		if reserve(future, r, false) {
			continue
		}
		plan.Unmet++
		if plan.Expected == nil {
			evidence(r, "", "new_host", []fitBlocker{{Reason: "new host capacity unknown"}})
			continue
		}
		h := virtual("")
		if blockers := hostFit(r, h); len(blockers) != 0 {
			evidence(r, "", "new_host", blockers)
			continue
		}
		reserveHost(h, r)
		future = append(future, h)
		plan.NewHosts++
		plan.Planned++
		plan.Unmet--
	}
	if plan.Expected == nil && plan.Unmet > 0 && len(startingIDs) == 0 {
		plan.NewHosts = 1
	}
	plan.reservedStarting = len(usedStarting)
	if plan.Expected == nil && plan.Unmet > 0 && len(startingIDs) > 0 {
		plan.reservedStarting = 1
	}
	return plan, nil
}
