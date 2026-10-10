-- 064_cost_hourly_family.sql — host-hour rows (allocated and unallocated)
-- per host-tied family (docs/costs.md, section 7): compute and block
-- storage are refreshed side by side, one row each per host, hour and
-- currency.
ALTER TABLE cost_hourly DROP CONSTRAINT cost_hourly_check;
ALTER TABLE cost_hourly ADD CONSTRAINT cost_hourly_check
  CHECK ((run_id IS NOT NULL AND tenant_id IS NOT NULL AND allocated = 0 AND unallocated = 0)
      OR (run_id IS NULL AND tenant_id IS NULL AND host_id IS NOT NULL AND source = 'compute'
          AND family IN ('compute', 'block-storage') AND amount = 0));
DROP INDEX cost_hourly_host;
CREATE UNIQUE INDEX cost_hourly_host ON cost_hourly (host_id, hour, family, currency) WHERE run_id IS NULL;
