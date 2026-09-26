-- 020_run_sessions_backfill.sql — run_sessions (017) for Runs recorded
-- before it. Each migration file is its own transaction; this one is
-- separate from 017 so the SHARE ROW EXCLUSIVE lock that adding the foreign
-- keys takes on runs is released before the scan of every run_events row.
-- The inserts below only take FOR KEY SHARE row locks on runs, which do
-- not block Run updates. ON CONFLICT DO NOTHING: if a luxd already wrote a
-- row between 017 and this migration, its row stands.

-- Backfill, from every place an id was recorded: session events, snapshot
-- manifests (which never wrote an event), and runs.session_id (at the
-- current epoch, only when no other source has that id for the Run).
INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id, first_seen, last_seen)
SELECT tenant_id, run_id, epoch, session_id, min(at), max(at) FROM (
  SELECT tenant_id, run_id, coalesce(epoch, 0) AS epoch, data->>'sessionId' AS session_id, created_at AS at
    FROM run_events WHERE type = 'session'
  UNION ALL
  SELECT tenant_id, run_id, epoch, manifest->>'sessionId', created_at
    FROM snapshots
) s
WHERE session_id <> ''
GROUP BY tenant_id, run_id, epoch, session_id
ON CONFLICT DO NOTHING;

INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id, first_seen, last_seen)
SELECT r.tenant_id, r.id, r.current_epoch, r.session_id, r.updated_at, r.updated_at
  FROM runs r
 WHERE r.session_id <> ''
   AND NOT EXISTS (SELECT 1 FROM run_sessions x WHERE x.run_id = r.id AND x.session_id = r.session_id)
ON CONFLICT DO NOTHING;
