-- 090_run_events_tenant_id.sql — a tenant's events newest first, for the
-- feed's filtered last=N (internal/server/feed.go, feedLastSQL): its scan of
-- the newest events and its fallback max(id) bound tenant_id as an index
-- condition, so they do not walk other tenants' rows under RLS. Plain, not
-- CONCURRENTLY: each migration runs in a transaction.
CREATE INDEX run_events_tenant ON run_events (tenant_id, id);
