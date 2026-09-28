-- Placement estimates are frozen independently when their end and rate are known.
CREATE TABLE cost_placement_snapshots (
  placement_id text PRIMARY KEY REFERENCES placements(id),
  run_id text NOT NULL REFERENCES runs(id),
  tenant_id text NOT NULL REFERENCES tenants(id),
  host_id text NOT NULL REFERENCES hosts(id),
  rate_from timestamptz,
  per_hour numeric(24, 9),
  currency text,
  cap_cpus float8,
  cap_memory bigint,
  amount numeric(30, 9),
  priced_to timestamptz,
  finalized boolean NOT NULL DEFAULT false,
  CHECK (NOT finalized OR (rate_from IS NOT NULL AND amount IS NOT NULL AND priced_to IS NOT NULL)),
  CHECK ((per_hour IS NULL AND currency IS NULL AND amount IS NULL)
      OR (per_hour IS NOT NULL AND currency IS NOT NULL AND cap_cpus IS NOT NULL AND cap_memory IS NOT NULL))
);
CREATE INDEX cost_placement_snapshots_run ON cost_placement_snapshots (run_id);
ALTER TABLE cost_placement_snapshots ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_placement_snapshots
  USING (lux_system()) WITH CHECK (lux_system());
