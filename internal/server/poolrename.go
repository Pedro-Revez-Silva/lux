package server

// Renaming a pool (POST /v1/pools/{name}/rename).
//
// A pool's name is in the database (pools.name, hosts.pool,
// host_tokens.pool, cost_hourly.pool, the spec of every Run not yet final)
// and, for a provisioned pool, on its instances: the lux:pool tag. The
// provisioner lists a pool's instances by that tag. The rule for the whole
// feature: luxd never terminates a running instance, or writes off its
// host, because of a rename. It is two steps:
//
//  1. One transaction moves every row to the new name and keeps the old
//     one in pools.renamed_from. From its commit the provisioner lists the
//     pool under both names, so every instance, re-tagged or not, is
//     listed and claimed by its (renamed) host row; the old name stays
//     reserved, so no other pool can take it and list these instances as
//     its orphans. reconcilePool and launch read the pool row FOR SHARE,
//     so a pass never pairs the old name with the hosts' new one.
//  2. On each provider check the provisioner re-tags the live instances
//     still listed under the old name (Provider.Retag), and records when in
//     pools.retagged_at. The rename is finished (renamed_from cleared,
//     rename_finished_at set) by a pass that lists nothing under the old
//     name, lists every live host's instance under the new one, has no
//     launch in flight, and comes listing_lag after the last re-tag.
//
// A tag listing is never enough to destroy anything (provisioner.go): it
// may come from a pass that read the old name, lag a re-tag either way, or
// predate a CreateTags that lands late. So a settled host missing from the
// listings is looked up by id (Provider.Describe), and kept, re-tagged,
// while the provider says it runs; an instance no host row claims is
// checked against the host rows again, under any pool name, right before
// it is terminated; and every Terminate, Retag and write-off first checks,
// in the same transaction, that the pass still holds the provisioner lease
// it began with (its fencing token), with provider calls cancelled at the
// lease's expiry.
//
// Around that: a pool that finished a rename is not renamed again for the
// lease plus listing_lag (a late re-tag of the previous rename may still
// land); a provisioned pool is not renamed while a luxd that cannot follow
// a rename runs (luxd_instances); its provider cannot change mid-rename.
//
// Nothing is tagged before step 1 commits, so no instance carries a name
// no pool row answers to. A luxd that stops before the commit leaves
// nothing done; after it, whichever luxd provisions next finds
// renamed_from and does step 2, which is idempotent. A re-tag the provider
// refuses (IAM) is logged and retried on the next check; the double
// listing keeps every instance claimed meanwhile. Static pools carry no
// tags: step 1 is the whole rename.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
	Confirm string `json:"confirm,omitempty" doc:"The pool's current name. Required while the pool has hosts that are not terminated (409 confirm_required otherwise), so a rename confirmed against counts that have since changed is refused."`
}

// PoolRenamed is what a rename moved or, with dryRun, would move.
type PoolRenamed struct {
	Pool      Pool `json:"pool"`
	Hosts     int  `json:"hosts" doc:"The pool's hosts that are not terminated; they follow the rename."`
	Runs      int  `json:"runs" doc:"Runs not yet final (neither succeeded, failed nor cancelled) that name the pool; their spec names the new one."`
	Instances int  `json:"instances" doc:"Of those hosts, the ones a provider launched: the provisioner re-tags their instances (lux:pool) after the rename."`
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

func lockPoolName(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pool-name:' || $1 || '/' || $2, 0))`, tenantID, name)
	return err
}

// CheckPoolNameFree holds the tenant's ("": the platform's) pool name
// until the transaction ends and refuses it while an unfinished rename
// reserves it: its instances may still carry it.
func CheckPoolNameFree(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	if err := lockPoolName(ctx, tx, tenantID, name); err != nil {
		return err
	}
	var reserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pools WHERE coalesce(tenant_id, '') = $1 AND renamed_from = $2)`,
		tenantID, name).Scan(&reserved); err != nil {
		return err
	}
	if reserved {
		return errf(http.StatusConflict, "pool_name_reserved", "pool %q is being renamed away from and its instances re-tagged: the name is free once that is done", name)
	}
	return nil
}

// renameArgs: rename From to To; Confirm is From, typed, when the pool has
// live hosts.
type renameArgs struct {
	From, To, Confirm string
	DryRun            bool
}

// capPoolRename is the capability a luxd advertises (luxd_instances) when
// its provisioner follows a pool rename: lists both names, never
// terminates a host on a tag listing alone.
const capPoolRename = "pool-rename"

// rename is step 1, in one transaction; tenantID "" names a platform pool.
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
		var id, provider string
		var renaming *string
		var cooldown float64
		err := tx.QueryRow(ctx, `SELECT id, provider, renamed_from,
				coalesce(extract(epoch FROM rename_finished_at + $3::interval - now()), 0)::float8
			FROM pools WHERE coalesce(tenant_id, '') = $1 AND name = $2 AND NOT retired FOR UPDATE`,
			tenantID, from, interval(s.renameCooldown())).Scan(&id, &provider, &renaming, &cooldown)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if renaming != nil && checkName {
			return errf(http.StatusConflict, "rename_in_progress",
				"pool %q is still being renamed from %q (its instances are being re-tagged): rename it again once that is done", from, *renaming)
		}
		if checkName && cooldown > 0 {
			// A re-tag sent by a provisioner whose lease has since expired can
			// still land for a while after the rename finished: a second
			// rename in that time could be undone on some instances.
			return errf(http.StatusConflict, "rename_cooldown",
				"pool %q finished a rename moments ago: it can be renamed again in %s", from, time.Duration(cooldown*float64(time.Second)).Round(time.Second))
		}
		if checkName {
			if err := checkRenameTarget(ctx, tx, tenantID, to); err != nil {
				return err
			}
		}
		if checkName && provider != "static" {
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
		var renamedFrom *string
		if provider != "static" {
			renamedFrom = &from
		}
		if _, err := tx.Exec(ctx, `UPDATE pools SET name = $2, renamed_from = $3, retagged_at = NULL WHERE id = $1`, id, to, renamedFrom); err != nil {
			return err
		}
		out.Pool, err = poolByID(ctx, tx, id)
		return err
	})
	return out, err
}

// renameCostHours moves the pool's hosts' cost hours still under the old
// name. Idempotent: step 2 runs it on every pass and when it finishes, for
// hours computed from a host row read before step 1 and written after it
// (cost writers copy hosts.pool without the rename's locks).
func renameCostHours(ctx context.Context, tx pgx.Tx, tenantID, from, to string) error {
	_, err := tx.Exec(ctx, `UPDATE cost_hourly SET pool = $3 WHERE pool = $2
		AND host_id IN (SELECT id FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $3)`, tenantID, from, to)
	return err
}

// renameCooldown: how long after a rename finished the pool may not be
// renamed again. A provisioner that lost its lease may still have a
// CreateTags in flight (bounded by the lease), and the listings may take
// listing_lag to show it.
func (s *Server) renameCooldown() time.Duration {
	return s.provisionLeaseDuration() + s.cfg.ListingLag
}

// checkDeploymentCanRename refuses a provisioned pool's rename while a luxd
// that cannot follow one may hold the provisioner lease: an older luxd
// lists only the new name, and would terminate every instance still
// carrying the old one. Every luxd writes control samples; the ones that
// can follow a rename also check in to luxd_instances.
func (s *Server) checkDeploymentCanRename(ctx context.Context, tx pgx.Tx) error {
	// The provisioner lease's holder too: a luxd that took it before its
	// first control sample.
	rows, err := tx.Query(ctx, `SELECT instance FROM (
			SELECT c.instance FROM control_samples c WHERE c.res = 0 AND c.at > now() - $1::interval
			UNION SELECT holder FROM leases WHERE name = 'provisioner' AND expires_at > now()) seen
		WHERE NOT EXISTS (SELECT 1 FROM luxd_instances l WHERE l.instance = seen.instance
		                  AND $2 = ANY(l.capabilities) AND l.seen_at > now() - $1::interval - $3::interval)
		ORDER BY 1 LIMIT 10`, interval(2*s.provisionLeaseDuration()), capPoolRename, interval(s.checkInEvery()))
	if err != nil {
		return err
	}
	old, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(old) > 0 {
		return errf(http.StatusConflict, "rename_unsupported_by_deployment",
			"luxd instances %v run a version that cannot follow a pool rename (it would terminate the pool's instances): upgrade every luxd first", old)
	}
	return nil
}

// checkIn records this luxd in luxd_instances: its version and what it
// can do.
func (s *Server) checkIn(ctx context.Context) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO luxd_instances (instance, version, capabilities, seen_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (instance) DO UPDATE SET version = EXCLUDED.version, capabilities = EXCLUDED.capabilities, seen_at = now()`,
			s.id, version.Version, []string{capPoolRename})
		return err
	})
}

// checkInEvery: a luxd's last check-in is at most this much older than
// its last control sample, which checkDeploymentCanRename allows for.
func (s *Server) checkInEvery() time.Duration {
	return min(s.cfg.ProviderCheckEvery, s.provisionLeaseDuration())
}

// checkInLoop checks in at start, then every checkInEvery.
func (s *Server) checkInLoop(ctx context.Context) {
	every := s.checkInEvery()
	for {
		if err := s.checkIn(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("luxd check-in", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// CheckPoolProviderChange refuses changing a pool's provider while a
// rename re-tags its instances: the old provider's instances would be
// listed by nothing.
func CheckPoolProviderChange(ctx context.Context, tx pgx.Tx, tenantID, name, provider string) error {
	var current string
	var renaming *string
	err := tx.QueryRow(ctx, `SELECT provider, renamed_from FROM pools WHERE coalesce(tenant_id, '') = $1 AND name = $2 FOR UPDATE`,
		tenantID, name).Scan(&current, &renaming)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if renaming != nil && current != provider {
		return errf(http.StatusConflict, "rename_in_progress",
			"pool %q is still being renamed from %q (its instances are being re-tagged): its provider cannot change until that is done", name, *renaming)
	}
	return nil
}

// checkRenameTarget: hosts and host tokens can name a pool no pool row has
// (a static host's token): renaming onto that name would merge them in.
func checkRenameTarget(ctx context.Context, tx pgx.Tx, tenantID, to string) error {
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pools WHERE coalesce(tenant_id, '') = $1 AND (name = $2 OR renamed_from = $2))
			OR EXISTS (SELECT 1 FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND state <> 'terminated')
			OR EXISTS (SELECT 1 FROM host_tokens WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND revoked_at IS NULL)`,
		tenantID, to).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return errf(http.StatusConflict, "pool_exists", "the name %q is taken: a pool (live, retired or being renamed from it), or hosts or host tokens, have it", to)
	}
	return nil
}

func poolByID(ctx context.Context, tx pgx.Tx, id string) (Pool, error) {
	var pl Pool
	var sda int
	err := tx.QueryRow(ctx, `SELECT `+poolColumns+` FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id WHERE p.id = $1`, id).Scan(
		&pl.Name, &pl.Tenant, &pl.Provider, &pl.Template, &pl.MinHosts, &pl.MaxHosts, &pl.WarmHosts,
		&sda, &pl.WarmWhileActive, &pl.Shared, &pl.Platform, &pl.HourlyPrice, &pl.Currency, &pl.RenamedFrom)
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

// renameCheck is what a provider check saw of a pool being renamed.
type renameCheck struct {
	// stale: live, claimed instances listed under the old name, by
	// template (its region), to re-tag.
	stale     map[string][]string
	templates map[string]json.RawMessage
	// unlisted: a live host whose instance the new name's listing missed.
	unlisted bool
}

// retagRenamed is step 2, after a provider check listed the pool under
// both names.
func (s *Server) retagRenamed(ctx context.Context, prov Provider, pl poolRow, st *poolState, rc renameCheck, lease *passLease) error {
	from := *pl.RenamedFrom
	newTag := poolTagValue(pl.TenantID, pl.Name)
	if len(rc.stale) > 0 {
		n := 0
		var failed error
		for key, pids := range rc.stale {
			// retagged_at is recorded before each call: a luxd that stops
			// mid-call leaves instances whose new tag the listings may not
			// show yet, and the next provisioner must know not to take them
			// for gone.
			var current bool
			err := s.fencedCall(ctx, lease, func(tx pgx.Tx) error {
				var err error
				if current, err = poolRenameStepTx(ctx, tx, pl, `retagged_at = now()`); err != nil || !current {
					return err
				}
				return renameCostHours(ctx, tx, ownerOf(pl), from, pl.Name)
			}, func(ctx context.Context) error {
				if !current {
					return nil
				}
				return prov.Retag(ctx, rc.templates[key], pids, tagPool, newTag)
			})
			if errors.Is(err, errFenced) {
				return err
			}
			if err != nil {
				failed = err
				continue
			}
			if !current {
				return nil
			}
			n += len(pids)
		}
		if failed != nil {
			s.log.Warn("pool rename: re-tagging instances failed; retried on the next provider check",
				"pool", pl.Name, "from", from, "retagged", n, "err", failed)
			return nil
		}
		s.log.Info("pool rename: instances re-tagged", "pool", pl.Name, "from", from, "instances", n)
		return nil
	}
	finish := !rc.unlisted && len(st.launching) == 0 && (pl.RetaggedAt == nil || time.Since(*pl.RetaggedAt) >= s.cfg.ListingLag)
	set := `retagged_at = retagged_at`
	if finish {
		set = `renamed_from = NULL, retagged_at = NULL, rename_finished_at = now()`
	}
	var done bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := s.fenceTx(ctx, tx, lease); err != nil {
			return err
		}
		var err error
		if done, err = poolRenameStepTx(ctx, tx, pl, set); err != nil || !done {
			return err
		}
		return renameCostHours(ctx, tx, ownerOf(pl), from, pl.Name)
	})
	if errors.Is(err, errFenced) {
		return err
	}
	if err != nil {
		s.log.Warn("pool rename", "pool", pl.Name, "err", err)
		return nil
	}
	if finish && done {
		// TODO(pool-events): a "rename finished" pool event.
		s.log.Info("pool rename finished: every instance carries the new name", "pool", pl.Name, "from", from)
	}
	return nil
}

// ownerOf is the pool's tenant id, "" for a platform pool.
func ownerOf(pl poolRow) string {
	if pl.TenantID == nil {
		return ""
	}
	return *pl.TenantID
}

// poolRenameStepTx updates the pool's rename columns, if the rename it was
// read with is still the one under way.
func poolRenameStepTx(ctx context.Context, tx pgx.Tx, pl poolRow, set string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE pools SET `+set+` WHERE id = $1 AND renamed_from = $2 AND name = $3`, pl.ID, *pl.RenamedFrom, pl.Name)
	return tag.RowsAffected() > 0, err
}
