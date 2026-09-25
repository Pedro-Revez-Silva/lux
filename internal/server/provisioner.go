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
	// id. env is what its runner starts with (URL, host token, name).
	Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (string, error)
	// Terminate ends a host. One that no longer exists is done.
	Terminate(ctx context.Context, template json.RawMessage, providerID string) error
	// Instances lists the provider's hosts carrying all the given tags,
	// by provider id.
	Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error)
}

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

// lostGrace: a lost provisioned host is terminated only after this long
// without its runner (a restart or a network blip is not a loss).
const lostGrace = 5 * time.Minute

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
			  AND (NOT retired OR EXISTS (SELECT 1 FROM hosts h WHERE h.pool = pools.name
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
	checkAlive := time.Since(s.lastAliveCheck) > time.Minute
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
	// active: a placement started or ended on the pool's hosts within its
	// scale-down time (warm_while_active keeps warm hosts only then).
	active bool
	// Idle hosts that have been idle longer than the cooldown, oldest first.
	idleExpired []string
	// Hosts to terminate now: drained ones that are done (no live
	// placements, nothing to upload), ones that never registered, and lost
	// ones (their runner stopped answering; the instance may still run).
	terminate []hostRef
	// Hosts the provider should still have (checked once a minute).
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

	// Once a minute (provider API limits), the provider's view of the
	// pool, by tag: hosts it terminated behind our back go (their Runs are
	// lost by the usual heartbeat path); instances of this pool that no
	// live row claims (a launch whose reply was lost) are terminated.
	if checkAlive {
		s.reconcileWithProvider(ctx, prov, pl, &st)
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
		if err := prov.Terminate(ctx, h.Template, h.ProviderID); err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			continue
		}
		s.markTerminated(ctx, h.ID, h.Reason)
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
			if ok, err := s.provisionLease(ctx); err != nil || !ok {
				return err
			}
		}
		if err := s.launch(ctx, prov, pl); err != nil {
			return err
		}
	}
	return nil
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
				s.recordProviderID(ctx, inst.Tags[tagHost], pid)
			case !known && !gone:
				// No live row claims it: an orphan (a launch whose reply was
				// lost, or a host written off). Terminate it.
				s.log.Warn("terminating an orphaned instance", "pool", pl.Name, "providerId", pid)
				if err := prov.Terminate(ctx, tmpl, pid); err != nil {
					s.log.Warn("terminate orphan", "providerId", pid, "err", err)
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
		if err := prov.Terminate(ctx, h.Template, pid); err != nil {
			s.log.Warn("terminate unlisted", "host", h.ID, "err", err)
			continue
		}
		s.providerGone(ctx, h, st)
	}
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

func (s *Server) providerGone(ctx context.Context, h hostRef, st *poolState) {
	s.markTerminated(ctx, h.ID, "the provider terminated this host")
	if !h.Draining {
		st.total--
	}
}

// poolTags are the tags every instance of a pool carries, and what its
// instances are listed by. Tenant pools are named by tenant and name.
func (s *Server) poolTags(pl poolRow) map[string]string {
	pool := pl.Name
	if pl.TenantID != nil {
		pool = *pl.TenantID + "/" + pl.Name
	}
	return map[string]string{tagManaged: "true", tagDeployment: s.deployment, tagPool: pool}
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
			h.tagged AND h.provision_requested_at < now() - interval '1 minute'
			  AND coalesce(h.last_heartbeat < now() - $7::interval, true)
		FROM hosts h
		WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '') AND h.provision_requested_at IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.Name, pl.TenantID, interval(s.scaleDownAfter(pl)), interval(s.cfg.LaunchTimeout), pl.Template, interval(lostGrace), interval(s.cfg.LeaseDuration))
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
			// row goes; an instance found later is an orphan.
			if err := s.markTerminatedTx(ctx, tx, h.ID, "launch never completed"); err != nil {
				return err
			}
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
	return rows.Err()
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
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
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
	if err != nil {
		return err
	}
	env := map[string]string{"LUX_URL": s.cfg.RunnerURL, "LUX_HOST_TOKEN": token, "LUX_HOST_NAME": name}
	tags := s.poolTags(pl)
	tags["Name"], tags[tagHost] = name, hostID
	pid, err := prov.Launch(ctx, pl.Template, tags, env)
	if err != nil {
		s.markTerminated(ctx, hostID, "launch failed: "+truncate(err.Error(), 200))
		return fmt.Errorf("launch in %s: %w", pl.Name, err)
	}
	s.log.Info("host launched", "pool", pl.Name, "host", name, "providerId", pid)
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE hosts SET provider_id = $2 WHERE id = $1`, hostID, pid)
		return err
	})
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
