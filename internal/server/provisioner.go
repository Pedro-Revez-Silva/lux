package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Describe looks hosts up by provider id, which unlike a tag listing
	// does not lag behind tag changes. An id the provider does not know is
	// absent from the result, not an error; it may be one just launched.
	Describe(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]Instance, error)
	// Retag sets one tag on hosts it launched: lux:pool-id on instances
	// launched without it, lux:pool after a pool rename.
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
// a row written off too early). They are listed by lux:pool-id, which
// never changes; lux:pool, the pool's name, follows a rename afterwards
// and is for people and cost tooling (poolrename.go).
const (
	tagManaged    = "lux:managed"
	tagDeployment = "lux:deployment" // which lux database launched it
	tagPool       = "lux:pool"
	tagPoolID     = "lux:pool-id" // pools.id
	tagHost       = "lux:host"    // the host row's id
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
	// Checked in first: the lease fence refuses a luxd that has not.
	select {
	case <-ctx.Done():
		return
	case <-s.checkedIn:
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
	// RenamedAt: when the pool was last renamed; PreviousNames: the names
	// it had.
	RenamedAt          *time.Time
	PreviousNames      []string
	IDMigratedAt       *time.Time
	RetiredAt          *time.Time
	LastEmptyListingAt *time.Time
	Legacy             bool // not yet migrated: list by name too
}

// poolRowColumns, selected FROM pools (not aliased).
const poolRowColumns = `id, name, provider, tenant_id, template, min_hosts, max_hosts, warm_hosts, retired,
	scale_down_after_s, warm_while_active, renamed_at, previous_names,
	id_migrated_at, retired_at, last_empty_listing_at, id_migrated_at IS NULL`

// poolDiscoveryLag covers EC2's eventual listing delay even after a lost
// RunInstances reply has been written off. It is not the shorter lag used
// to verify a known instance by id.
const poolDiscoveryLag = 10 * time.Minute

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
			  AND (NOT retired OR retired_at IS NULL OR retired_at > now() - $1::interval
			       OR last_empty_listing_at IS NULL OR last_empty_listing_at < retired_at
			       OR EXISTS (SELECT 1 FROM hosts h WHERE h.pool = pools.name
			       AND coalesce(h.tenant_id, '') = coalesce(pools.tenant_id, '') AND h.state <> 'terminated'))`,
			interval(s.cfg.LaunchTimeout+poolDiscoveryLag))
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
	// Tags the provider check found to add (applyTags).
	tags pendingTags
	// Hosts whose launch never completed: written off.
	abandoned []string
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
	// NotFound: a provider check found its instance unknown (not_found_since
	// set). NotFoundLong: listing_lag or longer ago, on a host created more
	// than LaunchTimeout plus listing_lag ago: unknown again, it is gone.
	NotFound, NotFoundLong bool
	// PoolIDTagged: its instance is known to carry lux:pool-id.
	PoolIDTagged bool
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
		// Cost hours a cost pass wrote under an old name from a host row it
		// read before the rename (poolrename.go).
		if pl.RenamedAt != nil && time.Since(*pl.RenamedAt) < costRepairAfterRename {
			for _, old := range pl.PreviousNames {
				if err := renameCostHours(ctx, tx, ownerOf(pl), old, pl.Name, costRepairFrom(*pl.RenamedAt)); err != nil {
					return err
				}
			}
		}
		return s.poolState(ctx, tx, pl, &st)
	})
	if err != nil {
		return err
	}
	for _, id := range st.abandoned {
		if err := s.writeOff(ctx, lease, id, "launch never completed"); err != nil {
			return err
		}
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
		s.terminateRequested(ctx, h.ID, h.Reason)
		err := s.fencedCall(ctx, lease, nil, func(ctx context.Context) error {
			return prov.Terminate(ctx, h.Template, h.ProviderID)
		})
		if errors.Is(err, errFenced) {
			return err
		}
		if err != nil {
			s.log.Warn("terminate", "host", h.ID, "err", err)
			s.providerError(ctx, hostEvents, h.ID, "terminate", h.ProviderID, err)
			continue
		}
		if err := s.writeOff(ctx, lease, h.ID, h.Reason); err != nil {
			return err
		}
	}

	// Scale up: enough for what waits plus the warm hosts, at least the
	// minimum, never past the maximum. A retired pool only winds down.
	if !pl.Retired {
		warm := s.warm(pl, &st)
		want := max(pl.Min-st.total, warm+st.demand-st.idle-st.provisioning)
		if pl.Max > 0 {
			want = min(want, pl.Max-st.total)
		}
		up := scaleUp(pl, &st, warm, want)
		for i := range max(want, 0) {
			if i > 0 {
				if err := s.renewLease(ctx, lease); err != nil {
					return err
				}
			}
			lctx, cancel := lease.bound(ctx)
			err := s.launch(lctx, prov, pl, up)
			cancel()
			if errors.Is(err, errPoolRetired) {
				break
			}
			if err != nil {
				return err
			}
			up = nil // recorded with the first launch
		}
	}

	// Last, so a slow or refused CreateTags delays nothing else.
	return s.applyTags(ctx, prov, pl, st.tags, lease)
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
	return map[string]any{"hosts": want, "reason": reason, "waiting": st.demand, "warm": warm, "min": pl.Min, "max": pl.Max,
		"total": st.total, "idle": st.idle, "provisioning": st.provisioning}
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

func (s *Server) reconcileWithProvider(ctx context.Context, prov Provider, pl poolRow, st *poolState, lease *passLease) error {
	// Instances are listed with the template each was launched with (its
	// region): the pool's current one, and any older ones its hosts carry.
	templates := map[string]json.RawMessage{string(pl.Template): pl.Template}
	rows := map[string]hostRef{} // provider id → live row
	for _, h := range st.existing {
		templates[string(h.Template)] = h.Template
		rows[h.ProviderID] = h
	}
	st.tags = pendingTags{templates: templates, poolID: map[string][]string{}, name: map[string][]string{}}
	// By id, regardless of its name tag. A pool not yet migrated is also
	// listed by name to find instances launched before the id tag existed.
	tagSets := []map[string]string{s.poolIDTags(pl)}
	if pl.Legacy {
		tagSets = append(tagSets, s.poolNameTags(pl))
	}
	name := poolTagValue(pl.TenantID, pl.Name)
	listed := map[string]bool{}
	nameListed, nameOnly := false, false
	var confirmed []string // host ids whose instance showed lux:pool-id
	type orphan struct {
		pid  string
		tmpl json.RawMessage
		inst Instance
	}
	var orphans []orphan
	var gone []hostRef
	for key, tmpl := range templates {
		for _, tags := range tagSets {
			lctx, cancel := lease.bound(ctx)
			insts, err := prov.Instances(lctx, tmpl, tags)
			cancel()
			if err != nil {
				s.log.Warn("provider check", "pool", pl.Name, "err", err)
				s.providerError(ctx, poolEvents, pl.ID, "list", "", err)
				return nil
			}
			if tags[tagPool] != "" {
				nameListed = true
			}
			for pid, inst := range insts {
				if listed[pid] {
					continue // two templates in one region, or both tags, list the same instances
				}
				// Listed by name, but another pool's by id: not this pool's
				// to claim or terminate.
				if id := inst.Tags[tagPoolID]; id != "" && id != pl.ID {
					continue
				}
				listed[pid] = true
				isGone := instanceGone(inst)
				if !isGone && tags[tagPool] != "" && inst.Tags[tagPoolID] == "" {
					nameOnly = true
				}
				h, known := rows[pid]
				if known && !isGone {
					confirmed = append(confirmed, st.tags.follow(key, pid, h, inst, pl.ID, name)...)
				}
				switch {
				case known && isGone:
					gone = append(gone, h)
				case !known && !isGone && st.launching[inst.Tags[tagHost]]:
					// A launch whose instance id is not recorded yet (in flight,
					// or luxd stopped mid-launch): its row claims it.
					s.recordProviderID(ctx, pl.ID, inst.Tags[tagHost], pid)
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

	// Terminated instances drop out of the provider's listings after a while
	// (EC2 purges them): a settled host that is not listed may be gone. A
	// tag listing alone never says so (it lags launches and tag changes):
	// each host missing from it is looked up by id (checkUnlisted).
	unlisted := map[string][]hostRef{} // by template
	var seen []string
	for pid, h := range rows {
		if listed[pid] && h.NotFound {
			seen = append(seen, h.ID)
		}
		if listed[pid] || !h.Listable {
			continue
		}
		unlisted[string(h.Template)] = append(unlisted[string(h.Template)], h)
	}
	if err := s.recordNotFound(ctx, lease, seen, nil); err != nil {
		return err
	}
	for key, hosts := range unlisted {
		ok, err := s.checkUnlisted(ctx, prov, pl, st, lease, key, hosts)
		if err != nil {
			return err
		}
		confirmed = append(confirmed, ok...)
	}
	for _, o := range orphans {
		if o.inst.Tags[tagPoolID] == "" {
			st.tags.poolID[string(o.tmpl)] = append(st.tags.poolID[string(o.tmpl)], o.pid)
		}
	}
	// Retag all name-only instances before orphan termination. A refused
	// tag leaves the pool legacy, so an untagged orphan stays discoverable.
	if ok, err := s.retagPoolIDs(ctx, prov, pl, st.tags, lease); err != nil {
		return err
	} else if !ok {
		return nil
	}
	clear(st.tags.poolID)
	for _, o := range orphans {
		if err := s.terminateOrphan(ctx, prov, pl, lease, o.tmpl, o.pid, o.inst); err != nil {
			return err
		}
	}
	if err := s.confirmPoolID(ctx, lease, confirmed); err != nil {
		return err
	}
	return s.recordPoolDiscovery(ctx, lease, pl, nameListed && !nameOnly, len(listed) == 0)
}

// retagPoolIDs makes orphan termination safe: a failed termination is
// rediscovered by the id tag even after name-based discovery ends.
func (s *Server) retagPoolIDs(ctx context.Context, prov Provider, pl poolRow, tags pendingTags, lease *passLease) (bool, error) {
	for key, ids := range tags.poolID {
		for batch := range slices.Chunk(ids, retagBatch) {
			err := s.fencedCall(ctx, lease, nil, func(ctx context.Context) error {
				return prov.Retag(ctx, tags.templates[key], batch, tagPoolID, pl.ID)
			})
			if errors.Is(err, errFenced) {
				return false, err
			}
			if err != nil {
				s.log.Warn("provider check: tagging instances failed; retried on the next check", "pool", pl.Name, "tag", tagPoolID, "err", err)
				return false, nil
			}
		}
	}
	return true, nil
}

// recordPoolDiscovery is the one-way transition from legacy discovery.
// A clean name listing and host evidence from the same provider check
// are required; a written-off launch without an id keeps the window open.
func (s *Server) recordPoolDiscovery(ctx context.Context, lease *passLease, pl poolRow, cleanName, empty bool) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := s.fenceTx(ctx, tx, lease); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE pools p SET
			last_empty_listing_at = CASE WHEN $4 THEN now() ELSE NULL END,
			id_migrated_at = CASE WHEN id_migrated_at IS NULL AND $3
				AND NOT EXISTS (SELECT 1 FROM hosts h WHERE `+legacyHost+`)
				AND NOT EXISTS (SELECT 1 FROM hosts h WHERE h.pool = p.name
					AND coalesce(h.tenant_id, '') = coalesce(p.tenant_id, '')
					AND h.provision_requested_at IS NOT NULL AND h.provider_id IS NULL
					AND h.terminated_at >= now() - $2::interval)
				THEN now() ELSE id_migrated_at END
			WHERE p.id = $1`, pl.ID, interval(s.cfg.LaunchTimeout+poolDiscoveryLag), cleanName, empty)
		return err
	})
}

// pendingTags: tags a provider check found missing or stale on the
// pool's live instances, by template key, applied at the end of the pass
// (applyTags).
type pendingTags struct {
	templates map[string]json.RawMessage
	// poolID: instances without lux:pool-id (launched before it).
	poolID map[string][]string
	// name: instances whose lux:pool is not the pool's name (a rename).
	name map[string][]string
}

// follow notes what a live instance of host h lacks, and returns h's id
// if it is to be recorded as carrying lux:pool-id.
func (t *pendingTags) follow(key, pid string, h hostRef, inst Instance, poolID, name string) []string {
	if inst.Tags[tagPoolID] == poolID {
		if inst.Tags[tagPool] != name && inst.Tags[tagHost] == h.ID {
			t.name[key] = append(t.name[key], pid)
		}
		if !h.PoolIDTagged {
			return []string{h.ID}
		}
		return nil
	}
	// IAM allows tagging only instances whose lux:host names a host.
	if inst.Tags[tagHost] == h.ID {
		t.poolID[key] = append(t.poolID[key], pid)
	}
	return nil
}

func instanceGone(inst Instance) bool {
	return inst.State == "terminated" || inst.State == "shutting-down"
}

// checkUnlisted looks up by id settled hosts (of one template) that no
// listing showed, writes off the ones the provider says are gone, and
// keeps the others, noting the tags they lack. An id the provider does
// not know is not proof: EC2 answers NotFound for a while after a launch.
// It writes a host off only when found unknown on two checks listing_lag
// apart, on a host older than a launch can take to show
// (hostRef.NotFoundLong). It returns the hosts whose instance carries
// lux:pool-id.
func (s *Server) checkUnlisted(ctx context.Context, prov Provider, pl poolRow, st *poolState, lease *passLease, key string, hosts []hostRef) ([]string, error) {
	pids := make([]string, len(hosts))
	for i, h := range hosts {
		pids[i] = h.ProviderID
	}
	dctx, cancel := lease.bound(ctx)
	insts, err := prov.Describe(dctx, hosts[0].Template, pids)
	cancel()
	if err != nil {
		s.log.Warn("provider check: describing unlisted hosts; nothing done this pass", "pool", pl.Name, "err", err)
		return nil, nil
	}
	var seen, unknown []string
	for _, h := range hosts {
		if _, known := insts[h.ProviderID]; known && h.NotFound {
			seen = append(seen, h.ID)
		} else if !known && !h.NotFound {
			unknown = append(unknown, h.ID)
		}
	}
	if err := s.recordNotFound(ctx, lease, seen, unknown); err != nil {
		return nil, err
	}
	var confirmed []string
	for _, h := range hosts {
		inst, known := insts[h.ProviderID]
		if !known {
			if h.Settled && h.NotFoundLong {
				if err := s.writeOff(ctx, lease, h.ID, "the provider no longer knows this host"); err != nil {
					return nil, err
				}
				if !h.Draining {
					st.total--
				}
			} else {
				s.log.Warn("provider check: the provider does not know a host's instance; kept until it says so again later",
					"pool", pl.Name, "host", h.ID, "providerId", h.ProviderID)
			}
			continue
		}
		if instanceGone(inst) {
			if h.Settled {
				if err := s.writeOff(ctx, lease, h.ID, "the provider terminated this host"); err != nil {
					return nil, err
				}
				if !h.Draining {
					st.total--
				}
			}
			continue // otherwise its runner still heartbeats: the reaper decides
		}
		s.log.Warn("provider check: a host missing from its pool's listing is still up; kept",
			"pool", pl.Name, "host", h.ID, "providerId", h.ProviderID, "state", inst.State,
			"lux:pool-id", inst.Tags[tagPoolID], "lux:pool", inst.Tags[tagPool])
		confirmed = append(confirmed, st.tags.follow(key, h.ProviderID, h, inst, pl.ID, poolTagValue(pl.TenantID, pl.Name))...)
	}
	return confirmed, nil
}

// confirmPoolID records hosts whose instance a provider check showed
// carrying lux:pool-id: once every live host of a pool is, the pool is no
// longer listed by name.
func (s *Server) confirmPoolID(ctx context.Context, lease *passLease, hostIDs []string) error {
	if len(hostIDs) == 0 {
		return nil
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := s.fenceTx(ctx, tx, lease); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE hosts SET pool_id_tagged = true WHERE id = ANY($1)`, hostIDs)
		return err
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("provider check: recording hosts tagged lux:pool-id", "err", err)
	}
	return nil
}

// retagBatch: EC2's CreateTags takes at most 1000 resources per call.
const retagBatch = 500

// applyTags sets lux:pool to the pool's current name where it differs.
// An IAM refusal is logged and retried by the next provider check; the
// name tag is not used for discovery after migration.
func (s *Server) applyTags(ctx context.Context, prov Provider, pl poolRow, t pendingTags, lease *passLease) error {
	apply := func(key string, pids []string, tag string, value func(tx pgx.Tx) (string, error)) error {
		n := 0
		var failed error
		for batch := range slices.Chunk(pids, retagBatch) {
			var v string
			err := s.fencedCall(ctx, lease, func(tx pgx.Tx) error {
				var err error
				v, err = value(tx)
				return err
			}, func(ctx context.Context) error {
				return prov.Retag(ctx, t.templates[key], batch, tag, v)
			})
			if errors.Is(err, errFenced) {
				return err
			}
			if err != nil {
				failed = err
				continue
			}
			n += len(batch)
		}
		if failed != nil {
			s.log.Warn("provider check: tagging instances failed; retried on the next check",
				"pool", pl.Name, "tag", tag, "tagged", n, "of", len(pids), "err", failed)
		} else {
			s.log.Info("provider check: instances tagged", "pool", pl.Name, "tag", tag, "instances", n)
		}
		return nil
	}
	for key, pids := range t.name {
		// The name as of the call: a rename since the pass began is what
		// the instances should carry.
		err := apply(key, pids, tagPool, func(tx pgx.Tx) (string, error) {
			var name string
			err := tx.QueryRow(ctx, `SELECT name FROM pools WHERE id = $1`, pl.ID).Scan(&name)
			return poolTagValue(pl.TenantID, name), err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// recordNotFound clears not_found_since on hosts whose instance the
// provider showed (seen), and sets it on those it did not know (unknown).
func (s *Server) recordNotFound(ctx context.Context, lease *passLease, seen, unknown []string) error {
	if len(seen) == 0 && len(unknown) == 0 {
		return nil
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := s.fenceTx(ctx, tx, lease); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET not_found_since = NULL WHERE id = ANY($1)`, seen); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE hosts SET not_found_since = coalesce(not_found_since, now()) WHERE id = ANY($1)`, unknown)
		return err
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("provider check: recording hosts the provider does not know", "err", err)
	}
	return nil
}

// terminateOrphan ends an instance of the pool that no live host row read
// by this pass claims (a launch whose reply was lost, or a host written
// off) — unless, checked again at the last moment, a live host row of the
// pool's owner does claim it: a launch may have committed after the pass
// read its rows. A claimed instance's tags are the next check's to follow.
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
		s.log.Warn("terminating an orphaned instance", "pool", pl.Name, "providerId", pid, "lux:pool", inst.Tags[tagPool])
		return prov.Terminate(ctx, tmpl, pid)
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("terminate orphan", "providerId", pid, "err", err)
		return nil
	}
	if claimed {
		s.log.Warn("provider check: an instance no host row read by this pass claimed is its host's; kept",
			"pool", pl.Name, "host", hostID, "providerId", pid, "hostPool", pool)
	}
	return nil
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

// poolIDTags are what a pool's instances are listed by: every instance
// launched since lux:pool-id carries them, whatever the pool is called.
func (s *Server) poolIDTags(pl poolRow) map[string]string {
	return map[string]string{tagManaged: "true", tagDeployment: s.deployment, tagPoolID: pl.ID}
}

// poolNameTags list a legacy pool's instances launched before lux:pool-id,
// by the name they were launched under: a pool with such instances is
// never renamed (poolrename.go). Tenant pools are named by tenant and name.
func (s *Server) poolNameTags(pl poolRow) map[string]string {
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
	// Runs waiting for a host in this pool. A Run that resolved to a pool
	// row (pool_owner set) counts toward exactly that one. Otherwise a
	// tenant pool serves its tenant; a platform pool anyone whose Runs name
	// it (and who has no pool of their own by that name).
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs r
		WHERE r.state = 'provisioning' AND NOT r.cancel_requested
		  AND coalesce(r.spec->'placement'->>'pool', 'default') = $1
		  AND CASE WHEN r.pool_owner IS NOT NULL THEN r.pool_owner = coalesce($2, '')
		           WHEN $2::text IS NULL
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
			coalesce(h.last_heartbeat < now() - $7::interval, true),
			h.not_found_since IS NOT NULL, h.pool_id_tagged,
			coalesce(h.not_found_since <= now() - $8::interval AND h.created_at < now() - $4::interval - $8::interval, false)
		FROM hosts h
		WHERE h.pool = $1 AND coalesce(h.tenant_id, '') = coalesce($2, '') AND h.provision_requested_at IS NOT NULL
		  AND h.state <> 'terminated'
		ORDER BY coalesce(h.last_placement_ended_at, h.registered_at, h.created_at)`,
		pl.Name, pl.TenantID, interval(s.scaleDownAfter(pl)), interval(s.cfg.LaunchTimeout), pl.Template, interval(s.cfg.LostGrace), interval(s.cfg.LeaseDuration), interval(s.cfg.ListingLag))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h hostRef
		var state string
		var draining, busy, pending, idleLong, launchLong, lostLong, silent bool
		if err := rows.Scan(&h.ID, &h.ProviderID, &h.Template, &state, &draining, &busy, &pending, &idleLong, &launchLong, &lostLong, &h.Listable, &silent, &h.NotFound, &h.PoolIDTagged, &h.NotFoundLong); err != nil {
			return err
		}
		h.Settled = h.Listable && silent
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
	return nil
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
	renamed := false
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// The pool may have been removed or renamed since the pass read it.
		// FOR SHARE waits for a removal or rename in flight, and holds one
		// off until this host row commits: a removal's drain then finds
		// it, a rename moves it. Not under a name the pool has just left:
		// the row would join no pool.
		var current string
		var retired bool
		err := tx.QueryRow(ctx, `SELECT retired, name FROM pools WHERE id = $1 FOR SHARE`, pl.ID).Scan(&retired, &current)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && retired {
			return errPoolRetired
		}
		if err != nil {
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
		_, err = tx.Exec(ctx, `INSERT INTO hosts (id, tenant_id, pool, token_id, name, state, provision_requested_at, launch_template, tagged, pool_id_tagged)
			VALUES ($1, $2, $3, $4, $5, 'provisioning', now(), $6, true, true)`,
			hostID, pl.TenantID, pl.Name, tokenID, name, pl.Template)
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
	if err != nil || renamed {
		return err
	}
	env := map[string]string{"LUX_URL": s.cfg.RunnerURL, "LUX_HOST_TOKEN": token, "LUX_HOST_NAME": name}
	tags := s.poolIDTags(pl)
	tags[tagPool], tags["Name"], tags[tagHost] = poolTagValue(pl.TenantID, pl.Name), name, hostID
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
// Taking the lease draws a new token unless this luxd holds it unexpired:
// after an expiry, even its own earlier pass may have been overtaken.
// Expiry is compared with clock_timestamp(), not now(): the statement may
// have waited for the row's lock since the transaction began (ON CONFLICT
// judges its WHERE once it holds the lock).
func (s *Server) provisionLease(ctx context.Context) (*passLease, error) {
	start := time.Now()
	var l passLease
	var left float64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO leases (name, holder, expires_at, token) VALUES ('provisioner', $1, clock_timestamp() + $2::interval, nextval('lease_tokens'))
			ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = clock_timestamp() + $2::interval,
				token = CASE WHEN leases.holder = EXCLUDED.holder AND leases.expires_at > clock_timestamp() THEN leases.token ELSE EXCLUDED.token END
				WHERE leases.holder = EXCLUDED.holder OR leases.expires_at < clock_timestamp()
			RETURNING token, extract(epoch FROM expires_at - clock_timestamp())::float8`, s.id, interval(s.provisionLeaseDuration())).Scan(&l.token, &left)
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
// took it, it expired, or it was taken again (a new token).
func (s *Server) renewLease(ctx context.Context, l *passLease) error {
	var until time.Time
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		until, err = s.fenceTx(ctx, tx, l)
		return err
	})
	if err != nil {
		return err
	}
	l.expires = until
	return nil
}

// fenceTx checks, in tx, that the pass still holds the lease it began
// with (same holder, same token, not expired) and renews it. The lease row
// stays locked until tx ends, so no other luxd takes the lease between
// this check and what tx decides. It returns when the lease expires, by
// this process's clock, derived from the database's clock after the lock.
func (s *Server) fenceTx(ctx context.Context, tx pgx.Tx, l *passLease) (time.Time, error) {
	// The lock first, in its own statement: an UPDATE judges its WHERE
	// before waiting for a row lock, and again only if the row changed.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE name = 'provisioner' FOR UPDATE`); err != nil {
		return time.Time{}, err
	}
	start := time.Now()
	var left float64
	err := tx.QueryRow(ctx, `UPDATE leases SET expires_at = clock_timestamp() + $3::interval
		WHERE name = 'provisioner' AND holder = $1 AND token = $2 AND expires_at > clock_timestamp()
		RETURNING extract(epoch FROM expires_at - clock_timestamp())::float8`, s.id, l.token, interval(s.provisionLeaseDuration())).Scan(&left)
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
				coalesce((SELECT p.retired FROM pools p WHERE p.name = h.pool AND p.tenant_id IS NOT DISTINCT FROM h.tenant_id), false) AS retired
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
