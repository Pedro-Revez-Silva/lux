-- 066_cost_hourly_host_pool_hour.sql — host-hour rows by pool and hour:
-- a tenant's unallocated and per-host reads (its own pools' host rows) and
-- a pool's idle and host time. cost_hourly_pool_hour (042) holds Run rows
-- only, and cost_hourly_host leads with host_id, so without this those
-- reads walk every host row in range.
CREATE INDEX cost_hourly_host_pool_hour ON cost_hourly (pool_id, hour) WHERE run_id IS NULL;
