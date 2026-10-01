package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
)

// capacityPlan is a transient reservation simulation, never a placement
// promise. summary() is what events carry of it; NewHosts is not exported.
type capacityPlan struct {
	Ready            int
	Starting         int // Runs covered by existing starts only.
	Planned          int
	Unmet            int // Capacity-eligible Runs not covered by ready, existing-start, or planned capacity.
	Blocked          int // Runs excluded before capacity simulation; they never trigger launches.
	Unknown          string
	Probe            bool // One host launched to re-observe capacity no expected host fits.
	hostDecisions    map[string]hostDecision
	NewHosts         int
	Expected         *hostExpectation
	Deficits         []planDeficit // Prerequisite and new-host blockers.
	Exhausted        []planDeficit // Fit blockers on actual ready or starting hosts.
	Omitted          int           // Evidence entries beyond planSampleSize per list.
	Ineligible       []ineligibleHost
	reserved         map[string]bool
	reservedIdle     int
	reservedStarting int
	// awaitingStart: unmet Runs wait for a current-template start to
	// register (a bootstrap or probe in flight), not on a blocker.
	awaitingStart bool
}

// hostDecision is a host.capacity_decision event's data.
type hostDecision struct {
	Pool     string        `json:"pool"`
	Stage    string        `json:"stage"`
	Decision string        `json:"decision"`
	Reason   string        `json:"reason,omitempty"`
	Blockers []planBlocker `json:"blockers,omitempty"`
}

type hostExpectation struct {
	Capacity     proto.Capacity    `json:"capacity"`
	Labels       map[string]string `json:"-"`
	Observations int               `json:"observations"`
	// nestedMismatch: the newest registration lacked nested=true, and so
	// did the one before it, if any (see planCapacity).
	nestedMismatch bool
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
		plan.hostDecisions[h.Host] = hostDecision{Pool: pl.Name, Stage: "ready", Decision: "ineligible", Reason: h.Reason}
	}
	return err
}

// idleDecisions adds decision "idle" for live hosts this pool decided on that
// this plan did not consider and whose latest decision says otherwise, so a verdict from past
// demand does not stay the latest. Those hosts are the ones this process
// recorded a non-idle decision for (s.decided, kept by recordHostDecisions);
// the first pass per pool since the process took the provisioner lease also
// reads them from the database, retracting a previous provisioner's
// verdicts. A decision another process records later is not retracted by
// this one. The idle stage is the host's state, provisioning as starting.
func (s *Server) idleDecisions(ctx context.Context, tx pgx.Tx, pl poolRow, plan *capacityPlan) error {
	decided := s.decidedFor(pl.ID)
	if !s.swept[pl.ID] {
		rows, err := tx.Query(ctx, `SELECT h.id FROM hosts h
			WHERE h.pool_id = $1 AND h.tenant_id IS NOT DISTINCT FROM $2::text AND h.state <> 'terminated'
			  AND (SELECT l.data->>'decision' FROM (SELECT e.id, e.type, e.data FROM host_events e
					WHERE e.host_id = h.id AND (e.host_id, e.id) <= (h.id, 9223372036854775807)
					ORDER BY e.host_id DESC, e.id DESC LIMIT $4) l
				WHERE l.type = $3 ORDER BY l.id DESC LIMIT 1) <> 'idle'`,
			pl.ID, pl.TenantID, evCapacityDecision, transitionWindow)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			decided[id] = true
		}
		s.swept[pl.ID] = true
	}
	var stale []string
	for id := range decided {
		if _, considered := plan.hostDecisions[id]; !considered {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	// A decided host may be outside this pool (a chosen host in another pool,
	// the platform's included, or moved since): it is retracted too, under
	// its current pool's name. stale holds only ids this pool decided on.
	rows, err := tx.Query(ctx, `SELECT h.id, CASE h.state WHEN 'provisioning' THEN 'starting' ELSE h.state END, COALESCE(p.name, '')
		FROM hosts h LEFT JOIN pools p ON p.id = h.pool_id
		WHERE h.id = ANY($1) AND (h.tenant_id IS NULL OR h.tenant_id IS NOT DISTINCT FROM $2::text) AND h.state <> 'terminated'`,
		stale, pl.TenantID)
	if err != nil {
		return err
	}
	type live struct{ ID, Stage, Pool string }
	hosts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[live])
	if err != nil {
		return err
	}
	// A terminated host has nothing to retract.
	for _, id := range stale {
		delete(decided, id)
	}
	for _, h := range hosts {
		decided[h.ID] = true
		plan.hostDecisions[h.ID] = hostDecision{Pool: h.Pool, Stage: h.Stage, Decision: "idle"}
	}
	return nil
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
		switch b.Kind {
		case kindResource:
			out = append(out, planBlocker{Resource: b.Resource, Requested: &b.Requested, Used: &b.Used, Capacity: &b.Capacity, Available: &b.Available})
		case kindLabel:
			out = append(out, planBlocker{Reason: "required labels do not match"})
		default:
			out = append(out, planBlocker{Reason: b.Reason})
		}
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

// templateOffersNested: the pool's runners start with --nested
// (template.nestedContainers, an ec2 template's opt-in). A malformed
// template offers nothing.
func templateOffersNested(pl poolRow) bool {
	if pl.Provider != "ec2" || len(pl.Template) == 0 {
		return false
	}
	var t struct {
		NestedContainers bool `json:"nestedContainers"`
	}
	return json.Unmarshal(pl.Template, &t) == nil && t.NestedContainers
}

// expectationWindow is how many of the latest registrations (per pool, tenant
// and exact template) the new-host expectation is taken from, so a changed
// $Default instance type ages out after that many newer hosts register.
const expectationWindow = 8

// hostExpectation also returns the latest registration time of that window,
// which bounds probe launches (see planCapacity).
func (s *Server) hostExpectation(ctx context.Context, tx pgx.Tx, pl poolRow) (*hostExpectation, time.Time, error) {
	var latest time.Time
	// hosts_pool_registered serves the order, id only breaking ties. The
	// tenant test is spelled out: IS NOT DISTINCT FROM is estimated at one
	// row, and the planner then reads the pool's whole history instead.
	rows, err := tx.Query(ctx, `SELECT capacity, labels, registered_at FROM hosts
		WHERE pool_id = $1 AND (tenant_id = $2::text OR tenant_id IS NULL AND $2::text IS NULL)
		  AND provision_requested_at IS NOT NULL AND registered_at IS NOT NULL
		  AND launch_template = $3::jsonb ORDER BY registered_at DESC, id DESC LIMIT $4`,
		pl.ID, pl.TenantID, pl.Template, expectationWindow)
	if err != nil {
		return nil, latest, err
	}
	defer rows.Close()
	var expected *hostExpectation
	var newestNested [2]bool // nested=true of the two newest, newest first.
	for i := 0; rows.Next(); i++ {
		var capacity proto.Capacity
		var labels map[string]string
		var registered time.Time
		if err := rows.Scan(&capacity, &labels, &registered); err != nil {
			return nil, latest, err
		}
		if i == 0 {
			latest = registered
		}
		if i < len(newestNested) {
			newestNested[i] = labels["nested"] == "true"
		}
		expected = expected.observe(capacity, labels)
	}
	if expected != nil {
		expected.nestedMismatch = !newestNested[0] && (expected.Observations == 1 || !newestNested[1])
	}
	return expected, latest, rows.Err()
}

// observe folds one more registration, newest first, into e (nil before
// the first): the minimum of each capacity, and the labels all share.
func (e *hostExpectation) observe(capacity proto.Capacity, labels map[string]string) *hostExpectation {
	if e == nil {
		e = &hostExpectation{Capacity: capacity, Labels: labels}
	} else {
		c := &e.Capacity
		c.CPUs = finiteMinimum(c.CPUs, capacity.CPUs)
		c.Memory = finiteMinimum(c.Memory, capacity.Memory)
		c.Disk = finiteMinimum(c.Disk, capacity.Disk)
		c.Runs = finiteMinimum(c.Runs, capacity.Runs)
		for k, v := range e.Labels {
			if other, ok := labels[k]; !ok || other != v {
				delete(e.Labels, k)
			}
		}
	}
	e.Observations++
	return e
}

// poolHostSizes is the size of each pool's hosts, as the planner expects
// them (hostExpectation: the minimum of each resource over the latest
// expectationWindow registrations, for a provisioned pool of its current
// template), for pools as listed to clients. One query for all of poolIDs;
// a pool no host ever registered in is absent.
func poolHostSizes(ctx context.Context, tx pgx.Tx, poolIDs []string) (map[string]poolHostSize, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, h.capacity, coalesce(h.instance_type, ''),
			EXISTS (SELECT 1 FROM hosts l WHERE l.pool_id = p.id AND l.state IN ('ready', 'draining'))
		FROM pools p CROSS JOIN LATERAL (SELECT capacity, instance_type, registered_at, id FROM hosts
			WHERE pool_id = p.id AND (tenant_id = p.tenant_id OR tenant_id IS NULL AND p.tenant_id IS NULL)
			  AND registered_at IS NOT NULL
			  AND (p.provider = 'static' OR provision_requested_at IS NOT NULL AND launch_template = p.template)
			ORDER BY registered_at DESC, id DESC LIMIT $2) h
		WHERE p.id = ANY($1) ORDER BY p.id, h.registered_at DESC, h.id DESC`, poolIDs, expectationWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	expected := map[string]*hostExpectation{}
	out := map[string]poolHostSize{}
	for rows.Next() {
		var id, instanceType string
		var capacity proto.Capacity
		var live bool
		if err := rows.Scan(&id, &capacity, &instanceType, &live); err != nil {
			return nil, err
		}
		if _, seen := out[id]; !seen {
			from := HostSizeFromHistory
			if live {
				from = HostSizeFromRunning
			}
			out[id] = poolHostSize{From: from, InstanceType: instanceType}
		}
		expected[id] = expected[id].observe(capacity, nil)
	}
	for id, e := range expected {
		size := out[id]
		size.Size = HostSize{CPUs: e.Capacity.CPUs, Memory: e.Capacity.Memory, Disk: e.Capacity.Disk}
		out[id] = size
	}
	return out, rows.Err()
}

type poolHostSize struct {
	Size               HostSize
	From, InstanceType string
}

// planCapacity plans the pool's waiting Runs; idle is the pool's idle ready
// hosts, of which those it reserves are not surplus (reservedIdle).
func (s *Server) planCapacity(ctx context.Context, tx pgx.Tx, pl poolRow, idle map[string]bool) (capacityPlan, error) {
	plan := capacityPlan{reserved: map[string]bool{}, hostDecisions: map[string]hostDecision{}}
	// Per actual host: the stage it was reserved at, and its first blocker;
	// its decision is settled after every Run was tried.
	type hostBlock struct {
		stage    string
		blockers []planBlocker
	}
	reservedStage, blockedHosts := map[string]string{}, map[string]hostBlock{}
	sample := func(list *[]planDeficit, d planDeficit) {
		if len(*list) < planSampleSize {
			*list = append(*list, d)
		} else {
			plan.Omitted++
		}
	}
	deficit := func(r pendingRun, stage string, fit []fitBlocker) {
		sample(&plan.Deficits, planDeficit{Run: r.ID, Stage: stage, Blockers: diagnosticBlockers(fit)})
	}
	unsatisfied := func(r pendingRun, reason string) {
		plan.Blocked++
		deficit(r, "prerequisite", []fitBlocker{{Reason: reason}})
	}
	// The first blocking Run per host represents it: one entry and one decision per host.
	hostBlocked := func(r pendingRun, host, stage string, fit []fitBlocker) {
		if _, seen := blockedHosts[host]; seen {
			return
		}
		blockers := diagnosticBlockers(fit)
		blockedHosts[host] = hostBlock{stage, blockers}
		sample(&plan.Exhausted, planDeficit{Run: r.ID, Host: host, Stage: stage, Blockers: blockers})
	}
	var err error
	var lastRegistered time.Time
	plan.Expected, lastRegistered, err = s.hostExpectation(ctx, tx, pl)
	if err != nil {
		return plan, err
	}
	if plan.Expected == nil {
		plan.Unknown = "no registered host observations for current template"
	}
	runs, waitingSince, err := waitingRuns(ctx, tx, pl)
	if err != nil {
		return plan, err
	}
	ready, err := s.readyHosts(ctx, tx, runs)
	if err != nil {
		return plan, err
	}
	if len(runs) > 0 {
		if err := s.ineligibleReadyHosts(ctx, tx, pl, &plan); err != nil {
			return plan, err
		}
	}
	// Only current-template, non-expired starts represent future capacity.
	rows, err := tx.Query(ctx, `SELECT id FROM hosts WHERE pool_id = $1
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
	// Whether a future host offers nested containers is the current
	// template's decision, never an older host's label; but registrations
	// of the current template without them (a custom AMI that drops
	// LUX_NESTED) stop its later hosts counting as nested. No single
	// registration flips that either way: the newest must lack nested=true
	// and so must the one before it, unless it is the only one (a broken
	// template's bootstrap). One outlier after a nested host thus does not
	// starve nested Runs, and one late nested host (an older launch
	// registering last) clears it for at most one burst, plus one host if
	// that burst registers one host at a time; its first two registrations
	// without nesting re-establish it. A template edit starts its own
	// observations.
	nested := templateOffersNested(pl)
	nestedMismatch := nested && plan.Expected != nil && plan.Expected.nestedMismatch
	nested = nested && !nestedMismatch
	virtual := func(id string) *candidateHost {
		h := &candidateHost{ID: id, TenantID: pl.TenantID, PoolID: pl.ID, Pool: pl.Name, Shared: pl.Shared && pl.TenantID == nil, Retired: pl.Retired, Connected: true}
		if plan.Expected != nil {
			h.Capacity = plan.Expected.Capacity
			h.Labels = maps.Clone(plan.Expected.Labels)
		}
		if h.Labels == nil {
			h.Labels = map[string]string{}
		}
		delete(h.Labels, "nested")
		if nested {
			h.Labels["nested"] = "true"
		}
		return h
	}
	var future []*candidateHost
	if plan.Expected != nil {
		for _, id := range startingIDs {
			future = append(future, virtual(id))
		}
	}
	probe := false
	incapable := 0 // Unmet nested Runs no future host can serve.
	// reserve fits r on the first of hosts it fits; hosts are at stage, or
	// "planned" for a new host (no id).
	reserve := func(hosts []*candidateHost, r pendingRun, stage string) bool {
		for _, h := range hosts {
			hostStage := stage
			if h.ID == "" {
				hostStage = "planned"
			}
			if blockers := hostFit(r, h); len(blockers) != 0 {
				// Another host's chosen-host wait is not evidence about this
				// host, nor is a host outside the pool (a candidate only
				// because another Run chose it) evidence for a Run that did not.
				if h.ID != "" && (r.PlaceOn == "" && h.PoolID == pl.ID || r.PlaceOn == h.ID) {
					hostBlocked(r, h.ID, hostStage, blockers)
				}
				continue
			}
			reserveHost(h, r)
			switch hostStage {
			case "ready":
				plan.Ready++
				plan.reserved[h.ID] = true
			case "starting":
				plan.Starting++
			case "planned":
				plan.Planned++
			}
			if h.ID != "" {
				reservedStage[h.ID] = hostStage
			}
			return true
		}
		return false
	}
	for _, r := range runs {
		if reason, err := s.unmetPrerequisite(ctx, tx, r); err != nil {
			return plan, err
		} else if reason != "" {
			unsatisfied(r, reason)
			continue
		}
		if reserve(ready, r, "ready") {
			continue
		}
		// A chosen-host wait must not trigger launches on unrelated hosts.
		if r.PlaceOn != "" {
			unsatisfied(r, "waiting for its chosen host")
			continue
		}
		// No future host of this template can serve a nested Run: it neither
		// bootstraps nor probes, except that a mismatch keeps the bounded
		// probe so a fixed $Default launch template is seen.
		if r.Spec.Sandbox.NestedContainers && !nested {
			plan.Unmet++
			incapable++
			reason := "pool template does not offer nested containers"
			if nestedMismatch {
				reason = "current template's hosts registered without nested containers"
				if waitingSince[r.ID].After(lastRegistered) {
					probe = true
				}
			}
			deficit(r, "new_host", []fitBlocker{{Kind: kindNested, Reason: reason}})
			continue
		}
		if reserve(future, r, "starting") {
			continue
		}
		if plan.Expected == nil {
			plan.Unmet++
			deficit(r, "new_host", []fitBlocker{{Reason: "new host capacity unknown"}})
			continue
		}
		h := virtual("")
		if blockers := hostFit(r, h); len(blockers) != 0 {
			plan.Unmet++
			deficit(r, "new_host", blockers)
			// The expectation may be stale ($Default moved to a larger
			// instance type): a Run that has seen no current-template host
			// register since it began waiting may justify one probe.
			if waitingSince[r.ID].After(lastRegistered) {
				probe = true
			}
			continue
		}
		reserveHost(h, r)
		future = append(future, h)
		plan.NewHosts++
		plan.Planned++
	}
	for id, b := range blockedHosts {
		decision := "blocked"
		if _, reserved := reservedStage[id]; reserved {
			decision = "exhausted"
		}
		plan.hostDecisions[id] = hostDecision{Pool: pl.Name, Stage: b.stage, Decision: decision, Blockers: b.blockers}
	}
	for id, stage := range reservedStage {
		if _, blocked := blockedHosts[id]; !blocked {
			plan.hostDecisions[id] = hostDecision{Pool: pl.Name, Stage: stage, Decision: "reserved"}
		}
	}
	if err := s.idleDecisions(ctx, tx, pl, &plan); err != nil {
		return plan, err
	}
	// Unknown capacity bootstraps one host; a stale expectation probes with
	// one. Either waits for any current-template start to register first, and
	// a probe is bounded per waiting cohort: once it registers, the Runs are
	// older than it. Planned new hosts already serve as the probe.
	bootstrap := plan.Expected == nil && plan.Unmet > incapable
	probe = probe && plan.NewHosts == 0
	if (bootstrap || probe) && len(startingIDs) == 0 {
		plan.NewHosts = 1
		plan.Probe = probe
	}
	plan.awaitingStart = (bootstrap || probe) && len(startingIDs) > 0
	for _, stage := range reservedStage {
		if stage == "starting" {
			plan.reservedStarting++
		}
	}
	for id := range plan.reserved {
		if idle[id] {
			plan.reservedIdle++
		}
	}
	if bootstrap && len(startingIDs) > 0 {
		plan.reservedStarting = 1
	}
	return plan, nil
}

// waitingRuns is the pool's Runs waiting for capacity, oldest first, with
// when each began waiting.
func waitingRuns(ctx context.Context, tx pgx.Tx, pl poolRow) ([]pendingRun, map[string]time.Time, error) {
	// updated_at is when the Run entered provisioning: setRunState stamps it,
	// and while it waits only state_reason and place_on change, neither of
	// which touches updated_at.
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, state, spec, snapshot_id,
		jsonb_array_length(secrets) > 0, coalesce(place_on, ''), coalesce(avoid_host, ''), pool_id, updated_at
		FROM runs WHERE pool_id = $1 AND state = 'provisioning' AND NOT cancel_requested
		ORDER BY updated_at, id`, pl.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var runs []pendingRun
	waitingSince := map[string]time.Time{}
	for rows.Next() {
		var r pendingRun
		var since time.Time
		if err := rows.Scan(&r.ID, &r.TenantID, &r.State, &r.Spec, &r.SnapshotID, &r.HasSecrets, &r.PlaceOn, &r.AvoidHost, &r.PoolID, &since); err != nil {
			return nil, nil, err
		}
		runs = append(runs, r)
		waitingSince[r.ID] = since
	}
	return runs, waitingSince, rows.Err()
}

// readyHosts is the hosts the scheduler would consider for runs, by id.
func (s *Server) readyHosts(ctx context.Context, tx pgx.Tx, runs []pendingRun) ([]*candidateHost, error) {
	// Every Run is in this pool: one (tenant, chosen host) tuple each suffices.
	var pools []*string
	var tenants, chosen []string
	seen := map[[2]string]bool{}
	for _, r := range runs {
		if key := [2]string{r.TenantID, r.PlaceOn}; !seen[key] {
			seen[key] = true
			pools = append(pools, r.PoolID)
			tenants = append(tenants, r.TenantID)
			chosen = append(chosen, r.PlaceOn)
		}
	}
	ids, err := s.eligibleHostIDs(ctx, tx, pools, tenants, chosen)
	if err != nil {
		return nil, err
	}
	ready, err := s.candidateHosts(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	// The hub only knows runners connected to this luxd; a fresh heartbeat
	// (required by candidateHosts) is the cluster-wide liveness signal.
	for _, h := range ready {
		h.Connected = true
	}
	slices.SortFunc(ready, func(a, b *candidateHost) int { return strings.Compare(a.ID, b.ID) })
	return ready, nil
}

// unmetPrerequisite is why r cannot be placed on any host yet (its secrets
// or its snapshot), or "".
func (s *Server) unmetPrerequisite(ctx context.Context, tx pgx.Tx, r pendingRun) (string, error) {
	if _, ok := s.secrets.get(r.ID); r.HasSecrets && !ok {
		return "run secrets unavailable", nil
	}
	if r.SnapshotID == nil {
		return "", nil
	}
	var available, uploaded bool
	err := tx.QueryRow(ctx, `SELECT available, uploaded FROM snapshots WHERE id = $1`, *r.SnapshotID).Scan(&available, &uploaded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "snapshot missing", nil
	case err != nil:
		return "", err
	case !available:
		return "snapshot unavailable", nil
	case !uploaded:
		return "snapshot upload pending", nil
	}
	if _, err := restoreManifest(ctx, tx, r.ID, *r.SnapshotID); err != nil {
		var foreign *foreignBlobError
		if errors.As(err, &foreign) {
			return "snapshot manifest references unavailable blobs", nil
		}
		return "", err
	}
	return "", nil
}
