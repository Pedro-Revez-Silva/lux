-- 042_dashboard_indexes.sql — indexes for the dashboards' reads.
--
-- A pool's and a host's events in time order (the console's default sort,
-- ?sort=time): the page and its (created_at, id) keyset read the index.
CREATE INDEX pool_events_pool_time ON pool_events (pool_id, created_at, id);
CREATE INDEX host_events_host_time ON host_events (host_id, created_at, id);

-- A pool's Runs' cost (GET /v1/pools/{name}/cost, /v1/pools/stats): the
-- rows that carry their host's pool, by pool and hour. Rows without one
-- (other families) go by their Run's pool.
CREATE INDEX cost_hourly_pool_hour ON cost_hourly (pool_id, hour) WHERE run_id IS NOT NULL;

-- 041's were chosen by no plan: the sampler's flow reads runs by
-- first_started_at and finished_at, stats reads every visible pool. The
-- sampler's launches read recent requests, pool or not.
DROP INDEX runs_pool_started;
DROP INDEX hosts_pool_launch;
CREATE INDEX hosts_launch_requested ON hosts (provision_requested_at) WHERE provision_requested_at IS NOT NULL;
