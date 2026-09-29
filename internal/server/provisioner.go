package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// Describe looks hosts up by provider id, which unlike a tag listing
	// does not lag behind tag changes. An id the provider does not know is
	// absent from the result, not an error.
	Describe(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]Instance, error)
	// Retag sets one tag on hosts it launched (a pool rename).
	Retag(ctx context.Context, template json.RawMessage, providerIDs []string, key, value string) error
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
	tagPool       = "lux:pool"
	tagHost       = "lux:host" // the host row's id
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
	// RenamedFrom: the old name while a rename is unfinished; RetaggedAt:
	// when the provisioner last re-tagged some of its instances; Aliases:
	// every name the pool's instances may still carry (poolrename.go).
	// RenamedAt: when that rename committed.
	RenamedFrom *string
	RenamedAt   *time.Time
	RetaggedAt  *time.Time
	Aliases     []string
}

// poolRowColumns, selected FROM pools (not aliased).
const poolRowColumns = `id, name, provider, tenant_id, template, min_hosts, max_hosts, warm_hosts, retired,
	scale_down_after_s, warm_while_active,
	(SELECT a.name FROM pool_tag_aliases a WHERE a.pool_id = pools.id AND a.retired_at IS NULL AND a.finished_at IS NULL),
	(SELECT a.added_at FROM pool_tag_aliases a WHERE a.pool_id = pools.id AND a.retired_at IS NULL AND a.finished_at IS NULL),
	retagged_at,
	ARRAY(SELECT a.name FROM pool_tag_aliases a WHERE a.pool_id = pools.id AND a.retired_at IS NULL ORDER BY a.id)`

func (s *Server) provision(ctx context.Context) error {
	// Leadership: a lease in the database, not a lock held on a connection
	// across provider calls. One luxd reconciles at a time; another takes
	// over when the lease lapses.
	lease, err := s.provisionLease(ctx)
	if err != nil || lease == nil {
		return err
	}
	var pools []poolRow
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+poolRowColumns+`
			FROM pools WHERE provider <> 'static'
			  AND (NOT retired OR EXISTS (SELECT 1 FROM pool_tag_aliases a WHERE a.pool_id = pools.id AND a.retired_at IS NULL)
			       OR EXISTS (SELECT 1 FROM hosts h WHERE h.pool = pools.name
			       AND coalesce(h.tenant_id, '') = coalesce(pools.tenant_id, '') AND h.state <> 'terminated'))`)
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
		if err := s.reconcilePool(ctx, prov, pl, checkAlive, lease); err != nil {
			if errors.Is(err, errFenced) {
				s.log.Warn("provisioner: lease lost mid-pass; pass abandoned", "pool", pl.Name)
				return nil
			}
			s.log.Warn("pool", "pool", pl.Name, "err", err)
		}
	}
	return nil
}

// poolState is what a pool has and needs, counted in one transaction.
type poolState struct {
	demand, idle, provisioning, total int
	// active: a placement started or ended on the pool's hosts within its
	// scale-down time (warm_while_active keeps warm hosts only then).
	active bool
	// Idle hosts that have been idle longer than the cooldown, oldest first.
	idleExpired []string
	// Hosts to terminate now: drained ones that are done (no live
	// placements, nothing to upload), ones that never registered, and lost
	// ones (their runner stopped answering; the instance may still run).
	terminate []hostRef
	// Hosts the provider should still have (checked every ProviderCheckEvery).
	existing []hostRef
	// Hosts launched whose instance id is not recorded yet, by id.
	launching map[string]bool
}

// hostRef is a provisioned host, with the template it was launched with
// (its region), whatever its pool's template says now.
type hostRef struct {
	ID, ProviderID, Reason string
	Template               json.RawMessage
	Draining               bool // not counted in the pool's total
	// Listable: launched with the tags we list by, long enough ago that
	// the provider lists it (its listings are eventually consistent).
	// Settled: that, and its runner is not heartbeating either; one missing
	// from the listings may be gone.
	Listable, Settled bool
}

func (s *Server) reconcilePool(ctx context.Context, prov Provider, pl poolRow, checkAlive bool, lease *passLease) error {
	// Provider calls can be slow: still the provisioner, and the same
	// holding of the lease this pass began with?
	if err := s.renewLease(ctx, lease); err != nil {
		return err
	}
	var st poolState
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// poolState may write off a launch that never completed.
		if _, err := s.fenceTx(ctx, tx, lease); err != nil {
			return err
		}
		// The row as of now, held until its hosts are read: a rename that
		// committed since the pools were listed must not pair the old name
		// with hosts that carry the new one.
		rows, err := tx.Query(ctx, `SELECT `+poolRowColumns+` FROM pools WHERE id = $1 FOR SHARE`, pl.ID)
		if err != nil {
			return err
		}
		if pl, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[poolRow]); err != nil {
			return err
		}
		return s.poolState(ctx, tx, pl, &st)
	})
	if err != nil {
		return err
	}

	// Every ProviderCheckEvery (provider API limits), the provider's view of the
	// pool, by tag: hosts it terminated behind our back go (their Runs are
	// lost by the usual heartbeat path); instances of this pool that no
	// live row claims (a launch whose reply was lost) are terminated.
	if checkAlive {
		if err := s.reconcileWithProvider(ctx, prov, pl, &st, lease); err != nil {
			return err
		}
	}

	// Scale down: drain idle hosts beyond what warm and waiting Runs need
	// (and never below the minimum), terminate what is done.
	for _, id := range st.idleExpired {
		if st.idle <= s.warm(pl, &st)+st.demand || st.total <= pl.Min {
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
		err := s.fencedCall(ctx, lease, nil, func(ctx context.Context) error {
			return prov.Terminate(ctx, h.Template, h.ProviderID)
		})
		if errors.Is(err, errFenced) {
			return err
		}
		if err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			continue
		}
		if err := s.writeOff(ctx, lease, h.ID, h.Reason); err != nil {
			return err
		}
	}

	// Scale up: enough for what waits plus the warm hosts, at least the
	// minimum, never past the maximum. A retired pool only winds down.
	if pl.Retired {
		return nil
	}
	want := max(pl.Min-st.total, s.warm(pl, &st)+st.demand-st.idle-st.provisioning)
	if pl.Max > 0 {
		want = min(want, pl.Max-st.total)
	}
	for i := range max(want, 0) {
		if i > 0 {
			if err := s.renewLease(ctx, lease); err != nil {
				return err
			}
		}
		lctx, cancel := lease.bound(ctx)
		err := s.launch(lctx, prov, pl)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) reconcileWithProvider(ctx context.Context, prov Provider, pl poolRow, st *poolState, lease *passLease) error {
	// Instances are listed with the template each was launched with (its
	// region): the pool's current one, and any older ones its hosts carry.
	templates := map[string]json.RawMessage{string(pl.Template): pl.Template}
	rows := map[string]hostRef{} // provider id → live row
	for _, h := range st.existing {
		templates[string(h.Template)] = h.Template
		rows[h.ProviderID] = h
	}
	// A renamed pool is listed under every name its instances may still
	// carry too (poolrename.go): its instances are its own whichever it is.
	tagSets := []map[string]string{s.poolTags(pl)}
	aliasOf := map[string]string{} // lux:pool value → alias
	for _, a := range pl.Aliases {
		tags := s.poolTags(pl)
		tags[tagPool] = poolTagValue(pl.TenantID, a)
		aliasOf[tags[tagPool]] = a
		tagSets = append(tagSets, tags)
	}
	rc := renameCheck{stale: map[string][]string{}, templates: templates, liveUnder: map[string]bool{}, checkedAt: time.Now()}
	listed := map[string]bool{}
	listedNew := map[string]bool{} // under the pool's current name
	type orphan struct {
		pid  string
		tmpl json.RawMessage
		inst Instance
	}
	var orphans []orphan
	var gone []hostRef
	for key, tmpl := range templates {
		for i, tags := range tagSets {
			lctx, cancel := lease.bound(ctx)
			insts, err := prov.Instances(lctx, tmpl, tags)
			cancel()
			if err != nil {
				s.log.Warn("provider check", "pool", pl.Name, "err", err)
				return nil
			}
			for pid, inst := range insts {
				if i == 0 {
					listedNew[pid] = true
				}
				if listed[pid] {
					continue // two templates in one region, or two names, list the same instances
				}
				listed[pid] = true
				isGone := instanceGone(inst)
				h, known := rows[pid]
				if alias, old := aliasOf[inst.Tags[tagPool]]; old && !isGone {
					rc.liveUnder[alias] = true
					if known {
						rc.stale[key] = append(rc.stale[key], pid)
					} else {
						// A launch not recorded yet, or an orphan terminated
						// below: the rename waits for either to settle.
						rc.unlisted = true
					}
				}
				switch {
				case known && isGone:
					gone = append(gone, h)
				case !known && !isGone && st.launching[inst.Tags[tagHost]]:
					// A launch whose instance id is not recorded yet (in flight,
					// or luxd stopped mid-launch): its row claims it.
					s.recordProviderID(ctx, inst.Tags[tagHost], pid)
				case !known && !isGone:
					orphans = append(orphans, orphan{pid, tmpl, inst})
				}
			}
		}
	}
	for _, h := range gone {
		if err := s.writeOff(ctx, lease, h.ID, "the provider terminated this host"); err != nil {
			return err
		}
		if !h.Draining {
			st.total--
		}
	}
	for _, o := range orphans {
		if err := s.terminateOrphan(ctx, prov, pl, lease, o.tmpl, o.pid, o.inst); err != nil {
			return err
		}
	}

	// Terminated instances drop out of the provider's listings after a while
	// (EC2 purges them): a settled host that is not listed may be gone. A
	// tag listing alone never says so (it lags tag changes, a rename's or a
	// late one's): each host missing from it is looked up by id
	// (checkUnlisted). Not within listing_lag of a rename's re-tag, when an
	// instance may briefly match no name.
	retagging := pl.RetaggedAt != nil && time.Since(*pl.RetaggedAt) < s.cfg.ListingLag
	unlisted := map[string][]hostRef{} // by template
	for pid, h := range rows {
		if !listedNew[pid] {
			rc.unlisted = true
		}
		if listed[pid] || !h.Listable || retagging {
			continue
		}
		unlisted[string(h.Template)] = append(unlisted[string(h.Template)], h)
	}
	for _, hosts := range unlisted {
		if err := s.checkUnlisted(ctx, prov, pl, st, lease, hosts); err != nil {
			return err
		}
	}
	if len(pl.Aliases) > 0 {
		return s.followAliases(ctx, prov, pl, st, rc, lease)
	}
	return nil
}

func instanceGone(inst Instance) bool {
	return inst.State == "terminated" || inst.State == "shutting-down"
}

// checkUnlisted looks up by id settled hosts (of one template) that no
// listing showed, writes off the ones the provider says are gone, and
// keeps the others, re-tagging any whose lux:pool is not the pool's.
func (s *Server) checkUnlisted(ctx context.Context, prov Provider, pl poolRow, st *poolState, lease *passLease, hosts []hostRef) error {
	pids := make([]string, len(hosts))
	for i, h := range hosts {
		pids[i] = h.ProviderID
	}
	dctx, cancel := lease.bound(ctx)
	insts, err := prov.Describe(dctx, hosts[0].Template, pids)
	cancel()
	if err != nil {
		s.log.Warn("provider check: describing unlisted hosts; nothing done this pass", "pool", pl.Name, "err", err)
		return nil
	}
	// The pool's name as this pass read it, only to spot a tag that
	// differs; retagOne re-tags with the name the host row has then.
	want := poolTagValue(pl.TenantID, pl.Name)
	for _, h := range hosts {
		inst, known := insts[h.ProviderID]
		if (!known || instanceGone(inst)) && h.Settled {
			if err := s.writeOff(ctx, lease, h.ID, "the provider terminated this host"); err != nil {
				return err
			}
			if !h.Draining {
				st.total--
			}
			continue
		}
		if !known || instanceGone(inst) {
			continue // its runner still heartbeats: the reaper decides
		}
		s.log.Warn("provider check: a host missing from its pool's listing is still up; kept",
			"pool", pl.Name, "host", h.ID, "providerId", h.ProviderID, "state", inst.State, "lux:pool", inst.Tags[tagPool])
		if inst.Tags[tagPool] == want || inst.Tags[tagHost] != h.ID {
			continue
		}
		if err := s.retagOne(ctx, prov, lease, h.Template, h.ProviderID, h.ID); err != nil {
			return err
		}
	}
	return nil
}

// terminateOrphan ends an instance of the pool that no live host row read
// by this pass claims (a launch whose reply was lost, or a host written
// off) — unless, checked again at the last moment, a live host row of the
// pool's owner does claim it, under whatever pool name: a rename or a
// launch may have committed after the pass read its rows.
func (s *Server) terminateOrphan(ctx context.Context, prov Provider, pl poolRow, lease *passLease, tmpl json.RawMessage, pid string, inst Instance) error {
	hostID := inst.Tags[tagHost]
	var claimed bool
	var pool string
	err := s.fencedCall(ctx, lease, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE hosts SET provider_id = coalesce(provider_id, $3)
			WHERE id = $1 AND coalesce(tenant_id, '') = coalesce($2, '') AND state <> 'terminated'
			  AND coalesce(provider_id, $3) = $3
			RETURNING pool`, hostID, pl.TenantID, pid).Scan(&pool)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		claimed = err == nil
		return err
	}, func(ctx context.Context) error {
		if claimed {
			return nil
		}
		s.log.Warn("terminating an orphaned instance", "pool", pl.Name, "providerId", pid)
		return prov.Terminate(ctx, tmpl, pid)
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("terminate orphan", "providerId", pid, "err", err)
		return nil
	}
	if !claimed {
		return nil
	}
	s.log.Warn("provider check: an instance no host row read by this pass claimed is its host's; kept",
		"pool", pl.Name, "host", hostID, "providerId", pid, "hostPool", pool, "lux:pool", inst.Tags[tagPool])
	if inst.Tags[tagPool] == poolTagValue(pl.TenantID, pool) {
		return nil
	}
	return s.retagOne(ctx, prov, lease, tmpl, pid, hostID)
}

// retagOne sets an instance's lux:pool to its host row's pool, read at
// the call (a rename may have moved it since the pass began); a refusal is
// logged and retried by a later pass.
func (s *Server) retagOne(ctx context.Context, prov Provider, lease *passLease, tmpl json.RawMessage, pid, hostID string) error {
	var value string
	err := s.fencedCall(ctx, lease, func(tx pgx.Tx) error {
		var tenantID *string
		var pool string
		err := tx.QueryRow(ctx, `SELECT tenant_id, pool FROM hosts WHERE id = $1 AND provider_id = $2 AND state <> 'terminated'`,
			hostID, pid).Scan(&tenantID, &pool)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		value = poolTagValue(tenantID, pool)
		return err
	}, func(ctx context.Context) error {
		if value == "" {
			return nil
		}
		return prov.Retag(ctx, tmpl, []string{pid}, tagPool, value)
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("re-tagging an instance", "providerId", pid, "lux:pool", value, "err", err)
		return nil
	}
	if value != "" {
		s.log.Warn("provider check: instance re-tagged with its pool's current name", "providerId", pid, "lux:pool", value)
	}
	return nil
}

func (s *Server) recordProviderID(ctx context.Context, hostID, pid string) {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET provider_id = $2 WHERE id = $1 AND provider_id IS NULL AND state <> 'terminated'`, hostID, pid)
		return err
	})
	if err != nil {
		s.log.Warn("record provider id", "host", hostID, "err", err)
	}
}

// poolTags are the tags every instance of a pool carries, and what its
// instances are listed by. Tenant pools are named by tenant and name.
func (s *Server) poolTags(pl poolRow) map[string]string {
	return map[string]string{tagManaged: "true", tagDeployment: s.deployment, tagPool: poolTagValue(pl.TenantID, pl.Name)}
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
	// Runs waiting for a host in this pool. A tenant pool serves its
	// tenant; a platform pool anyone whose Runs name it (and who has no
	// pool of their own by that name).
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r
		WHERE r.state = 'provisioning' AND NOT r.cancel_requested
		  AND coalesce(r.spec->'placement'->>'pool', 'default') = $1
		  AND CASE WHEN $2::text IS NULL
		           THEN NOT EXISTS (SELECT 1 FROM pools o WHERE o.tenant_id = r.tenant_id AND o.name = $1 AND NOT o.retired)
		           ELSE r.tenant_id = $2 END`, pl.Name, pl.TenantID).Scan(&st.demand); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM placements p JOIN hosts h ON h.id = p.host_id
			WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '')
			  AND (p.state IN `+livePlacementStates+` OR coalesce(p.ended_at, p.created_at) > now() - $3::interval))`,
		pl.Name, pl.TenantID, interval(s.scaleDownAfter(pl))).Scan(&st.active); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT h.id, coalesce(h.provider_id, ''), coalesce(h.launch_template, $5), h.state, h.draining,
			EXISTS (SELECT 1 FROM placements p WHERE p.host_id = h.id AND p.state IN `+livePlacementStates+`),
			EXISTS (SELECT 1 FROM blobs b WHERE b.host_id = h.id AND b.location = 'host'),
			coalesce(h.last_placement_ended_at, h.registered_at, h.created_at) < now() - $3::interval,
			h.provision_requested_at < now() - $4::interval,
			coalesce(h.lost_at < now() - $6::interval, false),
			h.tagged AND h.provision_requested_at < now() - $8::interval,
			coalesce(h.last_heartbeat < now() - $7::interval, true)
		FROM hosts h
		WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '') AND h.provision_requested_at IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.Name, pl.TenantID, interval(s.scaleDownAfter(pl)), interval(s.cfg.LaunchTimeout), pl.Template, interval(s.cfg.LostGrace), interval(s.cfg.LeaseDuration), interval(s.cfg.ListingLag))
	if err != nil {
		return err
	}
	defer rows.Close()
	var neverCompleted []string
	for rows.Next() {
		var h hostRef
		var state string
		var draining, busy, pending, idleLong, launchLong, lostLong, silent bool
		if err := rows.Scan(&h.ID, &h.ProviderID, &h.Template, &state, &draining, &busy, &pending, &idleLong, &launchLong, &lostLong, &h.Listable, &silent); err != nil {
			return err
		}
		h.Settled = h.Listable && silent
		switch {
		case h.ProviderID == "" && launchLong:
			// Launched, but its instance id was never recorded (luxd
			// stopped mid-launch, and no instance carries its tag): the
			// row goes; an instance found later is an orphan.
			neverCompleted = append(neverCompleted, h.ID)
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
			if idleLong {
				st.idleExpired = append(st.idleExpired, h.ID)
			}
		}
		st.total++
		st.existing = append(st.existing, h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// After the rows are read: the connection runs one statement at a time.
	rows.Close()
	for _, id := range neverCompleted {
		if err := s.markTerminatedTx(ctx, tx, id, "launch never completed"); err != nil {
			return err
		}
	}
	return nil
}

// launch asks the provider for one host. Its row exists first (state
// provisioning, with a one-use host token), and the instance is tagged with
// the row's id: a luxd that stops before recording the instance id still
// finds the instance by tag (reconcileWithProvider) and ends it.
func (s *Server) launch(ctx context.Context, prov Provider, pl poolRow) error {
	hostID := ids.New(ids.Host)
	name := fmt.Sprintf("%s-%s", pl.Name, hostID[len(hostID)-8:])
	token := ids.Secret("luxh")
	tokenID := ids.New(ids.HostToken)
	renamed := false
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Not under a name the pool has just left (a rename): the row
		// would join no pool. Held until commit, so a rename after it
		// moves this row too, and the provisioner re-tags its instance.
		var current string
		if err := tx.QueryRow(ctx, `SELECT name FROM pools WHERE id = $1 FOR SHARE`, pl.ID).Scan(&current); err != nil {
			return err
		}
		if renamed = current != pl.Name; renamed {
			return nil
		}
		if pl.TenantID != nil {
			if err := checkHostQuota(ctx, tx, *pl.TenantID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ($1, $2, $3, $4)`,
			tokenID, pl.TenantID, pl.Name, ids.Hash(token)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool, token_id, name, state, provision_requested_at, launch_template, tagged)
			VALUES ($1, $2, $3, $4, $5, 'provisioning', now(), $6, true)`,
			hostID, pl.TenantID, pl.Name, tokenID, name, pl.Template)
		return err
	})
	var he *HTTPError
	if errors.As(err, &he) && he.Code == "quota_exceeded" {
		return nil // at the tenant's host quota: Runs wait
	}
	if err != nil || renamed {
		return err
	}
	env := map[string]string{"LUX_URL": s.cfg.RunnerURL, "LUX_HOST_TOKEN": token, "LUX_HOST_NAME": name}
	tags := s.poolTags(pl)
	tags["Name"], tags[tagHost] = name, hostID
	l, err := prov.Launch(ctx, pl.Template, tags, env)
	if err != nil {
		s.markTerminated(ctx, hostID, "launch failed: "+truncate(err.Error(), 200))
		return fmt.Errorf("launch in %s: %w", pl.Name, err)
	}
	s.log.Info("host launched", "pool", pl.Name, "host", name, "providerId", l.ProviderID,
		"instanceType", l.InstanceType, "zone", l.Zone, "market", l.Market)
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET provider_id = $2,
				instance_type = nullif($3, ''), zone = nullif($4, ''), market = nullif($5, '')
			WHERE id = $1`, hostID, l.ProviderID, l.InstanceType, l.Zone, l.Market)
		return err
	}); err != nil {
		return err
	}
	return nil
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

// errFenced: the provisioner lease was lost, or held by another luxd,
// since the pass began. The pass stops before acting on what it read.
var errFenced = errors.New("the provisioner lease changed hands during the pass")

// passLease is a provisioner pass's hold on the lease: the fencing token
// it began with, and when, by this process's clock, the lease expires at
// the latest.
type passLease struct {
	token   int64
	expires time.Time
}

// bound limits a provider call to the lease: cancelled before another luxd
// can take it over.
func (l *passLease) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, l.expires)
}

func (s *Server) provisionLeaseDuration() time.Duration {
	return max(10*s.cfg.Tick, 30*time.Second)
}

// provisionLease makes this luxd the provisioner for the next while, if
// no other one is (a row with a holder and an expiry); nil if another is.
// A different holder taking the lease draws a new token.
func (s *Server) provisionLease(ctx context.Context) (*passLease, error) {
	start := time.Now()
	var l passLease
	var left float64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO leases (name, holder, expires_at, token) VALUES ('provisioner', $1, now() + $2::interval, nextval('lease_tokens'))
			ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at,
				token = CASE WHEN leases.holder = EXCLUDED.holder THEN leases.token ELSE EXCLUDED.token END
				WHERE leases.holder = EXCLUDED.holder OR leases.expires_at < now()
			RETURNING token, extract(epoch FROM expires_at - now())::float8`, s.id, interval(s.provisionLeaseDuration())).Scan(&l.token, &left)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Measured from before the transaction began: never later than the
	// database's own expiry.
	l.expires = start.Add(time.Duration(left * float64(time.Second)))
	return &l, nil
}

// renewLease extends the pass's lease, or says it is fenced: another luxd
// took it (a different token) or it could not be renewed.
func (s *Server) renewLease(ctx context.Context, l *passLease) error {
	now, err := s.provisionLease(ctx)
	if err != nil {
		return err
	}
	if now == nil || now.token != l.token {
		return errFenced
	}
	l.expires = now.expires
	return nil
}

// fenceTx checks, in tx, that the pass still holds the lease it began
// with (same holder, same token, not expired) and renews it. The lease row
// stays locked until tx ends, so no other luxd takes the lease between
// this check and what tx decides. It returns when the lease expires, by
// this process's clock.
func (s *Server) fenceTx(ctx context.Context, tx pgx.Tx, l *passLease) (time.Time, error) {
	start := time.Now()
	var left float64
	err := tx.QueryRow(ctx, `UPDATE leases SET expires_at = now() + $3::interval
		WHERE name = 'provisioner' AND holder = $1 AND token = $2 AND expires_at > now()
		RETURNING extract(epoch FROM expires_at - now())::float8`, s.id, l.token, interval(s.provisionLeaseDuration())).Scan(&left)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, errFenced
	}
	if err != nil {
		return time.Time{}, err
	}
	return start.Add(time.Duration(left * float64(time.Second))), nil
}

// fencedCall runs check (may be nil) in a transaction that confirms the
// lease, then call with a deadline no later than the lease's expiry. It
// returns errFenced, without calling, if the lease changed hands.
func (s *Server) fencedCall(ctx context.Context, l *passLease, check func(pgx.Tx) error, call func(context.Context) error) error {
	var until time.Time
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		if until, err = s.fenceTx(ctx, tx, l); err != nil {
			return err
		}
		if check != nil {
			return check(tx)
		}
		return nil
	})
	if err != nil {
		return err
	}
	l.expires = until
	cctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	return call(cctx)
}

// writeOff marks a provisioned host terminated, if the pass still holds
// its lease.
func (s *Server) writeOff(ctx context.Context, l *passLease, hostID, reason string) error {
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := s.fenceTx(ctx, tx, l); err != nil {
			return err
		}
		return s.markTerminatedTx(ctx, tx, hostID, reason)
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("mark terminated", "host", hostID, "err", err)
		return nil
	}
	s.hub.Disconnect(hostID)
	return nil
}

// releaseProvisionLease gives the lease up, if this luxd holds it.
func (s *Server) releaseProvisionLease() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM leases WHERE name = 'provisioner' AND holder = $1`, s.id)
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
	_, err := tx.Exec(ctx, `UPDATE hosts SET state = 'terminated', state_reason = $2,
			terminate_requested_at = coalesce(terminate_requested_at, now()), terminated_at = now()
		WHERE id = $1`, hostID, reason)
	if err != nil {
		return err
	}
	if err := hostsGone(ctx, tx, []string{hostID}); err != nil {
		return err
	}
	// Its host token was made for it alone (launch): spent.
	_, err = tx.Exec(ctx, `UPDATE host_tokens SET revoked_at = now()
		WHERE id = (SELECT token_id FROM hosts WHERE id = $1 AND provision_requested_at IS NOT NULL)`, hostID)
	return err
}

// hostsGone: the hosts' copies of snapshots are gone with them; uploaded
// snapshots stay available.
func hostsGone(ctx context.Context, tx pgx.Tx, hosts []string) error {
	_, err := tx.Exec(ctx, `UPDATE snapshots SET available = available AND uploaded, host_copy = false
		WHERE host_id = ANY($1)`, hosts)
	return err
}
