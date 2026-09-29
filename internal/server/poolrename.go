package server

// Renaming a pool (POST /v1/pools/{name}/rename).
//
// A pool's name is in the database (pools.name, hosts.pool,
// host_tokens.pool, cost_hourly.pool, the spec of every Run not yet final)
// and, for a provisioned pool, on its instances: the lux:pool tag. The
// provisioner lists a pool's instances by that tag, terminates a listed
// instance no live host row of the pool claims (an orphan), and writes off
// a settled host the listing misses. A rename must never let either check
// see a live instance wrongly. It is two steps:
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
//     pools.retagged_at. Tag filters are eventually consistent: for
//     listing_lag after a re-tag, a settled host missing from both
//     listings is not written off. The rename is finished (renamed_from
//     cleared) by a pass that lists nothing under the old name, lists
//     every live host's instance under the new one, has no launch in
//     flight, and comes listing_lag after the last re-tag.
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
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marcioapm/lux/internal/store"
)

type renamePoolInput struct {
	TenantQuery
	Name   string `path:"name" doc:"The pool's current name."`
	DryRun bool   `query:"dryRun" doc:"Check the rename and count what would follow it, without renaming."`
	Body   renamePoolRequest
}

type renamePoolRequest struct {
	Name string `json:"name" doc:"The new name." example:"burst-eu"`
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
	out, err := renamePool(ctx, s.db, s.log, principal(ctx).TenantID, in.Name, in.Body.Name, in.DryRun)
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

// renamePool is step 1, in one transaction; tenantID "" names a platform
// pool.
func renamePool(ctx context.Context, db *store.Store, log *slog.Logger, tenantID, from, to string, dryRun bool) (PoolRenamed, error) {
	var out PoolRenamed
	// A new name is never grandfathered: it becomes a tag value and a
	// hostname, and the rule keeps "<tenant>/<name>" tag values unambiguous.
	if err := ValidPoolName(to); err != nil {
		return out, errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
	}
	if to == from {
		return out, errf(http.StatusUnprocessableEntity, "invalid_pool", "the pool is already named %q", to)
	}
	var err error
	// Runs are locked before hosts, as drainHosts does; a deadlock with a
	// path taking them the other way round is retried.
	for range 3 {
		out, err = renamePoolTx(ctx, db, tenantID, from, to, dryRun)
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
	log.Info("pool renamed", "tenant", tenantID, "from", from, "to", to,
		"hosts", out.Hosts, "runs", out.Runs, "instances", out.Instances)
	return out, nil
}

func renamePoolTx(ctx context.Context, db *store.Store, tenantID, from, to string, dryRun bool) (PoolRenamed, error) {
	out := PoolRenamed{DryRun: dryRun}
	err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Both names, always in the same order.
		if err := lockPoolName(ctx, tx, tenantID, min(from, to)); err != nil {
			return err
		}
		if err := lockPoolName(ctx, tx, tenantID, max(from, to)); err != nil {
			return err
		}
		var id, provider string
		var renaming *string
		err := tx.QueryRow(ctx, `SELECT id, provider, renamed_from FROM pools
			WHERE coalesce(tenant_id, '') = $1 AND name = $2 AND NOT retired FOR UPDATE`, tenantID, from).Scan(&id, &provider, &renaming)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if renaming != nil {
			return errf(http.StatusConflict, "rename_in_progress",
				"pool %q is still being renamed from %q (its instances are being re-tagged): rename it again once that is done", from, *renaming)
		}
		// Hosts and host tokens can name a pool no pool row has (a static
		// host's token): renaming onto that name would merge them in.
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
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE provision_requested_at IS NOT NULL)
			FROM hosts WHERE coalesce(tenant_id, '') = $1 AND pool = $2 AND state <> 'terminated'`,
			tenantID, from).Scan(&out.Hosts, &out.Instances); err != nil {
			return err
		}
		if dryRun {
			out.Pool, err = poolByID(ctx, tx, id)
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE runs SET spec = jsonb_set(spec, '{placement,pool}', to_jsonb($3::text)), updated_at = now()
			WHERE id IN (SELECT r.id `+runs+` ORDER BY r.id FOR UPDATE)`, tenantID, from, to); err != nil {
			return err
		}
		// Terminated hosts too: cost pricing joins a host to its pool by
		// name, also after it is gone, and a pool later given the old name
		// must not claim them.
		rows, err := tx.Query(ctx, `UPDATE hosts SET pool = $3 WHERE coalesce(tenant_id, '') = $1 AND pool = $2 RETURNING id`, tenantID, from, to)
		if err != nil {
			return err
		}
		hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_tokens SET pool = $3 WHERE coalesce(tenant_id, '') = $1 AND pool = $2`, tenantID, from, to); err != nil {
			return err
		}
		// cost_hourly.pool is copied from hosts.pool whenever an hour is
		// (re)computed: left alone, one host's hours would split between
		// both names as they are recomputed.
		if _, err := tx.Exec(ctx, `UPDATE cost_hourly SET pool = $2 WHERE host_id = ANY($1) AND pool = $3`, hosts, to, from); err != nil {
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
func (s *Server) retagRenamed(ctx context.Context, prov Provider, pl poolRow, st *poolState, rc renameCheck) {
	from := *pl.RenamedFrom
	newTag := poolTagValue(pl.TenantID, pl.Name)
	if len(rc.stale) > 0 {
		// Recorded first: a luxd that stops mid-call leaves instances whose
		// new tag the listings may not show yet, and the next provisioner
		// must know not to take them for gone.
		if !s.poolRenameStep(ctx, pl, `retagged_at = now()`) {
			return
		}
		n := 0
		var failed error
		for key, pids := range rc.stale {
			if err := prov.Retag(ctx, rc.templates[key], pids, tagPool, newTag); err != nil {
				failed = err
				continue
			}
			n += len(pids)
		}
		if failed != nil {
			s.log.Warn("pool rename: re-tagging instances failed; retried on the next provider check",
				"pool", pl.Name, "from", from, "retagged", n, "err", failed)
			return
		}
		s.log.Info("pool rename: instances re-tagged", "pool", pl.Name, "from", from, "instances", n)
		return
	}
	if rc.unlisted || len(st.launching) > 0 || pl.RetaggedAt != nil && time.Since(*pl.RetaggedAt) < s.cfg.ListingLag {
		return
	}
	if s.poolRenameStep(ctx, pl, `renamed_from = NULL, retagged_at = NULL`) {
		// TODO(pool-events): a "rename finished" pool event.
		s.log.Info("pool rename finished: every instance carries the new name", "pool", pl.Name, "from", from)
	}
}

// poolRenameStep updates the pool's rename columns, if the rename it was
// read with is still the one under way.
func (s *Server) poolRenameStep(ctx context.Context, pl poolRow, set string) bool {
	var n int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE pools SET `+set+` WHERE id = $1 AND renamed_from = $2`, pl.ID, *pl.RenamedFrom)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		s.log.Warn("pool rename", "pool", pl.Name, "err", err)
	}
	return err == nil && n > 0
}
