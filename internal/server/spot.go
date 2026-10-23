package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// spotRunSQL, for SQL over runs (as r): the Run is a spot Run.
const spotRunSQL = `coalesce(r.spec->>'spot', '') = 'true'`

// reclaimWaitReason is a normal Run's state_reason while spot placements
// it reclaimed release their resources.
const reclaimWaitReason = "waiting for reclaimed spot capacity to be released"

// reclaimable reports whether a normal Run waiting for wait may reclaim
// spot placements: a capacity wait, not a snapshot or chosen-host one.
func reclaimable(wait string) bool {
	return strings.HasPrefix(wait, "waiting for capacity") || wait == "no host matches"
}

func hasPledge(hosts []*candidateHost, runID string) bool {
	return slices.ContainsFunc(hosts, func(h *candidateHost) bool { _, ok := h.Pledges[runID]; return ok })
}

// releasePledge ends runID's pledge now that it is placed (on h, or
// elsewhere: the victims stay signalled, a reclamation is not undone).
func (s *Server) releasePledge(ctx context.Context, tx pgx.Tx, runID string, h *candidateHost) error {
	h.dropPledge(runID)
	_, err := tx.Exec(ctx, `DELETE FROM spot_pledges WHERE run_id = $1`, runID)
	return err
}

// spotWaitReason is a waiting spot Run's state_reason, from its admission
// blockers: tenant-readable, never naming hosts.
func spotWaitReason(blockers []AdmissionBlocker) string {
	var parts []string
	for _, b := range blockers {
		switch b.Code {
		case blockNoHosts:
			parts = append(parts, "no live host in its pool")
		case blockConstraint:
			parts = append(parts, fmt.Sprintf("%d %s its constraints", b.Hosts, plural(b.Hosts, "host fails", "hosts fail")))
		case blockCapability:
			parts = append(parts, fmt.Sprintf("%d %s spot support", b.Hosts, plural(b.Hosts, "host lacks", "hosts lack")))
		case blockNotEnough:
			parts = append(parts, fmt.Sprintf("%d %s spare capacity", b.Hosts, plural(b.Hosts, "host lacks", "hosts lack")))
		case blockNotSettled:
			parts = append(parts, fmt.Sprintf("%d %s spare capacity free for under 60s", b.Hosts, plural(b.Hosts, "host has", "hosts have")))
		case blockNormalPending:
			parts = append(parts, "normal Runs are waiting for that capacity")
		case blockDiskPressure:
			parts = append(parts, fmt.Sprintf("%d %s under disk pressure (low free disk)", b.Hosts, plural(b.Hosts, "host is", "hosts are")))
		}
	}
	return truncate("waiting for spot capacity: "+strings.Join(parts, ", "), maxWaitReason)
}

// spotPlacement is a live placement of a spot Run, a reclamation candidate.
type spotPlacement struct {
	ID, RunID, TenantID string
	// ReclaimFor: the normal Run it is being reclaimed for, "" if none.
	ReclaimFor string
	Res        spec.Resources
}

// Admission blocker codes, in the order a result lists them.
const (
	blockNoHosts       = "no_hosts"
	blockConstraint    = "constraint"
	blockCapability    = "capability"
	blockNotEnough     = "not_enough_spare"
	blockNotSettled    = "space_not_settled"
	blockNormalPending = "normal_demand_pending"
	blockDiskPressure  = "disk_pressure"
)

// admission is where a Run would start now, or why on no host.
type admission struct {
	Host     *candidateHost
	Blockers []AdmissionBlocker
}

// blockerSet aggregates per-host blockers by code.
type blockerSet map[string]*AdmissionBlocker

func (b blockerSet) add(code, reason string, remaining *time.Duration) {
	e := b[code]
	if e == nil {
		e = &AdmissionBlocker{Code: code, Reason: reason}
		b[code] = e
	}
	e.Hosts++
	if remaining != nil {
		sec := int((*remaining + time.Second - 1) / time.Second)
		if e.RemainingSeconds == nil || sec < *e.RemainingSeconds {
			e.RemainingSeconds = &sec
		}
	}
}

func (b blockerSet) list() []AdmissionBlocker {
	var out []AdmissionBlocker
	for _, code := range []string{blockNoHosts, blockConstraint, blockCapability, blockDiskPressure, blockNotEnough, blockNotSettled, blockNormalPending} {
		if e := b[code]; e != nil {
			out = append(out, *e)
		}
	}
	return out
}

// classify adds h's hostFit blockers to set; ok when there are none.
func (b blockerSet) classify(blockers []fitBlocker) bool {
	if len(blockers) == 0 {
		return true
	}
	for _, f := range blockers {
		switch f.Kind {
		case kindSpotReclaim, kindSyncModes, kindNonPreemptible, kindManagedImages:
			b.add(blockCapability, f.Reason, nil)
			return false
		case kindDiskPressure:
			b.add(blockDiskPressure, "", nil)
			return false
		case kindResource:
		default:
			b.add(blockConstraint, f.Reason, nil)
			return false
		}
	}
	b.add(blockNotEnough, "", nil)
	return false
}

// byHostID is a copy of hosts in id order, so admission and reclaim plans
// try hosts deterministically.
func byHostID(hosts []*candidateHost) []*candidateHost {
	ordered := slices.Clone(hosts)
	slices.SortFunc(ordered, func(a, b *candidateHost) int { return strings.Compare(a.ID, b.ID) })
	return ordered
}

// spotAdmission decides where spot Run r may start now. It is the single
// admission rule for the scheduler and for submit's validateOnly: a host
// that hostFit accepts (constraints, runner capability, current spare),
// whose spare vector covering r has been continuously free for more than
// spotSettle, and that no queued normal Run in demand could use. hosts is
// not modified.
func spotAdmission(r pendingRun, hosts []*candidateHost, demand []pendingRun, now time.Time) admission {
	set := blockerSet{}
	ordered := byHostID(hosts)
	res := r.Spec.Resources
	for _, h := range ordered {
		if !set.classify(hostFit(r, h)) {
			continue
		}
		if h.age == nil || !h.age.aged(now).covers(res.CPUs, int64(res.Memory), int64(res.Disk)) {
			var wait *time.Duration
			if h.age != nil {
				if d, ok := h.age.settlesIn(now, res.CPUs, int64(res.Memory), int64(res.Disk)); ok {
					wait = &d
				}
			}
			set.add(blockNotSettled, "", wait)
			continue
		}
		if slices.ContainsFunc(demand, func(n pendingRun) bool { return normalWants(n, h) }) {
			set.add(blockNormalPending, "", nil)
			continue
		}
		return admission{Host: h}
	}
	if len(ordered) == 0 {
		set.add(blockNoHosts, "", nil)
	}
	return admission{Blockers: set.list()}
}

// normalAdmission is where normal Run r fits now (hostFit only), the
// validateOnly answer for a normal Run: it does not account for snapshot
// locality, reclaimable spot placements or hosts a pool could launch.
func normalAdmission(r pendingRun, hosts []*candidateHost) admission {
	set := blockerSet{}
	ordered := byHostID(hosts)
	for _, h := range ordered {
		if set.classify(hostFit(r, h.forRun(r.ID))) {
			return admission{Host: h}
		}
	}
	if len(ordered) == 0 {
		set.add(blockNoHosts, "", nil)
	}
	return admission{Blockers: set.list()}
}

// normalWants reports whether queued normal Run n could be placed on h,
// directly or by reclaiming h's spot placements. A Run holding a pledge is
// already served by it.
func normalWants(n pendingRun, h *candidateHost) bool {
	return len(hostFit(n, h.withoutSpots())) == 0
}

// withoutSpots is h with every spot placement released, simulated once
// and kept until h changes (changed): spot admission asks it for every
// queued normal Run.
func (h *candidateHost) withoutSpots() *candidateHost {
	if h.noSpots == nil {
		sim := newHostSim(h)
		for _, sp := range h.Spots {
			sim.remove(sp)
		}
		h.noSpots = sim.host()
	}
	return h.noSpots
}

// changed drops what was simulated from h's previous usage.
func (h *candidateHost) changed() { h.noSpots = nil }

func (h *candidateHost) clone() *candidateHost {
	c := *h
	c.Tenants = slices.Clone(h.Tenants)
	c.noSpots = nil
	return &c
}

// usageOf is what a placement reserving res holds on its host.
func usageOf(res spec.Resources) freeVec {
	return freeVec{res.CPUs, int64(res.Memory), int64(res.Disk), 1}
}

// reserve counts v as used on h.
func (h *candidateHost) reserve(v freeVec) {
	h.changed()
	h.UsedCPUs += v.CPUs
	h.UsedMem += v.Memory
	h.UsedDisk += v.Disk
	h.UsedRuns += v.Runs
}

// release stops counting v as used on h.
func (h *candidateHost) release(v freeVec) {
	h.changed()
	h.UsedCPUs -= v.CPUs
	h.UsedMem -= v.Memory
	h.UsedDisk -= v.Disk
	h.UsedRuns -= v.Runs
}

// remove simulates sp's resources and tenancy released from h.
func (h *candidateHost) remove(sp spotPlacement) {
	h.release(usageOf(sp.Res))
	if i := slices.Index(h.Tenants, sp.TenantID); i >= 0 {
		h.Tenants = slices.Delete(h.Tenants, i, i+1)
	}
}

// hostSim simulates spot placements released from and restored to a host,
// each in constant time. Tenancy is a count per tenant: a tenant is
// present while its count is positive, which is what removing one entry
// of the tenant list per victim (when it has one left) leaves present.
type hostSim struct {
	h       candidateHost
	tenants map[string]int
	present int // tenants with a positive count
	// view and its tenant list are reused by fits: hostFit keeps neither.
	view    candidateHost
	viewTen [2]string
}

func newHostSim(h *candidateHost) *hostSim {
	s := &hostSim{h: *h, tenants: map[string]int{}}
	s.h.noSpots = nil
	for _, t := range h.Tenants {
		if s.tenants[t]++; s.tenants[t] == 1 {
			s.present++
		}
	}
	return s
}

func (s *hostSim) remove(sp spotPlacement) {
	s.h.release(usageOf(sp.Res))
	if s.tenants[sp.TenantID]--; s.tenants[sp.TenantID] == 0 {
		s.present--
	}
}

func (s *hostSim) restore(sp spotPlacement) {
	s.h.reserve(usageOf(sp.Res))
	if s.tenants[sp.TenantID]++; s.tenants[sp.TenantID] == 1 {
		s.present++
	}
}

// host is the simulated host for hostFit: its tenants each listed once
// (hostFit asks only whether another tenant is present), sorted.
func (s *hostSim) host() *candidateHost {
	c := s.h
	c.Tenants = nil
	for t, n := range s.tenants {
		if n > 0 {
			c.Tenants = append(c.Tenants, t)
		}
	}
	slices.Sort(c.Tenants)
	return &c
}

// hostFor is the simulated host as hostFit sees it for tenant's Run, in
// constant time: tenant if present, then, when any other tenant is, one
// stand-in for them all (hostFit only compares tenants with tenant).
func (s *hostSim) hostFor(tenant string) *candidateHost {
	s.view = s.h
	ten := s.viewTen[:0]
	others := s.present
	if s.tenants[tenant] > 0 {
		ten = append(ten, tenant)
		others--
	}
	if others > 0 {
		ten = append(ten, tenant+"\x00")
	}
	s.view.Tenants = ten
	return &s.view
}

func (s *hostSim) fits(r pendingRun) bool { return len(hostFit(r, s.hostFor(r.TenantID))) == 0 }

// forRun is h as runID sees it: its own pledge's reservation not counted.
func (h *candidateHost) forRun(runID string) *candidateHost {
	extra, ok := h.Pledges[runID]
	if !ok {
		return h
	}
	c := h.clone()
	c.release(extra)
	return c
}

// pledgeExtra is what a pledge of res reserves beyond its victims' live
// usage on h: per component, what the victims do not hold themselves.
func pledgeExtra(h *candidateHost, runID string, res spec.Resources) freeVec {
	var live freeVec
	for _, sp := range h.Spots {
		if sp.ReclaimFor == runID {
			live.CPUs += sp.Res.CPUs
			live.Memory += int64(sp.Res.Memory)
			live.Disk += int64(sp.Res.Disk)
			live.Runs++
		}
	}
	return freeVec{max(0, res.CPUs-live.CPUs), max(0, int64(res.Memory)-live.Memory), max(0, int64(res.Disk)-live.Disk), max(0, 1-live.Runs)}
}

func (h *candidateHost) addPledge(runID string, extra freeVec) {
	if h.Pledges == nil {
		h.Pledges = map[string]freeVec{}
	}
	h.Pledges[runID] = extra
	h.reserve(extra)
}

func (h *candidateHost) dropPledge(runID string) {
	extra, ok := h.Pledges[runID]
	if !ok {
		return
	}
	delete(h.Pledges, runID)
	h.release(extra)
}

// loadPledges counts each pledge's extra reservation (pledgeExtra) as used
// on its host. Pledges are written and deleted only under their host's lock.
func loadPledges(ctx context.Context, tx pgx.Tx, hostIDs []string, byID map[string]*candidateHost) error {
	rows, err := tx.Query(ctx, `SELECT run_id, host_id, tenant_id, resources FROM spot_pledges WHERE host_id = ANY($1) ORDER BY run_id`, hostIDs)
	if err != nil {
		return err
	}
	type pledge struct {
		run, host, tenant string
		res               spec.Resources
	}
	var all []pledge
	for rows.Next() {
		var p pledge
		if err := rows.Scan(&p.run, &p.host, &p.tenant, &p.res); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range all {
		if h := byID[p.host]; h != nil {
			h.addPledge(p.run, pledgeExtra(h, p.run, p.res))
			h.Tenants = append(h.Tenants, p.tenant)
		}
	}
	return nil
}

// ageKey identifies what a host's capacity history holds for: a runner
// hello (capacity_epoch), a capacity change, a pool move or a change of its
// pool's shape each start it over. It names no luxd: every luxd appends to
// the same history (a placement trigger records every transition, whoever
// makes it), so another live luxd's writes keep it, and a restart resumes it.
func ageKey(h *candidateHost) string {
	c := h.Capacity
	return fmt.Sprintf("%d/%g/%d/%d/%d/%s/%s", h.CapEpoch, c.CPUs, c.Memory, c.Disk, c.Runs, h.PoolID, h.Shape)
}

// loadCapacityAge reads each host's history (one primary-key read per
// host) and records its current free vector at now.
func (s *Server) loadCapacityAge(ctx context.Context, tx pgx.Tx, hostIDs []string, byID map[string]*candidateHost) error {
	rows, err := tx.Query(ctx, `SELECT host_id, key, history FROM host_capacity_age WHERE host_id = ANY($1)`, hostIDs)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		c := &capacityAge{}
		if err := rows.Scan(&id, &c.Key, &c.History); err != nil {
			rows.Close()
			return err
		}
		if h := byID[id]; h != nil {
			h.age = c
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.now()
	for _, h := range byID {
		if h.age == nil {
			h.age = &capacityAge{}
		}
		h.age.observe(ageKey(h), now, freeOf(h))
	}
	return nil
}

// saveCapacityAge writes the histories this transaction changed. Callers
// hold the hosts' locks.
func saveCapacityAge(ctx context.Context, tx pgx.Tx, hosts []*candidateHost) error {
	for _, h := range hosts {
		if h.age == nil || !h.age.dirty {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_capacity_age (host_id, key, history) VALUES ($1, $2, $3)
			ON CONFLICT (host_id) DO UPDATE SET key = excluded.key, history = excluded.history`, h.ID, h.age.Key, h.age.marshal()); err != nil {
			return err
		}
		h.age.dirty = false
	}
	return nil
}

// normalDemandLimit bounds the queued normal Runs read for spot admission;
// a longer queue refuses every spot Run in the batch.
const normalDemandLimit = 500

// normalDemand is every queued normal Run that could use one of hosts
// (their pools, or chosen among them), not holding a pledge. ok is false
// when the queue exceeded normalDemandLimit.
func normalDemand(ctx context.Context, tx pgx.Tx, hosts []*candidateHost) ([]pendingRun, bool, error) {
	var pools, ids []string
	for _, h := range hosts {
		pools = append(pools, h.PoolID)
		ids = append(ids, h.ID)
	}
	slices.Sort(pools)
	pools = slices.Compact(pools)
	rows, err := tx.Query(ctx, `SELECT r.id, r.tenant_id, r.state, r.spec, r.pending_sync, coalesce(r.place_on, ''), r.pool_id, p.shape
		FROM runs r LEFT JOIN pools p ON p.id = r.pool_id
		WHERE r.state IN `+queuedRunStates+` AND NOT r.terminate_requested
		  AND coalesce(r.spec->>'spot', '') <> 'true'
		  AND (r.pool_id = ANY($1) OR r.place_on = ANY($2))
		  AND NOT EXISTS (SELECT 1 FROM spot_pledges sp WHERE sp.run_id = r.id AND sp.host_id = ANY($2))
		ORDER BY r.updated_at, r.id LIMIT $3`, pools, ids, normalDemandLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []pendingRun
	for rows.Next() {
		var r pendingRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.State, &r.Spec, &r.PendingSync, &r.PlaceOn, &r.PoolID, &r.Shape); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if len(out) > normalDemandLimit {
		return nil, false, rows.Err()
	}
	return out, true, rows.Err()
}

// reclaimPlan picks, on one host, a deterministic small set of unclaimed
// spot placements whose release makes normal Run r fit (hostFit on the host
// with them removed, tenancy included). Hosts are tried in id order, or
// for a pack Run the fullest by normal work first, ties by id, as the
// planner reserves. Pack's occupancy tie-break is left out: every host here
// needs a reclaim, and the most occupied one has the most spot work to
// kill. On each host the youngest placements are removed until r
// fits, then any victim r fits without is put back. nil when no host's spot
// placements suffice. One simulation per host: each step releases or
// restores one victim.
func reclaimPlan(r pendingRun, hosts []*candidateHost) (*candidateHost, []spotPlacement) {
	ordered := byHostID(hosts)
	if r.Pack {
		rank := func(h *candidateHost) packCandidate {
			c := packCandidateOf(h, r.AvoidHost, 0)
			c.occupied = c.used
			return c
		}
		slices.SortFunc(ordered, func(a, b *candidateHost) int { return packOrder(rank(a), rank(b)) })
	}
	for _, h := range ordered {
		var free []spotPlacement
		for _, sp := range h.Spots {
			if sp.ReclaimFor == "" {
				free = append(free, sp)
			}
		}
		// Spots are loaded oldest first: the youngest lose the least work.
		slices.Reverse(free)
		sim := newHostSim(h)
		n, fit := 0, false
		for n < len(free) && !fit {
			sim.remove(free[n])
			n++
			fit = sim.fits(r)
		}
		if !fit {
			continue
		}
		chosen := slices.Clone(free[:n])
		// sim is h without chosen: put each back while r still fits.
		for i := 0; i < len(chosen); {
			if len(chosen) > 1 {
				sim.restore(chosen[i])
				if sim.fits(r) {
					chosen = slices.Delete(chosen, i, i+1)
					continue
				}
				sim.remove(chosen[i])
			}
			i++
		}
		return h, chosen
	}
	return nil, nil
}

// pledge claims victims on h for normal Run r and reserves r's resources
// there, in the scheduler's transaction under h's lock. A victim whose row
// another transaction holds is not waited for: the plan is abandoned
// (false) and retried on a later pass.
func pledge(ctx context.Context, tx pgx.Tx, r pendingRun, h *candidateHost, victims []spotPlacement) (bool, error) {
	ids := make([]string, len(victims))
	for i, v := range victims {
		ids[i] = v.ID
	}
	rows, err := tx.Query(ctx, `SELECT id FROM placements WHERE id = ANY($1) AND reclaim_for IS NULL AND state IN `+livePlacementStates+`
		ORDER BY id FOR UPDATE SKIP LOCKED`, ids)
	if err != nil {
		return false, err
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(locked) != len(ids) {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE placements SET reclaim_for = $2 WHERE id = ANY($1)`, ids, r.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO spot_pledges (run_id, host_id, host_epoch, tenant_id, resources) VALUES ($1, $2, $3, $4, $5)`,
		r.ID, h.ID, h.CapEpoch, r.TenantID, r.Spec.Resources); err != nil {
		return false, err
	}
	for i := range h.Spots {
		if slices.Contains(ids, h.Spots[i].ID) {
			h.Spots[i].ReclaimFor = r.ID
		}
	}
	h.addPledge(r.ID, pledgeExtra(h, r.ID, r.Spec.Resources))
	h.Tenants = append(h.Tenants, r.TenantID)
	return true, nil
}

// dropPledges deletes the pledges of Runs no longer waiting for a host (on
// hosts this transaction holds), and every pledge whose host is no longer
// live or whose runner said hello since (a restart: its spot placements may
// be gone). No live scheduler can place on such a host, so its pledges are
// nobody's to read.
func (s *Server) dropPledges(ctx context.Context, tx pgx.Tx, hostIDs []string) error {
	_, err := tx.Exec(ctx, `DELETE FROM spot_pledges sp WHERE
		(sp.host_id = ANY($1) AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = sp.run_id AND r.state IN `+queuedRunStates+` AND NOT r.terminate_requested))
		OR NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id = sp.host_id AND h.state = 'ready' AND NOT h.draining
			AND h.capacity_epoch = sp.host_epoch AND h.last_heartbeat > now() - $2::interval)`, hostIDs, interval(s.cfg.LeaseDuration))
	return err
}

// cleanupPledges runs even when the queue is empty: cancelled requesters
// can leave no batch that would discover their host. It holds sorted host
// locks in a separate transaction and never takes a Run lock under them.
func (s *Server) cleanupPledges(ctx context.Context) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT sp.host_id FROM spot_pledges sp
			WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id=sp.run_id AND r.state IN `+queuedRunStates+` AND NOT r.terminate_requested)
			OR NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id=sp.host_id AND h.state='ready' AND NOT h.draining
				AND h.capacity_epoch=sp.host_epoch AND h.last_heartbeat > now()-$1::interval)
			ORDER BY sp.host_id`, interval(s.cfg.LeaseDuration))
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := lockCostHost(ctx, tx, id); err != nil {
				return err
			}
		}
		return s.dropPledges(ctx, tx, ids)
	})
}

// signalReclaims sends the stop for every claimed spot placement not yet
// told: the Run's row first, then its host's (requestStop), never the other
// way round. The claim is durable, so a crash between the scheduler's
// commit and this pass is caught up by the next.
func (s *Server) signalReclaims(ctx context.Context) error {
	type claim struct{ run, tenant, host, forRun string }
	var claims []claim
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT run_id, tenant_id, host_id, reclaim_for FROM placements
			WHERE reclaim_for IS NOT NULL AND stop_reason <> $1 AND state IN `+livePlacementStates+` ORDER BY run_id`, stopSpotReclaim)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c claim
			if err := rows.Scan(&c.run, &c.tenant, &c.host, &c.forRun); err != nil {
				rows.Close()
				return err
			}
			claims = append(claims, c)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, c := range claims {
		var host string
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, c.run); err != nil {
				return err
			}
			var err error
			if host, err = s.requestStop(ctx, tx, c.tenant, c.run, stopSpotReclaim); err != nil || host == "" {
				return err
			}
			if err := addEvent(ctx, tx, c.tenant, c.run, 0, "spot.reclaimed", map[string]any{"for": c.forRun, "host": host}); err != nil {
				return err
			}
			return hostEvent(ctx, tx, host, evSpotReclaimed, map[string]any{"run": c.run, "for": c.forRun})
		})
		if err != nil {
			return err
		}
		if host != "" {
			s.hub.Notify(host)
		}
	}
	return nil
}
