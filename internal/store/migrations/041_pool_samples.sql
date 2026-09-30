-- 041_pool_samples.sql — a pool's history, per pool id (a rename keeps it;
-- so does a pool removed and set again under its name, which is the same
-- row and id), sampled with system_samples and
-- rolled up and expired as they are (docs/telemetry.md). History starts
-- when this ships: nothing is backfilled.
--
-- tenant_id '' is the whole pool: its hosts by state, capacity, what every
-- live placement on its hosts holds, every Run bound to it, and its
-- launches. A tenant's row is that tenant's own part (Runs, allocation,
-- starts and finishes), written for each tenant with anything on the pool,
-- so a tenant's view of a shared pool never counts another tenant's Runs.
CREATE TABLE pool_samples (
  pool_id         text NOT NULL REFERENCES pools(id),
  tenant_id       text NOT NULL,
  res             int  NOT NULL,
  at              timestamptz NOT NULL,
  -- Starts, finishes and launches counted up to here (see sampleSystem).
  window_end      timestamptz,
  hosts           jsonb NOT NULL DEFAULT '{}', -- by state (whole pool only)
  cap_cpus        float8 NOT NULL DEFAULT 0,   -- ready and draining hosts (whole pool only)
  cap_mem         bigint NOT NULL DEFAULT 0,
  alloc_cpus      float8 NOT NULL DEFAULT 0,   -- live placements on its hosts
  alloc_mem       bigint NOT NULL DEFAULT 0,
  running         int NOT NULL DEFAULT 0,      -- Runs bound to it, running
  queued          int NOT NULL DEFAULT 0,      -- ... submitted, resuming or provisioning
  started         int NOT NULL DEFAULT 0,      -- first starts in the window
  finished        int NOT NULL DEFAULT 0,
  launches        int NOT NULL DEFAULT 0,      -- launches requested in the window (whole pool only)
  launch_failures int NOT NULL DEFAULT 0,      -- launches the provider refused in the window
  PRIMARY KEY (pool_id, tenant_id, res, at)
);
CREATE INDEX pool_samples_res_at ON pool_samples (res, at);
ALTER TABLE pool_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON pool_samples USING (lux_system()) WITH CHECK (lux_system());

-- A pool's Runs started or finished in a window, and its launches.
CREATE INDEX runs_pool_started ON runs (pool_id, first_started_at) WHERE first_started_at IS NOT NULL;
CREATE INDEX hosts_pool_launch ON hosts (pool_id, provision_requested_at) WHERE provision_requested_at IS NOT NULL;
