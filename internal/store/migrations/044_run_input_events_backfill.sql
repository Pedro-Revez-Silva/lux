-- 044_run_input_events_backfill.sql — the keys of the input events
-- recorded before 043. A transaction of its own: it scans all of
-- run_events, and holds on runs and tenants only the row locks of its
-- foreign-key checks, which do not block their writes. An event without a
-- request id (absent or JSON null) has no key.

INSERT INTO run_input_events (tenant_id, run_id, type, request_id)
SELECT DISTINCT tenant_id, run_id, type, data->>'requestId' FROM run_events
WHERE type IN ('input.delivered', 'input.consumed', 'input.failed') AND data->>'requestId' IS NOT NULL
ON CONFLICT DO NOTHING;
