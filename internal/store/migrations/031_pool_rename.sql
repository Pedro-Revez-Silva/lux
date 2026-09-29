-- 031_pool_rename.sql — renaming a provisioned pool
-- (internal/server/poolrename.go). The database moves to the new name in
-- one transaction; the provider's instances are re-tagged afterwards, by
-- the provisioner.
--
-- pool_tag_aliases: lux:pool tag values a pool's instances may still
--   carry, one row per name the pool was renamed away from. The
--   provisioner lists the pool under its name and every live alias
--   (retired_at NULL), and a live alias is reserved: no pool of the owner
--   may take it, so no other pool lists these instances as its orphans.
--   An instance launched with the old tag after the rename (its
--   RunInstances in flight across it) is still listed, and claimed or
--   terminated as an orphan.
-- finished_at: the rename away from this name finished: a provider check
--   listed nothing under any alias and every live host under the pool's
--   name. At most one alias per pool is unfinished.
-- empty_since: since when every provider check found nothing under this
--   alias (NULL once one found something). An alias is retired once
--   finished and empty for twice the longest a launch can take to show in
--   the listings.
-- tenant_id: the pool's, copied so the reservation is one unique index.
CREATE TABLE pool_tag_aliases (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pool_id     text NOT NULL REFERENCES pools(id),
  tenant_id   text REFERENCES tenants(id),
  name        text NOT NULL,
  added_at    timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  empty_since timestamptz,
  retired_at  timestamptz
);
CREATE UNIQUE INDEX pool_tag_aliases_live ON pool_tag_aliases (coalesce(tenant_id, ''), name) WHERE retired_at IS NULL;
CREATE UNIQUE INDEX pool_tag_aliases_unfinished ON pool_tag_aliases (pool_id) WHERE retired_at IS NULL AND finished_at IS NULL;
CREATE INDEX pool_tag_aliases_pool ON pool_tag_aliases (pool_id) WHERE retired_at IS NULL;
ALTER TABLE pool_tag_aliases ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON pool_tag_aliases USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system());

-- retagged_at: when the provisioner last re-tagged some of the pool's
--   instances; the rename does not finish until listing_lag after it.
-- rename_finished_at: when the last rename finished. A late re-tag from an
--   expired provisioner may still land for a while after it, so the pool
--   is not renamed again until the lease and listing_lag have passed.
ALTER TABLE pools ADD COLUMN retagged_at timestamptz;
ALTER TABLE pools ADD COLUMN rename_finished_at timestamptz;

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
