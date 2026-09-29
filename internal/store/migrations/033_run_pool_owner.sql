-- 033_run_pool_owner.sql — which pool a Run resolved to, not only its
-- name. Pool names are unique per owner (pools_name), so a tenant pool and
-- a platform pool can share one: with only the name, a Run meant for the
-- platform's `burst` could land on (or wait for) the tenant's `burst`.
--
-- pool_owner is the owner of the Run's pool (spec.placement.pool), as in
-- pools_name: '' for a platform pool, the tenant id for a tenant pool.
-- NULL: not resolved to a pool row (Runs from before this migration, and
-- names with no pool, such as static hosts that joined with a pool name no
-- pools row has); those keep matching hosts and pools by name alone.
-- Renaming a pool would have to rewrite spec.placement.pool of the Runs
-- with its (pool_owner, name).
ALTER TABLE runs ADD COLUMN pool_owner text;

-- A submit runs in its tenant's scope, where platform pools are out of
-- reach: this reads, as the table's owner, the owner of the pool a Run
-- naming `pool` goes to. The tenant's own shadows the platform's, as it
-- always has; NULL when neither exists. Only lux_app may call it (granted
-- in store.ensureAppRole).
CREATE FUNCTION lux_pool_owner(pool text) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
  SELECT coalesce(tenant_id, '')
  FROM pools
  WHERE name = pool AND NOT retired AND (tenant_id = lux_tenant() OR tenant_id IS NULL)
  ORDER BY tenant_id NULLS LAST
  LIMIT 1
$$;
REVOKE ALL ON FUNCTION lux_pool_owner(text) FROM PUBLIC;
