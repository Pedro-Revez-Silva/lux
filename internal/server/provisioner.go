package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Provider provisions hosts for pools whose provider it is (ec2).
type Provider interface {
	// Launch starts one host, tagged with tags, and returns its provider
	// id and what the provider says it is. env is what its runner starts
	// with (URL, host token, name).
	Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (Launched, error)
	// Terminate ends a host. One that no longer exists is done.
	Terminate(ctx context.Context, template json.RawMessage, providerID string) error
	// Instances lists the provider's hosts carrying all the given tags,
	// by provider id.
	Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error)
}

// Launched is a host as the provider started it. InstanceType is the
// provider's answer, not the template's (a launch template may choose it);
// empty fields are unknown and stored as NULL.
type Launched struct {
	ProviderID   string
	InstanceType string
	Zone         string
	Market       string // MarketOnDemand or MarketSpot
}

const (
	MarketOnDemand = "on-demand"
	MarketSpot     = "spot"
)

// Instance is a provider's view of one host.
type Instance struct {
	// State: terminated and shutting-down are gone; anything else may
	// still run.
	State string
	// Tags it carries (lux:host names its host row).
	Tags map[string]string
}

// Tags lux puts on what it launches: how the provisioner finds a pool's
// instances whatever its database knows (a launch whose reply was lost,
// a row written off too early).
const (
	tagManaged    = "lux:managed"
	tagDeployment = "lux:deployment" // which lux database launched it
	tagPool       = "lux:pool"       // its name at launch; informational, not updated on rename
	tagPoolID     = "lux:pool-id"    // what a pool's instances are listed by
	tagHost       = "lux:host"       // the host row's id
)

// provisionerLoop keeps provisioned pools the size their demand, minimum
// and warm settings say. One luxd at a time does it (provisionLease).
func (s *Server) provisionerLoop(ctx context.Context) {
	if len(s.cfg.Providers) == 0 {
		return
	}
	for s.deployment == "" {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&s.deployment)
		})
		if err != nil {
			s.log.Warn("provisioner: reading the deployment id", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
	// A luxd shutting down (a deploy) lets another take over at once
	// rather than after the lease's expiry.
	defer s.releaseProvisionLease()
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.provision(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("provisioner", "err", err)
		}
	}
}

type poolRow struct {
	ID, Name, Provider string
	TenantID           *string
	Template           json.RawMessage
	Min, Max, Warm     int
	Retired            bool
	// ScaleDownAfterS: the pool's own idle seconds, or nil for luxd's.
	ScaleDownAfterS *int
	WarmWhileActive bool
}

func (s *Server) provision(ctx context.Context) error {
	// Leadership: a lease in the database, not a lock held on a connection
	// across provider calls. One luxd reconciles at a time; another takes
	// over when the lease lapses.
	if ok, err := s.provisionLease(ctx); err != nil || !ok {
		return err
	}
	var pools []poolRow
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, provider, tenant_id, template, min_hosts, max_hosts, warm_hosts, retired,
				scale_down_after_s, warm_while_active
			FROM pools WHERE provider <> 'static'
			  AND (NOT retired OR EXISTS (SELECT 1 FROM hosts h WHERE h.pool_id = pools.id AND h.state <> 'terminated'))`)
		if err != nil {
			return err
		}
		pools, err = pgx.CollectRows(rows, pgx.RowToStructByPos[poolRow])
		return err
	})
	if err != nil {
		return err
	}
	checkAlive := time.Since(s.lastAliveCheck) > s.cfg.ProviderCheckEvery
	if checkAlive {
		s.lastAliveCheck = time.Now()
	}
	for _, pl := range pools {
		prov := s.cfg.Providers[pl.Provider]
		if prov == nil {
			continue
		}
		// Provider calls can be slow: still the provisioner?
		if ok, err := s.provisionLease(ctx); err != nil || !ok {
			return err
		}
		if err := s.reconcilePool(ctx, prov, pl, checkAlive); err != nil {
			s.log.Warn("pool", "pool", pl.Name, "err", err)
		}
	}
	return nil
}

// poolState is what a pool has and needs, counted in one transaction.
type poolState struct {
	demand, idle, provisioning, total int
	plan                              capacityPlan
	// active: a placement started or ended on the pool's hosts within its
	// scale-down time (warm_while_active keeps warm hosts only then).
	active bool
	// Idle hosts that have been idle longer than the cooldown, oldest first.
	idleExpired []string
	idleHosts   map[string]bool
	// Hosts to terminate now: drained ones that are done (no live
	// placements, nothing to upload), ones that never registered, and lost
	// ones (their runner stopped answering; the instance may still run).
	terminate []hostRef
	// Hosts the provider should still have (checked every ProviderCheckEvery).
	existing []hostRef
	// Hosts launched whose instance id is not recorded yet, by id.
	launching map[string]bool
	// Hosts whose launch never completed: written off.
	abandoned []string
}

// hostRef is a provisioned host, with the template it was launched with
// (its region), whatever its pool's template says now.
type hostRef struct {
	ID, ProviderID, Reason string
	Template               json.RawMessage
	Draining               bool // not counted in the pool's total
	// Settled: launched with the tags we list by, long enough ago that the
	// provider lists it (its listings are eventually consistent), and its
	// runner is not heartbeating; one missing from the listings is gone.
	Settled bool
}

func (s *Server) reconcilePool(ctx context.Context, prov Provider, pl poolRow, checkAlive bool) error {
	var st poolState
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.poolState(ctx, tx, pl, &st)
	})
	if err != nil {
		return err
	}
	for _, id := range st.abandoned {
		s.markTerminated(ctx, id, "launch never completed")
	}

	// Every ProviderCheckEvery (provider API limits), the provider's view of the
	// pool, by tag: hosts it terminated behind our back go (their Runs are
	// lost by the usual heartbeat path); instances of this pool that no
	// live row claims (a launch whose reply was lost) are terminated.
	if checkAlive {
		s.reconcileWithProvider(ctx, prov, pl, &st)
	}

	// Provider reconciliation changes both live reservations and valid starts.
	st = poolState{}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := s.poolState(ctx, tx, pl, &st); err != nil {
			return err
		}
		var err error
		st.plan, err = s.planCapacity(ctx, tx, pl)
		for id := range st.plan.reserved {
			if st.idleHosts[id] {
				st.plan.reservedIdle++
			}
		}
		return err
	}); err != nil {
		return err
	}
	s.recordHostDecisions(ctx, pl, st.plan.hostDecisions)

	// Ready hosts reserved by the simulation are not idle surplus.
	for _, id := range st.idleExpired {
		if st.plan.reserved[id] {
			continue
		}
		if st.idle-st.plan.reservedIdle <= s.warm(pl, &st) || st.total <= pl.Min {
			break
		}
		drained, err := s.drainForScaleDown(ctx, id)
		if err != nil {
			return err
		}
		if drained {
			st.total--
			st.idle--
		}
	}
	for _, h := range st.terminate {
		s.terminateRequested(ctx, h.ID, h.Reason)
		if err := prov.Terminate(ctx, h.Template, h.ProviderID); err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			s.providerError(ctx, hostEvents, h.ID, "terminate", h.ProviderID, err)
			continue
		}
		s.markTerminated(ctx, h.ID, h.Reason)
	}

	// Scale up: enough for what waits plus the warm hosts, at least the
	// minimum, never past the maximum. A retired pool only winds down.
	if pl.Retired {
		return nil
	}
	warm := s.warm(pl, &st)
	warmStarting := max(0, st.provisioning-st.plan.reservedStarting)
	warmDeficit := max(0, warm-(st.idle-st.plan.reservedIdle)-warmStarting)
	want := max(pl.Min-st.total, st.plan.NewHosts+warmDeficit)
	if pl.Max > 0 {
		want = min(want, pl.Max-st.total)
	}
	if want <= 0 && st.plan.Unmet > 0 {
		s.scaleBlocked(ctx, pl, &st)
	}
	up := scaleUp(pl, &st, warm, want)
	for i := range max(want, 0) {
		if i > 0 {
			if ok, err := s.provisionLease(ctx); err != nil || !ok {
				return err
			}
			up = nil // recorded with the first launch
		}
		if err := s.launch(ctx, prov, pl, up); errors.Is(err, errPoolRetired) {
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}

// scaleUp is a pool.scale_up event's data: how many hosts and why, with
// the counts the decision was made from.
func scaleUp(pl poolRow, st *poolState, warm, want int) map[string]any {
	reason := "waiting runs"
	switch {
	case pl.Min-st.total >= want:
		reason = "minimum"
	case st.demand == 0:
		reason = "warm"
	}
	d := map[string]any{"hosts": want, "reason": reason, "waiting": st.demand, "warm": warm, "min": pl.Min, "max": pl.Max,
		"total": st.total, "idle": st.idle, "provisioning": st.provisioning}
	maps.Copy(d, st.plan.summary())
	if st.plan.Probe {
		d["probe"] = true
	}
	return d
}

// summary is the plan's bounded evidence, shared by pool.scale_up and
// pool.scale_blocked.
func (p *capacityPlan) summary() map[string]any {
	return map[string]any{"ready": p.Ready, "future": p.Future, "planned": p.Planned, "unmet": p.Unmet,
		"blocked": p.Blocked, "unknown": p.Unknown, "expected": p.Expected,
		"deficits": p.Deficits, "exhausted": p.Exhausted, "ineligible": p.Ineligible, "omitted": p.Omitted}
}

// scaleBlocked records why Runs the plan could not cover launch nothing (no
// new host fits them, or max or quota stops it). A pool that stays stuck
// folds each pass into one row.
func (s *Server) scaleBlocked(ctx context.Context, pl poolRow, st *poolState) {
	d := st.plan.summary()
	d["waiting"], d["total"], d["max"] = st.demand, st.total, pl.Max
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return poolRepeatEvent(ctx, tx, pl.ID, evScaleBlocked, d)
	}); err != nil {
		s.log.Warn("recording a blocked scale-up", "pool", pl.Name, "err", err)
	}
}

// evCapacityDecision records the planner's verdict on one actual host.
const evCapacityDecision = "host.capacity_decision"

// maxHostDecisions bounds host-decision transactions per pool pass.
const maxHostDecisions = 32

// recordHostDecisions appends a host's capacity decision only when it differs
// from that host's latest one. Each host uses its own transaction after the
// planning transaction commits, so no row lock is held. The exclusive stream
// lock precedes the read, which keeps concurrent writers from both appending
// the same change.
func (s *Server) recordHostDecisions(ctx context.Context, pl poolRow, decisions map[string]map[string]any) {
	hosts := slices.Sorted(maps.Keys(decisions))
	if len(hosts) > maxHostDecisions {
		hosts = hosts[:maxHostDecisions]
	}
	for _, id := range hosts {
		data := maps.Clone(decisions[id])
		data["pool"] = pl.Name
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return hostDecisionEvent(ctx, tx, id, data)
		}); err != nil {
			s.log.Warn("recording a capacity decision", "host", id, "err", err)
		}
	}
}

func hostDecisionEvent(ctx context.Context, tx pgx.Tx, hostID string, data map[string]any) error {
	if err := lockStream(ctx, tx, hostEvents, hostID, true); err != nil {
		return err
	}
	// Bounded to the host's latest events: a decision older than that is
	// re-recorded once, which is still accurate.
	var same bool
	err := tx.QueryRow(ctx, `SELECT data = $3::jsonb FROM (SELECT id, type, data FROM host_events
			WHERE host_id = $1 ORDER BY host_id DESC, id DESC LIMIT $4) latest
		WHERE type = $2 ORDER BY id DESC LIMIT 1`, hostID, evCapacityDecision, data, 4*foldWindow).Scan(&same)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if same {
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO host_events (tenant_id, host_id, type, data)
		SELECT tenant_id, id, $2, $3 FROM hosts WHERE id = $1`, hostID, evCapacityDecision, data)
	return err
}

// providerError records a failed provider call, in a transaction of its own
// (the call is outside any), on the host or the pool it was for. Repeats
// fold into one event.
func (s *Server) providerError(ctx context.Context, t eventTable, owner, op, providerID string, cause error) {
	data := map[string]any{"op": op, "error": providerErrorText(cause)}
	if providerID != "" {
		data["providerId"] = providerID
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if t == hostEvents {
			return hostRepeatEvent(ctx, tx, owner, data)
		}
		return poolRepeatEvent(ctx, tx, owner, evPoolProviderErr, data)
	})
	if err != nil {
		s.log.Warn("recording a provider error", "err", err)
	}
}

// terminateRequested stamps a host luxd is about to ask the provider to
// terminate, the first time.
func (s *Server) terminateRequested(ctx context.Context, hostID, reason string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE hosts SET terminate_requested_at = now()
			WHERE id = $1 AND terminate_requested_at IS NULL`, hostID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return hostEvent(ctx, tx, hostID, evTerminateRequest, map[string]any{"reason": reason})
	})
	if err != nil {
		s.log.Warn("terminate requested", "host", hostID, "err", err)
	}
}

func (s *Server) reconcileWithProvider(ctx context.Context, prov Provider, pl poolRow, st *poolState) {
	// Instances are listed with the template each was launched with (its
	// region): the pool's current one, and any older ones its hosts carry.
	templates := map[string]json.RawMessage{string(pl.Template): pl.Template}
	rows := map[string]hostRef{} // provider id → live row
	for _, h := range st.existing {
		templates[string(h.Template)] = h.Template
		rows[h.ProviderID] = h
	}
	tags := s.poolTags(pl)
	listed := map[string]bool{}
	for _, tmpl := range templates {
		insts, err := prov.Instances(ctx, tmpl, tags)
		if err != nil {
			s.log.Warn("provider check", "pool", pl.Name, "err", err)
			s.providerError(ctx, poolEvents, pl.ID, "list", "", err)
			return
		}
		for pid, inst := range insts {
			if listed[pid] {
				continue // two templates in one region list the same instances
			}
			listed[pid] = true
			gone := inst.State == "terminated" || inst.State == "shutting-down"
			h, known := rows[pid]
			switch {
			case known && gone:
				s.providerGone(ctx, h, st)
			case !known && !gone && st.launching[inst.Tags[tagHost]]:
				// A launch whose instance id is not recorded yet (in flight,
				// or luxd stopped mid-launch): its row claims it.
				s.recordProviderID(ctx, pl.ID, inst.Tags[tagHost], pid)
			case !known && !gone:
				// No live row claims it: an orphan (a launch whose reply was
				// lost, or a host written off). Terminate it.
				s.log.Warn("terminating an orphaned instance", "pool", pl.Name, "providerId", pid)
				if err := prov.Terminate(ctx, tmpl, pid); err != nil {
					s.log.Warn("terminate orphan", "providerId", pid, "err", err)
					s.providerError(ctx, poolEvents, pl.ID, "terminate orphan", pid, err)
				}
			}
		}
	}
	// Terminated instances drop out of the provider's listings after a while
	// (EC2 purges them): a settled host that is not listed is gone too. It
	// is terminated first all the same: if a listing was merely incomplete,
	// a host written off must not run on.
	for pid, h := range rows {
		if listed[pid] || !h.Settled {
			continue
		}
		s.terminateRequested(ctx, h.ID, "the provider no longer lists it")
		if err := prov.Terminate(ctx, h.Template, pid); err != nil {
			s.log.Warn("terminate unlisted", "host", h.ID, "err", err)
			s.providerError(ctx, hostEvents, h.ID, "terminate", pid, err)
			continue
		}
		s.providerGone(ctx, h, st)
	}
}

// recordProviderID claims a tagged instance for the launch whose reply was
// lost, and records it as launched (recovered: found by its tag, so only
// what a listing says, its id).
func (s *Server) recordProviderID(ctx context.Context, poolID, hostID, pid string) {
	var drained []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		retired, err := lockPoolRetired(ctx, tx, poolID)
		if err != nil {
			return err
		}
		var name string
		err = tx.QueryRow(ctx, `UPDATE hosts SET provider_id = $2 WHERE id = $1 AND provider_id IS NULL AND state <> 'terminated'
			RETURNING name`, hostID, pid).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		later := laterEvents{func() error {
			return poolEvent(ctx, tx, poolID, evHostLaunched, map[string]any{"host": hostID, "name": name, "providerId": pid, "recovered": true})
		}}
		if drained, err = s.cordonIfRetired(ctx, tx, &later, retired, hostID); err != nil {
			return err
		}
		return later.write()
	})
	if err != nil {
		s.log.Warn("record provider id", "host", hostID, "err", err)
		return
	}
	s.notifyAll(drained)
}

func (s *Server) providerGone(ctx context.Context, h hostRef, st *poolState) {
	s.markTerminated(ctx, h.ID, "the provider terminated this host")
	if !h.Draining {
		st.total--
	}
}

// poolTags are what a pool's instances are listed by: its id, never its
// name, which a rename changes.
func (s *Server) poolTags(pl poolRow) map[string]string {
	return map[string]string{tagManaged: "true", tagDeployment: s.deployment, tagPoolID: pl.ID}
}

// warm is how many idle hosts the pool keeps ready: its warm count, or
// with warm_while_active, that only while it is in use (a placement live,
// or started or ended within its scale-down time); otherwise none, so an
// idle pool goes down to its minimum. A Run waiting is served by the usual
// demand; the warm host follows once it runs.
func (s *Server) warm(pl poolRow, st *poolState) int {
	if pl.WarmWhileActive && !st.active {
		return 0
	}
	return pl.Warm
}

// scaleDownAfter is how long the pool's hosts stay idle before release.
func (s *Server) scaleDownAfter(pl poolRow) time.Duration {
	if pl.ScaleDownAfterS != nil && *pl.ScaleDownAfterS > 0 {
		return time.Duration(*pl.ScaleDownAfterS) * time.Second
	}
	return s.cfg.ScaleDownAfter
}

func (s *Server) poolState(ctx context.Context, tx pgx.Tx, pl poolRow, st *poolState) error {
	// Runs waiting for a host in this pool: those bound to it.
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r
		WHERE r.state = 'provisioning' AND NOT r.cancel_requested AND r.pool_id = $1`, pl.ID).Scan(&st.demand); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM placements p JOIN hosts h ON h.id = p.host_id
			WHERE h.pool_id = $1
			  AND (p.state IN `+livePlacementStates+` OR coalesce(p.ended_at, p.created_at) > now() - $2::interval))`,
		pl.ID, interval(s.scaleDownAfter(pl))).Scan(&st.active); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT h.id, coalesce(h.provider_id, ''), coalesce(h.launch_template, $5), h.state, h.draining,
			EXISTS (SELECT 1 FROM placements p WHERE p.host_id = h.id AND p.state IN `+livePlacementStates+`),
			EXISTS (SELECT 1 FROM blobs b WHERE b.host_id = h.id AND b.location = 'host'),
			coalesce(h.last_placement_ended_at, h.registered_at, h.created_at) < now() - $3::interval,
			h.provision_requested_at < now() - $4::interval,
			coalesce(h.lost_at < now() - $6::interval, false),
			h.tagged AND h.provision_requested_at < now() - $8::interval
			  AND coalesce(h.last_heartbeat < now() - $7::interval, true)
		FROM hosts h
		WHERE h.pool_id = $1 AND $2::text IS NOT DISTINCT FROM h.tenant_id AND h.provision_requested_at IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.ID, pl.TenantID, interval(s.scaleDownAfter(pl)), interval(s.cfg.LaunchTimeout), pl.Template, interval(s.cfg.LostGrace), interval(s.cfg.LeaseDuration), interval(s.cfg.ListingLag))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h hostRef
		var state string
		var draining, busy, pending, idleLong, launchLong, lostLong bool
		if err := rows.Scan(&h.ID, &h.ProviderID, &h.Template, &state, &draining, &busy, &pending, &idleLong, &launchLong, &lostLong, &h.Settled); err != nil {
			return err
		}
		switch {
		case h.ProviderID == "" && launchLong:
			// Launched, but its instance id was never recorded (luxd
			// stopped mid-launch, and no instance carries its tag): the
			// row goes; an instance found later is an orphan. Each in a
			// transaction of its own (reconcilePool): this one must not
			// lock a host after writing a pool event (infraevents.go).
			st.abandoned = append(st.abandoned, h.ID)
			continue
		case h.ProviderID == "":
			// Being launched (perhaps by another luxd, or this one before
			// it restarted): counted, so no one launches it twice.
			if st.launching == nil {
				st.launching = map[string]bool{}
			}
			st.launching[h.ID] = true
			st.provisioning++
			st.total++
			continue
		case state == "provisioning" && launchLong:
			h.Reason = "never registered"
			st.terminate = append(st.terminate, h)
			continue
		case state == "lost" && lostLong:
			// Its runner has been gone past the grace period; its Runs were
			// written off. The instance may still run (and bill): terminate
			// it; a replacement comes from the usual scale-up.
			h.Reason = "lost: terminated"
			st.terminate = append(st.terminate, h)
			continue
		case draining || state == "draining":
			if !busy && !pending {
				h.Reason = "drained: terminated"
				st.terminate = append(st.terminate, h)
			}
			h.Draining = true
			st.existing = append(st.existing, h)
			continue // not counted: on its way out
		case state == "provisioning":
			st.provisioning++
		case state == "ready" && !busy:
			st.idle++
			if st.idleHosts == nil {
				st.idleHosts = map[string]bool{}
			}
			st.idleHosts[h.ID] = true
			if idleLong {
				st.idleExpired = append(st.idleExpired, h.ID)
			}
		}
		st.total++
		st.existing = append(st.existing, h)
	}
	return rows.Err()
}

// launch asks the provider for one host. Its row exists first (state
// provisioning, with a one-use host token), and the instance is tagged with
// the row's id: a luxd that stops before recording the instance id still
// finds the instance by tag (reconcileWithProvider) and ends it. up, if not
// nil, is the scale-up this launch starts, recorded with it.
func (s *Server) launch(ctx context.Context, prov Provider, pl poolRow, up map[string]any) error {
	hostID := ids.New(ids.Host)
	name := fmt.Sprintf("%s-%s", pl.Name, hostID[len(hostID)-8:])
	token := ids.Secret("luxh")
	tokenID := ids.New(ids.HostToken)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// The pool may have been removed since the pass read it. FOR SHARE
		// waits for a removal in flight, and holds one off until this host
		// row commits, where the removal's drain finds it.
		if retired, err := lockPoolRetired(ctx, tx, pl.ID); err != nil || retired {
			return cmp.Or(err, errPoolRetired)
		}
		if pl.TenantID != nil {
			if err := checkHostQuota(ctx, tx, *pl.TenantID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ($1, $2, $3, $4)`,
			tokenID, pl.TenantID, pl.ID, ids.Hash(token)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool_id, token_id, name, state, provision_requested_at, launch_template, tagged)
			VALUES ($1, $2, $3, $4, $5, 'provisioning', now(), $6, true)`,
			hostID, pl.TenantID, pl.ID, tokenID, name, pl.Template)
		if err != nil {
			return err
		}
		if up != nil {
			if err := poolRepeatEvent(ctx, tx, pl.ID, evScaleUp, up); err != nil {
				return err
			}
		}
		return poolRepeatEvent(ctx, tx, pl.ID, evLaunchRequested, map[string]any{"host": hostID, "name": name}, "host", "name")
	})
	var he *HTTPError
	if errors.As(err, &he) && he.Code == "quota_exceeded" {
		return nil // at the tenant's host quota: Runs wait
	}
	if err != nil {
		return err
	}
	env := map[string]string{"LUX_URL": s.cfg.RunnerURL, "LUX_HOST_TOKEN": token, "LUX_HOST_NAME": name}
	tags := s.poolTags(pl)
	tags["Name"], tags[tagHost], tags[tagPool] = name, hostID, pl.Name
	l, launchErr := prov.Launch(ctx, pl.Template, tags, env)
	if launchErr != nil {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			if err := s.terminateTx(ctx, tx, hostID, "launch failed: "+truncate(launchErr.Error(), 200), false); err != nil {
				return err
			}
			return poolRepeatEvent(ctx, tx, pl.ID, evLaunchFailed, map[string]any{"host": hostID, "error": providerErrorText(launchErr)}, "host")
		})
		if err != nil {
			s.log.Warn("mark terminated", "host", hostID, "err", err)
		}
		return fmt.Errorf("launch in %s: %w", pl.Name, launchErr)
	}
	s.log.Info("host launched", "pool", pl.Name, "host", name, "providerId", l.ProviderID,
		"instanceType", l.InstanceType, "zone", l.Zone, "market", l.Market)
	var drained []string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		retired, err := lockPoolRetired(ctx, tx, pl.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET provider_id = $2,
				instance_type = nullif($3, ''), zone = nullif($4, ''), market = nullif($5, '')
			WHERE id = $1`, hostID, l.ProviderID, l.InstanceType, l.Zone, l.Market); err != nil {
			return err
		}
		later := laterEvents{func() error {
			return poolEvent(ctx, tx, pl.ID, evHostLaunched, map[string]any{"host": hostID, "name": name, "providerId": l.ProviderID,
				"instanceType": l.InstanceType, "zone": l.Zone, "market": l.Market})
		}}
		if drained, err = s.cordonIfRetired(ctx, tx, &later, retired, hostID); err != nil {
			return err
		}
		return later.write()
	}); err != nil {
		return err
	}
	s.notifyAll(drained)
	return nil
}

// errPoolRetired: launch found its pool removed since the pass read it,
// and launched nothing.
var errPoolRetired = errors.New("pool retired")

// lockPoolRetired locks a pool's row FOR SHARE (the first lock of the lock
// order in infraevents.go: a removal's FOR NO KEY UPDATE waits for it and
// it for the removal) and reports whether the pool is retired, or gone.
func lockPoolRetired(ctx context.Context, tx pgx.Tx, poolID string) (bool, error) {
	var retired bool
	err := tx.QueryRow(ctx, `SELECT retired FROM pools WHERE id = $1 FOR SHARE`, poolID).Scan(&retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return retired, err
}

// cordonIfRetired drains a host whose launch completes after its pool was
// removed, as the removal drains the pool's other hosts: its Runs (none
// yet) are left alone and the provisioner terminates it once idle.
func (s *Server) cordonIfRetired(ctx context.Context, tx pgx.Tx, later *laterEvents, retired bool, hostID string) ([]string, error) {
	if !retired {
		return nil, nil
	}
	return s.drainHostsLater(ctx, tx, later, poolRemovedReason, causeManual, "", "id = $1", hostID)
}

// checkHostQuota: one rule for a tenant's hosts, whether a runner
// registers or luxd launches: hosts that are, or may come, up count.
func checkHostQuota(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var n, quota int
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce((SELECT max_hosts FROM tenants WHERE id = $1), 0)
		FROM hosts WHERE tenant_id = $1 AND state IN ('provisioning', 'ready', 'draining')`, tenantID).Scan(&n, &quota); err != nil {
		return err
	}
	if quota > 0 && n >= quota {
		return errf(http.StatusTooManyRequests, "quota_exceeded", "tenant host quota reached (%d)", quota)
	}
	return nil
}

// instanceID names this luxd process in leases.
var instanceID = ids.New("luxd")

// provisionLease makes this luxd the provisioner for the next while, if
// no other one is (a row with a holder and an expiry).
func (s *Server) provisionLease(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', $1, now() + $2::interval)
			ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
				WHERE leases.holder = EXCLUDED.holder OR leases.expires_at < now()
			RETURNING true`, instanceID, interval(max(10*s.cfg.Tick, 30*time.Second))).Scan(&ok)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// releaseProvisionLease gives the lease up, if this luxd holds it.
func (s *Server) releaseProvisionLease() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM leases WHERE name = 'provisioner' AND holder = $1`, instanceID)
		return err
	})
	if err != nil {
		s.log.Warn("releasing the provisioner lease", "err", err)
	}
}

// drainForScaleDown cordons an idle host; the next pass terminates it once
// it has nothing left to upload.
// Only if it is still idle: the scheduler may have placed a Run on it
// since the pool was counted.
func (s *Server) drainForScaleDown(ctx context.Context, hostID string) (bool, error) {
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		hosts, err = s.drainHosts(ctx, tx, "idle: scaling down", causeScaleDown, "", `id = $1 AND state = 'ready'
			AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.host_id = hosts.id AND p.state IN `+livePlacementStates+`)`, hostID)
		return err
	})
	s.notifyAll(hosts)
	return len(hosts) > 0, err
}

func (s *Server) markTerminated(ctx context.Context, hostID, reason string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.markTerminatedTx(ctx, tx, hostID, reason)
	})
	if err != nil {
		s.log.Warn("mark terminated", "host", hostID, "err", err)
		return
	}
	s.hub.Disconnect(hostID)
}

func (s *Server) markTerminatedTx(ctx context.Context, tx pgx.Tx, hostID, reason string) error {
	return s.terminateTx(ctx, tx, hostID, reason, true)
}

// terminateTx marks a host terminated. With record, the first time it
// records host.terminated, and pool.host_released on its pool with why the
// pool let it go; a host whose launch failed records neither
// (pool.launch_failed says it all, once for many attempts).
func (s *Server) terminateTx(ctx context.Context, tx pgx.Tx, hostID, reason string, record bool) error {
	var was, name string
	var causes []string
	var idle *float64
	var retired bool
	err := tx.QueryRow(ctx, `WITH old AS (
			SELECT h.id, h.state, h.name, h.drain_causes,
				extract(epoch FROM coalesce(h.drain_requested_at, now()) - coalesce(h.last_placement_ended_at, h.registered_at))::float8 AS idle,
				coalesce((SELECT p.retired FROM pools p WHERE p.id = h.pool_id), false) AS retired
			FROM hosts h WHERE h.id = $1 FOR NO KEY UPDATE OF h)
		UPDATE hosts SET state = 'terminated', state_reason = $2,
			terminate_requested_at = coalesce(terminate_requested_at, now()), terminated_at = now()
		FROM old WHERE hosts.id = old.id
		RETURNING old.state, old.name, old.drain_causes, old.idle, old.retired`, hostID, reason).Scan(&was, &name, &causes, &idle, &retired)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	found := err == nil
	if err := hostsGone(ctx, tx, []string{hostID}); err != nil {
		return err
	}
	// Its host token was made for it alone (launch): spent.
	if _, err := tx.Exec(ctx, `UPDATE host_tokens SET revoked_at = now()
		WHERE id = (SELECT token_id FROM hosts WHERE id = $1 AND provision_requested_at IS NOT NULL)`, hostID); err != nil {
		return err
	}
	// The events last: event streams come after every row lock.
	if !record || !found || was == "terminated" {
		return nil
	}
	if err := hostEvent(ctx, tx, hostID, evTerminated, map[string]any{"reason": reason}); err != nil {
		return err
	}
	d := map[string]any{"host": hostID, "name": name, "reason": releaseReason(causes, retired, reason), "detail": reason}
	if slices.Contains(causes, causeScaleDown) && idle != nil {
		d["idleSeconds"] = int(*idle)
	}
	return hostPoolEvent(ctx, tx, hostID, evHostReleased, d)
}

// releaseReason says why a pool let a host go, from why it was drained
// (idle, pool removed, outdated, manual, evicted), or else from why it was
// terminated (never registered, lost, gone at the provider).
func releaseReason(causes []string, retired bool, reason string) string {
	switch {
	case slices.Contains(causes, causePreempt):
		return "evicted"
	case slices.Contains(causes, causeManual) && retired:
		return "pool removed"
	case slices.Contains(causes, causeManual):
		return "manual"
	case slices.Contains(causes, causeOutdated):
		return "outdated"
	case slices.Contains(causes, causeScaleDown):
		return "idle"
	}
	return reason
}

// hostsGone: the hosts' copies of snapshots are gone with them; uploaded
// snapshots stay available.
func hostsGone(ctx context.Context, tx pgx.Tx, hosts []string) error {
	_, err := tx.Exec(ctx, `UPDATE snapshots SET available = available AND uploaded, host_copy = false
		WHERE host_id = ANY($1)`, hosts)
	return err
}
