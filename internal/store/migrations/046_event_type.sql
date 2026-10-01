-- 046_event_type.sql — a pool's and a host's events by type (?sort=type),
-- keys from the index alone, as 045's for time and id: the page stops at
-- its end whatever the owner's event count.
CREATE INDEX pool_events_pool_type ON pool_events (pool_id, type, id) INCLUDE (tenant_id);
CREATE INDEX host_events_host_type ON host_events (host_id, type, id) INCLUDE (tenant_id);
