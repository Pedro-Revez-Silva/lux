-- 031_default_pool.sql — a pool marked as the default: where a Run that
-- names no pool goes. At most one per tenant, and one among platform pools
-- (tenant_id NULL), which serves tenants without their own. No existing
-- pool is marked: until one is, such Runs go to a pool named "default".
ALTER TABLE pools ADD COLUMN is_default boolean NOT NULL DEFAULT false;

-- One default per owner (coalesce as in pools_name: '' is the platform),
-- on a partial btree index. An exclusion constraint rather than a unique
-- index because it can be checked at the end of the statement: moving the
-- mark is one UPDATE that sets one row and clears another, which a unique
-- index, checked row by row, refuses or not depending on the rows' order.
ALTER TABLE pools ADD CONSTRAINT pools_one_default
  EXCLUDE USING btree ((coalesce(tenant_id, '')) WITH =) WHERE (is_default)
  DEFERRABLE INITIALLY IMMEDIATE;

-- A submit runs in its tenant's scope, where platform pools are out of
-- reach: this reads the caller's default and the platform's as the table's
-- owner, the tenant's first. Only lux_app may call it (granted in
-- store.ensureAppRole, like lux_cost_enqueue).
CREATE FUNCTION lux_default_pool(OUT pool text, OUT pool_from text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
  SELECT name, CASE WHEN tenant_id IS NULL THEN 'platform-default' ELSE 'tenant-default' END
  FROM pools
  WHERE is_default AND NOT retired AND (tenant_id = lux_tenant() OR tenant_id IS NULL)
  ORDER BY tenant_id NULLS LAST
  LIMIT 1
$$;
REVOKE ALL ON FUNCTION lux_default_pool() FROM PUBLIC;
