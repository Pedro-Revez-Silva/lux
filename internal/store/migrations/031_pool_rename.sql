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
ALTER TABLE pools ADD COLUMN renamed_from text;
ALTER TABLE pools ADD COLUMN retagged_at timestamptz;
CREATE UNIQUE INDEX pools_renamed_from ON pools (coalesce(tenant_id, ''), renamed_from) WHERE renamed_from IS NOT NULL;
