-- Run hours are tenant rows; host totals are system-only rows.
CREATE TABLE cost_hourly (
  hour timestamptz NOT NULL,
  tenant_id text REFERENCES tenants(id),
  run_id text REFERENCES runs(id),
  source text NOT NULL,
  family text NOT NULL,
  currency text NOT NULL,
  host_id text REFERENCES hosts(id),
  pool text,
  amount numeric(30, 9) NOT NULL DEFAULT 0,
  allocated numeric(30, 9) NOT NULL DEFAULT 0,
  unallocated numeric(30, 9) NOT NULL DEFAULT 0,
  CHECK ((run_id IS NOT NULL AND tenant_id IS NOT NULL AND allocated = 0 AND unallocated = 0)
      OR (run_id IS NULL AND tenant_id IS NULL AND host_id IS NOT NULL AND source = 'compute' AND family = 'compute' AND amount = 0))
);
CREATE UNIQUE INDEX cost_hourly_run ON cost_hourly (run_id, source, hour, family, currency, (coalesce(host_id, '')))
  WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX cost_hourly_host ON cost_hourly (host_id, hour, currency) WHERE run_id IS NULL;
CREATE INDEX cost_hourly_hour ON cost_hourly (hour);
CREATE INDEX cost_hourly_tenant_hour ON cost_hourly (tenant_id, hour) WHERE run_id IS NOT NULL;
ALTER TABLE cost_hourly ENABLE ROW LEVEL SECURITY;
CREATE POLICY cost_hourly_scope ON cost_hourly
  USING (lux_system() OR (run_id IS NOT NULL AND tenant_id = lux_tenant()))
  WITH CHECK (lux_system() OR (run_id IS NOT NULL AND tenant_id = lux_tenant()));
