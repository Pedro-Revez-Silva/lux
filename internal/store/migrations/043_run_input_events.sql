-- 043_run_input_events.sql — luxd records each input event once per
-- request id and type, whatever the runner re-reports after a restart or
-- reconnect. The key it deduplicates on is a row here, inserted with the
-- event (ON CONFLICT DO NOTHING): one primary-key probe, not a search of
-- the Run's event history. (An expression index on run_events'
-- data->>'requestId' cannot serve that search: under row-level security
-- Postgres does not push the non-leakproof ->> into an index condition.)
--
-- The table is empty here, so the foreign keys' locks on runs and tenants
-- (which block writes to them) last only as long as this file; 044 fills
-- it in a transaction of its own. lux_app's grants come with every
-- migrate (ensureAppRole).

CREATE TABLE run_input_events (
  tenant_id  text NOT NULL REFERENCES tenants(id),
  run_id     text NOT NULL REFERENCES runs(id),
  type       text NOT NULL,
  request_id text NOT NULL,
  PRIMARY KEY (run_id, type, request_id)
);

ALTER TABLE run_input_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON run_input_events USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
