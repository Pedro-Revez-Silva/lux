-- 019_cost_lines.sql — what each Run costs, as lines reported per source
-- (docs/costs.md, sections 1 and 5). A source's latest answer for a Run
-- replaces all its earlier lines for that Run. Amounts are exact decimals
-- in the currency they were reported in, never converted or summed across
-- currencies.

CREATE TABLE cost_lines (
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  source      text NOT NULL,              -- 'compute' or a plugin's configured name
  item        text NOT NULL DEFAULT '',   -- '' when the source gives none
  family      text NOT NULL,              -- free-form: compute, ai, video, image-gen, ...
  amount      numeric(24, 9) NOT NULL,
  currency    text NOT NULL,              -- ISO 4217 as sent
  period_from timestamptz NOT NULL,       -- the time window this line covers
  period_to   timestamptz NOT NULL,
  final       boolean NOT NULL DEFAULT false,  -- false: an estimate
  details     jsonb NOT NULL DEFAULT '{}',
  reported_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, run_id, item)
);
CREATE INDEX cost_lines_tenant_period ON cost_lines (tenant_id, period_to);
-- The read API's lookup: the primary key leads with source.
CREATE INDEX cost_lines_run ON cost_lines (run_id);

-- Each source's standing for a Run: answered, still owed, or settled.
CREATE TABLE cost_sources (
  run_id        text NOT NULL REFERENCES runs(id),
  tenant_id     text NOT NULL,
  source        text NOT NULL,
  status        text NOT NULL CHECK (status IN ('ok', 'incomplete', 'final')),
  answered_at   timestamptz,       -- last successful answer
  attempts      int NOT NULL DEFAULT 0,
  next_at       timestamptz,       -- next retry or settle attempt
  settles_left  int,               -- set when the Run turns terminal
  last_error    text NOT NULL DEFAULT '',  -- operators only
  PRIMARY KEY (run_id, source)
);

ALTER TABLE cost_lines ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON cost_lines USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
ALTER TABLE cost_sources ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON cost_sources USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
