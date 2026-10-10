-- 065_cost_hourly_tenant_hosts.sql — a tenant reads the host-hour rows
-- (allocated and unallocated, every host-tied family) of the hosts in its
-- own pools (docs/costs.md, section 8). A platform pool's host rows stay
-- the operators': pools.tenant_id is NULL for them.
CREATE POLICY cost_hourly_own_hosts ON cost_hourly FOR SELECT
  USING (run_id IS NULL AND lux_tenant() IS NOT NULL
    AND EXISTS (SELECT 1 FROM pools p WHERE p.id = cost_hourly.pool_id AND p.tenant_id = lux_tenant()));
