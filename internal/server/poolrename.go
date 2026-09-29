package server

// Renaming a pool (POST /v1/pools/{name}/rename).
//
// A pool's name is in the database (pools.name, hosts.pool,
// host_tokens.pool, cost_hourly.pool, the spec of every Run not yet final)
// and, for a provisioned pool, on its instances: the lux:pool tag. The
// provisioner does not find a pool's instances by that tag but by
// lux:pool-id, the pool's id, set at launch and never changed
// (provisioner.go). So a rename is one transaction that moves every row to
// the new name; the lux:pool tags are updated afterwards by the
// provisioner's checks, for people and cost tooling only, and a failure
// there changes nothing else.
//
// Two things make that hold:
//
//   - Instances launched before lux:pool-id existed carry only the name.
//     Their pool is "legacy" (poolRow.Legacy) until a provider check has
//     seen lux:pool-id on every one: meanwhile the provisioner also lists
//     it by name and adds the tag, and a rename is refused
//     (pool_not_migrated): renamed, those instances would be listed by
//     nothing.
//   - A luxd of an earlier version lists by name. Once any pool has been
//     renamed (pools.renamed_at), the database refuses it the provisioner
//     lease (migration 031's trigger), and a rename of a provisioned pool
//     locks the lease row and refuses to start while such a luxd holds it
//     or runs (rename_unsupported_by_deployment).
//
// A pool's old names are kept (pools.previous_names), only to tell a host
// token or a Run naming one what the pool is called now (pool_renamed);
// any pool may take one.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
	"github.com/marcioapm/lux/internal/version"
)

type renamePoolInput struct {
	TenantQuery
	Name   string `path:"name" doc:"The pool's current name."`
	DryRun bool   `query:"dryRun" doc:"Count what would follow the rename, and check the new name if one is given, without renaming."`
	Body   renamePoolRequest
}

type renamePoolRequest struct {
	Name    string `json:"name" doc:"The new name." example:"burst-eu"`
	Confirm string `json:"confirm,omitempty" doc:"The pool's current name. Required while the pool has hosts that are not terminated (409 confirm_required otherwise). It confirms the pool, not the counts a dry run showed: hosts and Runs that joined since follow the rename too."`
}

// PoolRenamed is what a rename moved or, with dryRun, would move.
type PoolRenamed struct {
	Pool      Pool `json:"pool"`
	Hosts     int  `json:"hosts" doc:"The pool's hosts that are not terminated; they follow the rename."`
	Runs      int  `json:"runs" doc:"Runs not yet final (neither succeeded, failed nor cancelled) that name the pool; their spec names the new one."`
	Instances int  `json:"instances" doc:"Of those hosts, the ones a provider launched: they keep running, and the provisioner updates their lux:pool name tag after the rename."`
	DryRun    bool `json:"dryRun,omitempty"`
}

type renamePoolOutput struct {
	Body PoolRenamed
}

// renamePool: a tenant's pool, or with an operator key naming no tenant, a
// platform pool.
func (s *Server) renamePool(ctx context.Context, in *renamePoolInput) (*renamePoolOutput, error) {
	out, err := s.rename(ctx, principal(ctx).TenantID, renameArgs{From: in.Name, To: in.Body.Name, Confirm: in.Body.Confirm, DryRun: in.DryRun})
	if err != nil {
		return nil, err
	}
	if !in.DryRun {
		s.Kick()
	}
	return &renamePoolOutput{Body: out}, nil
}

func poolNameKey(tenantID, name string) string { return "pool-name:" + tenantID + "/" + name }

// lockPoolName holds one owner's pool name until tx ends: a rename holds
// both its names, and whatever writes a row naming a pool holds that name.
func lockPoolName(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, poolNameKey(tenantID, name))
	return err
}

// lockPoolNameShared is lockPoolName for a writer that only names the
// pool (a Run's spec): writers wait for a rename, not for each other.
func lockPoolNameShared(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, poolNameKey(tenantID, name))
	return err
}

// lockPoolNameAnyOwner holds a pool name for every owner until tx ends:
// a platform pool's rename looks for tenants' pools by its new name, which
// no tenant may create meanwhile. Taken after lockPoolName.
func lockPoolNameAnyOwner(ctx context.Context, tx pgx.Tx, name string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pool-name-any:' || $1, 0))`, name)
	return err
}

// LockPoolName holds the tenant's ("": the platform's) pool name, for that
// owner and across owners, until the transaction ends: taken by whatever
// creates or updates a pool row.
func LockPoolName(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	if err := lockPoolName(ctx, tx, tenantID, name); err != nil {
		return err
	}
	return lockPoolNameAnyOwner(ctx, tx, name)
}

// CreateHostToken mints a host token for the owner's pool (tenantID nil:
// the platform's) in tx and returns it. It holds the pool name as a
// rename does, so a rename either moves the token or commits first.
// Minting never creates a pool row: a static pool may have none, its
// hosts and tokens naming it. A name a pool of the owner was renamed away
// from is refused (pool_renamed) while no pool row of the owner has it:
// such a token would start a pool of its own beside the renamed one.
func CreateHostToken(ctx context.Context, tx pgx.Tx, tenantID *string, pool string, labels map[string]string) (string, error) {
	owner := ""
	if tenantID != nil {
		owner = *tenantID
	}
	if err := lockPoolName(ctx, tx, owner, pool); err != nil {
		return "", err
	}
	if err := CheckPoolName(ctx, tx, tenantID, pool); err != nil {
		return "", err
	}
	var renamedTo string
	err := tx.QueryRow(ctx, `SELECT name FROM pools p WHERE coalesce(tenant_id, '') = $1 AND $2 = ANY (previous_names)
			AND NOT EXISTS (SELECT 1 FROM pools o WHERE coalesce(o.tenant_id, '') = $1 AND o.name = $2)
		ORDER BY renamed_at DESC NULLS LAST LIMIT 1`, owner, pool).Scan(&renamedTo)
	if err == nil {
		return "", errf(http.StatusConflict, "pool_renamed",
			"pool %q was renamed %q: mint the token for %q, or create a pool named %q first", pool, renamedTo, renamedTo, pool)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	token := ids.Secret("luxh")
	_, err = tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, labels, token_hash) VALUES ($1, $2, $3, $4, $5)`,
		ids.New(ids.HostToken), tenantID, pool, labels, ids.Hash(token))
	return token, err
}

// checkRunPool holds, for the rest of tx, the pool name a Run submitted
// by tenantID names, shared with other submissions and exclusive of a
// rename of the tenant's or the platform's pool by that name: the rename
// either moves the Run or commits first. In the second case, the name is
// refused (pool_renamed) if a pool the tenant's Runs may use was renamed
// away from it and nothing answers to it now.
func checkRunPool(ctx context.Context, tx pgx.Tx, tenantID, pool string) error {
	if err := lockPoolNameShared(ctx, tx, tenantID, pool); err != nil {
		return err
	}
	if err := lockPoolNameShared(ctx, tx, "", pool); err != nil {
		return err
	}
	var renamedTo *string
	if err := tx.QueryRow(ctx, `SELECT lux_pool_renamed_to($1)`, pool).Scan(&renamedTo); err != nil {
		return err
	}
	if renamedTo != nil {
		return errf(http.StatusConflict, "pool_renamed", "pool %q was renamed %q: submit the Run to %q", pool, *renamedTo, *renamedTo)
	}
	return nil
}

// renameArgs: rename From to To; Confirm is From, typed, when the pool has
// live hosts.
type renameArgs struct {
	From, To, Confirm string
	DryRun            bool
}

// capPoolIDDiscovery is the capability a luxd advertises (luxd_instances)
// when its provisioner finds instances by lux:pool-id, whatever their
// lux:pool says. Migration 031's lease fence names it too.
const capPoolIDDiscovery = "pool-id-discovery"

// rename is the whole rename, in one transaction; tenantID "" names a
// platform pool.
func (s *Server) rename(ctx context.Context, tenantID string, a renameArgs) (PoolRenamed, error) {
	from, to, dryRun := a.From, a.To, a.DryRun
	var out PoolRenamed
	// A dry run without a name only counts (the console's confirmation).
	// A new name is never grandfathered: it becomes a tag value and a
	// hostname, and the rule keeps "<tenant>/<name>" tag values unambiguous.
	if !dryRun || to != "" {
		if err := ValidPoolName(to); err != nil {
			return out, errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
		}
		if to == from {
			return out, errf(http.StatusUnprocessableEntity, "invalid_pool", "the pool is already named %q", to)
		}
	}
	var err error
	// Runs are locked before hosts, as drainHosts does; a deadlock with a
	// path taking them the other way round is retried.
	for range 3 {
		out, err = s.renamePoolTx(ctx, tenantID, a)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "40P01" {
			break
		}
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return out, errf(http.StatusConflict, "pool_exists", "a pool named %q exists", to)
	}
	if err != nil || dryRun {
		return out, err
	}
	// TODO(pool-events): write a "renamed" pool event (from, to, hosts,
	// runs, instances) in renamePoolTx's transaction once the pool events
	// table exists.
	s.log.Info("pool renamed", "tenant", tenantID, "from", from, "to", to,
		"hosts", out.Hosts, "runs", out.Runs, "instances", out.Instances)
	return out, nil
}

func (s *Server) renamePoolTx(ctx context.Context, tenantID string, a renameArgs) (PoolRenamed, error) {
	from, to, dryRun := a.From, a.To, a.DryRun
	out := PoolRenamed{DryRun: dryRun}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Both names, always in the same order.
		if err := lockPoolName(ctx, tx, tenantID, min(from, to)); err != nil {
			return err
		}
		if err := lockPoolName(ctx, tx, tenantID, max(from, to)); err != nil {
			return err
		}
		checkName := to != ""
		if checkName {
			if err := lockPoolNameAnyOwner(ctx, tx, to); err != nil {
				return err
			}
			if err := lockProvisionerLease(ctx, tx); err != nil {
				return err
			}
		}
		var id, provider string
		err := tx.QueryRow(ctx, `SELECT id, provider FROM pools WHERE coalesce(tenant_id, '') = $1 AND name = $2 AND NOT retired FOR UPDATE`,
			tenantID, from).Scan(&id, &provider)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		provisioned := provider != "static"
		if provisioned {
			// Checked on a dry run too: it does not depend on the name.
			if err := checkPoolMigrated(ctx, tx, id, from); err != nil {
				return err
			}
		}
		if checkName {
			if err := checkRenameTarget(ctx, tx, tenantID, to); err != nil {
				return err
			}
		}
		if checkName && provisioned {
			if err := s.checkDeploymentCanRename(ctx, tx); err != nil {
				return err
			}
		}

		// A platform pool's Runs are those of every tenant that names it
		// and has no live pool of its own by that name (as poolState's
		// demand counts them).
		runs := `FROM runs r WHERE r.state NOT IN ('succeeded', 'failed', 'cancelled')
			AND coalesce(r.spec->'placement'->>'pool', 'default') = $2
			AND CASE WHEN $1 = '' THEN NOT EXISTS (SELECT 1 FROM pools o WHERE o.tenant_id = r.tenant_id AND o.name = $2 AND NOT o.retired)
			         ELSE r.tenant_id = $1 END`
		if err := tx.QueryRow(ctx, `SELECT count(*) `+runs, tenantID, from).Scan(&out.Runs); err != nil {
			return err
		}
		if checkName && tenantID == "" {
			// A tenant whose Runs follow the platform pool, and who owns a
			// pool by the new name, would find them resolved to its own.
			// TODO(pool-owner): select these Runs by runs.pool_owner once it
			// exists, rather than by "no pool of their own by that name".
			var tenant string
			err := tx.QueryRow(ctx, `SELECT coalesce(t.name, o.tenant_id) FROM pools o LEFT JOIN tenants t ON t.id = o.tenant_id
				WHERE o.name = $3 AND NOT o.retired AND o.tenant_id IN (SELECT r.tenant_id `+runs+`)
				ORDER BY 1 LIMIT 1`, tenantID, from, to).Scan(&tenant)
			if err == nil {
				return errf(http.StatusConflict, "pool_exists",
					"tenant %q has Runs waiting for platform pool %q and its own pool named %q: renamed, those Runs would go to its pool", tenant, from, to)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE provision_requested_at IS NOT NULL)
			FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND state <> 'terminated'`,
			tenantID, from).Scan(&out.Hosts, &out.Instances); err != nil {
			return err
		}
		if dryRun {
			out.Pool, err = poolByID(ctx, tx, id)
			return err
		}
		if out.Hosts > 0 && a.Confirm != from {
			return errf(http.StatusConflict, "confirm_required",
				"pool %q has %d hosts that follow the rename: confirm with its current name", from, out.Hosts)
		}

		if _, err := tx.Exec(ctx, `UPDATE runs SET spec = jsonb_set(spec, '{placement,pool}', to_jsonb($3::text)), updated_at = now()
			WHERE id IN (SELECT r.id `+runs+` ORDER BY r.id FOR UPDATE)`, tenantID, from, to); err != nil {
			return err
		}
		// Terminated hosts too: cost pricing joins a host to its pool by
		// name, also after it is gone, and a pool later given the old name
		// must not claim them.
		if _, err := tx.Exec(ctx, `UPDATE hosts SET pool = $3 WHERE coalesce(tenant_id, '') = $1 AND pool = $2`, tenantID, from, to); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_tokens SET pool = $3 WHERE coalesce(tenant_id, '') = $1 AND pool = $2`, tenantID, from, to); err != nil {
			return err
		}
		// cost_hourly.pool is copied from hosts.pool whenever an hour is
		// (re)computed: left alone, one host's hours would split between
		// both names as they are recomputed.
		if err := renameCostHours(ctx, tx, tenantID, from, to); err != nil {
			return err
		}
		// renamed_at only for a provisioned pool: it is what fences older
		// luxd out of the provisioner lease, and a static pool's rename
		// changes nothing they list.
		if _, err := tx.Exec(ctx, `UPDATE pools SET name = $2,
				previous_names = array_append(array_remove(previous_names, $2), $3),
				renamed_at = CASE WHEN provider <> 'static' THEN now() ELSE renamed_at END
			WHERE id = $1`, id, to, from); err != nil {
			return err
		}
		out.Pool, err = poolByID(ctx, tx, id)
		return err
	})
	return out, err
}

// checkPoolMigrated refuses a rename while some live host of the pool may
// run an instance without lux:pool-id: the provisioner finds those by
// name only, and would lose them.
func checkPoolMigrated(ctx context.Context, tx pgx.Tx, poolID, name string) error {
	var unconfirmed int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM hosts h JOIN pools p ON p.id = $1
		WHERE `+legacyHost, poolID).Scan(&unconfirmed); err != nil {
		return err
	}
	if unconfirmed > 0 {
		return errf(http.StatusConflict, "pool_not_migrated",
			"pool %q has %d hosts whose instances were launched before lux:pool-id and are not yet confirmed to carry it: "+
				"wait for the next provider checks to tag them (luxd needs ec2:CreateTags of lux:pool-id), or scale the pool to zero", name, unconfirmed)
	}
	return nil
}

// legacyHost, a condition on hosts h of pool p: a live provisioned host
// whose instance is not confirmed to carry lux:pool-id. Hosts launched
// before the deployment tags (hosts.tagged) are never listed, by name or
// id, so they do not count.
const legacyHost = `h.pool = p.name AND coalesce(h.tenant_id, '') = coalesce(p.tenant_id, '')
	AND h.provision_requested_at IS NOT NULL AND h.tagged AND NOT h.pool_id_tagged AND h.state <> 'terminated'`

// renameCostHours moves the pool's hosts' cost hours still under the old
// name. Idempotent: the provisioner runs it again for a while after a
// rename, for hours computed from a host row read before the rename and
// written after it (cost writers copy hosts.pool without the rename's
// locks).
func renameCostHours(ctx context.Context, tx pgx.Tx, tenantID, from, to string) error {
	_, err := tx.Exec(ctx, `UPDATE cost_hourly SET pool = $3 WHERE pool = $2
		AND host_id IN (SELECT id FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $3)`, tenantID, from, to)
	return err
}

// costRepairAfterRename: how long after a rename the provisioner still
// moves cost hours written under an old name. A cost pass's transaction
// is far shorter.
const costRepairAfterRename = time.Hour

// checkDeploymentCanRename refuses a provisioned pool's rename while a luxd
// that discovers instances by name may hold the provisioner lease: after
// the rename it would take the pool's instances, still tagged with the old
// name, for another pool's, or miss them. Every luxd writes control
// samples; the ones that discover by lux:pool-id also check in to
// luxd_instances. The lease row must be locked (lockProvisionerLease): the
// lease fence (migration 031) is what keeps an older luxd from taking the
// lease once renamed_at is set.
func (s *Server) checkDeploymentCanRename(ctx context.Context, tx pgx.Tx) error {
	// The lease's holder too: a luxd that took it before its first
	// control sample.
	rows, err := tx.Query(ctx, `SELECT instance FROM (
			SELECT c.instance FROM control_samples c WHERE c.res = 0 AND c.at > now() - $1::interval
			UNION SELECT holder FROM leases WHERE name = 'provisioner' AND expires_at > clock_timestamp()) seen
		WHERE NOT EXISTS (SELECT 1 FROM luxd_instances l WHERE l.instance = seen.instance
		                  AND $2 = ANY(l.capabilities) AND l.seen_at > now() - $1::interval - $3::interval)
		ORDER BY 1 LIMIT 10`, interval(2*s.provisionLeaseDuration()), capPoolIDDiscovery, interval(s.checkInEvery()))
	if err != nil {
		return err
	}
	old, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(old) > 0 {
		return errf(http.StatusConflict, "rename_unsupported_by_deployment",
			"luxd instances %v run a version that finds a pool's instances by its name (it would lose them after a rename): upgrade every luxd first", old)
	}
	return nil
}

// lockProvisionerLease locks the provisioner lease's row until tx ends,
// creating it (expired, held by no one) if there is none: a luxd taking
// the lease meanwhile waits, and its fence then sees what tx committed.
// Taken before the pool row, in the provisioner's order.
func lockProvisionerLease(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', '', '-infinity') ON CONFLICT (name) DO NOTHING`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE name = 'provisioner' FOR UPDATE`)
	return err
}

// checkIn records this luxd in luxd_instances: its version and what it
// can do. Each process start is a new instance: rows long unseen go.
func (s *Server) checkIn(ctx context.Context) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO luxd_instances (instance, version, capabilities, seen_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (instance) DO UPDATE SET version = EXCLUDED.version, capabilities = EXCLUDED.capabilities, seen_at = now()`,
			s.id, version.Version, []string{capPoolIDDiscovery})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM luxd_instances WHERE seen_at < now() - interval '7 days'`)
		return err
	})
}

// checkInEvery: a luxd's last check-in is at most this much older than
// its last control sample, which checkDeploymentCanRename allows for.
func (s *Server) checkInEvery() time.Duration {
	return min(s.cfg.ProviderCheckEvery, s.provisionLeaseDuration())
}

// checkInLoop checks in at start, then every checkInEvery. checkedIn is
// closed after the first check-in: the provisioner waits for it, as the
// lease fence (migration 031) refuses a holder that has not checked in
// once a pool has been renamed.
func (s *Server) checkInLoop(ctx context.Context) {
	every := s.checkInEvery()
	first := true
	for {
		err := s.checkIn(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("luxd check-in", "err", err)
		}
		if err == nil && first {
			close(s.checkedIn)
			first = false
		}
		// Retried sooner until the first one lands.
		next := every
		if first {
			next = min(every, 5*time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// checkRenameTarget: hosts and host tokens can name a pool no pool row has
// (a static host's token): renaming onto that name would merge them in.
func checkRenameTarget(ctx context.Context, tx pgx.Tx, tenantID, to string) error {
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pools WHERE coalesce(tenant_id, '') = $1 AND name = $2)
			OR EXISTS (SELECT 1 FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND state <> 'terminated')
			OR EXISTS (SELECT 1 FROM host_tokens WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND revoked_at IS NULL)`,
		tenantID, to).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return errf(http.StatusConflict, "pool_exists", "the name %q is taken: a pool (live or retired), or hosts or host tokens, have it", to)
	}
	return nil
}

func poolByID(ctx context.Context, tx pgx.Tx, id string) (Pool, error) {
	var pl Pool
	var sda int
	err := tx.QueryRow(ctx, `SELECT `+poolColumns+` FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id WHERE p.id = $1`, id).Scan(
		&pl.Name, &pl.Tenant, &pl.Provider, &pl.Template, &pl.MinHosts, &pl.MaxHosts, &pl.WarmHosts,
		&sda, &pl.WarmWhileActive, &pl.Shared, &pl.Platform, &pl.HourlyPrice, &pl.Currency)
	pl.ScaleDownAfter.Duration = time.Duration(sda) * time.Second
	return pl, err
}

// poolTagValue is a pool name as its instances' lux:pool tag has it.
func poolTagValue(tenantID *string, name string) string {
	if tenantID != nil {
		return *tenantID + "/" + name
	}
	return name
}

// ownerOf is the pool's tenant id, "" for a platform pool.
func ownerOf(pl poolRow) string {
	if pl.TenantID == nil {
		return ""
	}
	return *pl.TenantID
}
