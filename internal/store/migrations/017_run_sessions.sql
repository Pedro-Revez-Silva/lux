-- 017_run_sessions.sql — every top-level session id a Run has had, per
-- epoch. runs.session_id keeps only the latest; a resume that cannot load
-- the old session starts a new one, and the old id's usage still belongs
-- to the Run (docs/costs.md, section 6). Written where runs.session_id is:
-- adapter session events and snapshot manifests.

CREATE TABLE run_sessions (
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  epoch       int  NOT NULL,
  session_id  text NOT NULL,
  first_seen  timestamptz NOT NULL DEFAULT now(),
  last_seen   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, epoch, session_id)
);
-- The same id in two Runs of one tenant.
CREATE INDEX run_sessions_session ON run_sessions (tenant_id, session_id);

ALTER TABLE run_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON run_sessions USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());

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
GROUP BY tenant_id, run_id, epoch, session_id;

INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id, first_seen, last_seen)
SELECT r.tenant_id, r.id, r.current_epoch, r.session_id, r.updated_at, r.updated_at
  FROM runs r
 WHERE r.session_id <> ''
   AND NOT EXISTS (SELECT 1 FROM run_sessions x WHERE x.run_id = r.id AND x.session_id = r.session_id);
