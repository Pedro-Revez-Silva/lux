-- 031_pool_rename.sql — an unfinished rename of a provisioned pool
-- (internal/server/poolrename.go). The database moves to the new name in
-- one transaction; the provider's instances are re-tagged afterwards, by
-- the provisioner. Until they all are:
--
-- renamed_from: the old name. The provisioner lists the pool's instances
--   under both names, and the old name stays reserved (no pool may take
--   it) so no other pool lists these instances as its orphans.
-- retagged_at: when every instance was last confirmed to carry the new
--   name. renamed_from is cleared once the provider's listings have had
--   time to catch up with the new tags (luxd's listing_lag after it).
-- rename_finished_at: when renamed_from was last cleared. A late re-tag
--   from an expired provisioner may still land for a while after it, so
--   the pool is not renamed again until the lease and listing_lag have
--   passed.
ALTER TABLE pools ADD COLUMN renamed_from text;
ALTER TABLE pools ADD COLUMN retagged_at timestamptz;
ALTER TABLE pools ADD COLUMN rename_finished_at timestamptz;
CREATE UNIQUE INDEX pools_renamed_from ON pools (coalesce(tenant_id, ''), renamed_from) WHERE renamed_from IS NOT NULL;

-- A fencing token: a new one each time a different holder takes the lease,
-- so a provisioner pass can tell, before each destructive provider call,
-- that no other luxd has held the lease since the pass began. From a
-- sequence, so a lease row deleted (released) and taken again never hands
-- out a token already used.
CREATE SEQUENCE lease_tokens;
ALTER TABLE leases ADD COLUMN token bigint NOT NULL DEFAULT 0;

-- Every luxd process on this database, as of its last check-in: its
-- version and what it can do. A pool rename refuses to start while a luxd
-- that cannot follow one (an older binary, seen only in control_samples)
-- may hold the provisioner lease. instance is control_samples.instance.
CREATE TABLE luxd_instances (
  instance     text PRIMARY KEY,
  version      text NOT NULL,
  capabilities text[] NOT NULL,
  seen_at      timestamptz NOT NULL
);
ALTER TABLE luxd_instances ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON luxd_instances USING (lux_system()) WITH CHECK (lux_system());
