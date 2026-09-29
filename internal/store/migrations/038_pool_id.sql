-- 038_pool_id.sql — a pool's id is its identity; its name is metadata a
-- rename changes. Hosts, host tokens, Runs and hourly costs refer to their
-- pool by id from here on, and the name columns go.

-- A static pool used to exist only as a name on host tokens and hosts.
-- Every such name becomes a static pool row of its owner, with an id made
-- from owner and name.
INSERT INTO pools (id, tenant_id, name, provider)
SELECT DISTINCT 'pool_' || substr(md5(coalesce(x.tenant_id, '') || '/' || x.pool), 1, 16), x.tenant_id, x.pool, 'static'
FROM (SELECT tenant_id, pool FROM host_tokens UNION SELECT tenant_id, pool FROM hosts) x
WHERE NOT EXISTS (SELECT 1 FROM pools p WHERE coalesce(p.tenant_id, '') = coalesce(x.tenant_id, '') AND p.name = x.pool);

ALTER TABLE host_tokens ADD COLUMN pool_id text REFERENCES pools(id);
UPDATE host_tokens t SET pool_id = p.id FROM pools p
WHERE coalesce(p.tenant_id, '') = coalesce(t.tenant_id, '') AND p.name = t.pool;
ALTER TABLE host_tokens DROP COLUMN pool;

ALTER TABLE hosts ADD COLUMN pool_id text REFERENCES pools(id);
UPDATE hosts h SET pool_id = p.id FROM pools p
WHERE coalesce(p.tenant_id, '') = coalesce(h.tenant_id, '') AND p.name = h.pool;
ALTER TABLE hosts DROP COLUMN pool;
CREATE INDEX hosts_pool ON hosts (pool_id);

-- A Run's pool: the one its pool_owner named; for a Run from before 033,
-- the tenant's pool of its spec's name, else the platform's. NULL: no pool
-- had the name; the scheduler binds it once one does.
ALTER TABLE runs ADD COLUMN pool_id text REFERENCES pools(id);
UPDATE runs r SET pool_id = (
  SELECT p.id FROM pools p
  WHERE p.name = coalesce(r.spec->'placement'->>'pool', 'default')
    AND (r.pool_owner IS NOT NULL OR NOT p.retired)
    AND CASE WHEN r.pool_owner IS NOT NULL THEN coalesce(p.tenant_id, '') = r.pool_owner
             ELSE (p.tenant_id = r.tenant_id OR p.tenant_id IS NULL) END
  ORDER BY p.tenant_id NULLS LAST, p.retired LIMIT 1);
ALTER TABLE runs DROP COLUMN pool_owner;
CREATE INDEX runs_pool ON runs (pool_id) WHERE state IN ('submitted', 'resuming', 'provisioning');

ALTER TABLE cost_hourly ADD COLUMN pool_id text REFERENCES pools(id);
UPDATE cost_hourly c SET pool_id = h.pool_id FROM hosts h WHERE h.id = c.host_id;
ALTER TABLE cost_hourly DROP COLUMN pool;

-- A tenant reads the platform's pools by name already (GET /v1/pools, as
-- the system); this lets tenant-scoped reads show a platform pool's
-- current name beside its Runs and costs. Writes stay the owner's.
CREATE POLICY platform_read ON pools FOR SELECT USING (tenant_id IS NULL);

-- The default and a named pool now resolve to a pool id (lux_app's only,
-- granted in store.ensureAppRole).
DROP FUNCTION lux_default_pool();
CREATE FUNCTION lux_default_pool(OUT pool_id text, OUT pool text, OUT pool_from text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
  SELECT id, name, CASE WHEN tenant_id IS NULL THEN 'platform-default' ELSE 'tenant-default' END
  FROM pools
  WHERE is_default AND NOT retired AND (tenant_id = lux_tenant() OR tenant_id IS NULL)
  ORDER BY tenant_id NULLS LAST
  LIMIT 1
$$;
REVOKE ALL ON FUNCTION lux_default_pool() FROM PUBLIC;

DROP FUNCTION lux_pool_owner(text);
CREATE FUNCTION lux_pool_id(pool text, OUT pool_id text, OUT platform boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
  SELECT id, tenant_id IS NULL
  FROM pools
  WHERE name = pool AND NOT retired AND (tenant_id = lux_tenant() OR tenant_id IS NULL)
  ORDER BY tenant_id NULLS LAST
  LIMIT 1
$$;
REVOKE ALL ON FUNCTION lux_pool_id(text) FROM PUBLIC;
