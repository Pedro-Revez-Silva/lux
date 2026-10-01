-- 045_event_cover.sql — a pool's and a host's event lists read their keys
-- from the index alone.
--
-- Under row-level security the planner guesses about 0.5% of an owner's
-- events pass tenant_rows, so an ordered scan that must visit the heap for
-- tenant_id looked dearer than a bitmap scan of every event of the owner
-- and a sort: linear in the owner's events. With tenant_id in the index,
-- the ordered scan checks the policy without the heap and stops at the
-- page's end (lifecycleEvents reads the keys this way, then the rows by
-- id). Same keys as the indexes they replace, which every other reader of
-- these tables (folds, transitions, the capacity plan) uses unchanged.
CREATE INDEX pool_events_pool_time_cover ON pool_events (pool_id, created_at, id) INCLUDE (tenant_id);
CREATE INDEX host_events_host_time_cover ON host_events (host_id, created_at, id) INCLUDE (tenant_id);
CREATE INDEX pool_events_pool_cover ON pool_events (pool_id, id) INCLUDE (tenant_id);
CREATE INDEX host_events_host_cover ON host_events (host_id, id) INCLUDE (tenant_id);
DROP INDEX pool_events_pool_time;
DROP INDEX host_events_host_time;
DROP INDEX pool_events_pool;
DROP INDEX host_events_host;
