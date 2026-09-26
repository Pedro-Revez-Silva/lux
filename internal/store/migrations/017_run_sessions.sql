-- 017_run_sessions.sql — every top-level session id a Run has had, per
-- epoch. runs.session_id keeps only the latest; a resume that cannot load
-- the old session starts a new one, and the old id's usage still belongs
-- to the Run (docs/costs.md, section 6). Written where runs.session_id is:
-- adapter session events and snapshot manifests. Rows written before this
-- luxd are filled by 020_run_sessions_backfill.sql, a transaction of its
-- own: the foreign keys' lock on runs is released before that scan.

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
