-- 031_pool_rename.sql — renaming a pool (internal/server/poolrename.go).
-- The provisioner finds a pool's instances by an immutable tag,
-- lux:pool-id (pools.id), not by its name (lux:pool), so a rename is a
-- database change; the name tag is updated afterwards, for people only.

-- pool_id_tagged: the host's instance carries lux:pool-id: launched with
--   it, or seen carrying it by a provider check. Hosts from before this
--   migration do not; while a pool has a live one, the provisioner also
--   lists the pool by name, tags what it finds with lux:pool-id, and the
--   pool cannot be renamed.
-- not_found_since: when a provider check first found the host's instance
--   unknown to the provider (EC2's InvalidInstanceID.NotFound), cleared on
--   any sighting. EC2 is eventually consistent: a just-launched instance
--   may be unknown for a while, so NotFound writes a host off only when
--   seen again listing_lag later, on an old host.
ALTER TABLE hosts ADD COLUMN pool_id_tagged boolean NOT NULL DEFAULT false;
ALTER TABLE hosts ADD COLUMN not_found_since timestamptz;

-- renamed_at: when the pool was last renamed; a provisioned pool having
--   one fences older luxd out of the provisioner lease (below).
-- previous_names: names the pool had, oldest first: a host token or Run
--   naming one while nothing else does is refused, naming the pool's new
--   name, rather than joining a pool that no longer exists. Any pool may
--   take one.
ALTER TABLE pools ADD COLUMN renamed_at timestamptz;
ALTER TABLE pools ADD COLUMN previous_names text[] NOT NULL DEFAULT '{}';

-- A fencing token: a new one each time a different holder takes the lease,
-- so a provisioner pass can tell, before each destructive provider call,
-- that no other luxd has held the lease since the pass began. From a
-- sequence, so a lease row deleted (released) and taken again never hands
-- out a token already used.
CREATE SEQUENCE lease_tokens;
ALTER TABLE leases ADD COLUMN token bigint NOT NULL DEFAULT 0;
-- The lease is now a fence: luxd's own, as control_samples are; no
-- tenant scope reads or moves it.
ALTER TABLE leases ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON leases USING (lux_system()) WITH CHECK (lux_system());

-- Every luxd process on this database, as of its last check-in: its
-- version and what it can do. instance is control_samples.instance.
CREATE TABLE luxd_instances (
  instance     text PRIMARY KEY,
  version      text NOT NULL,
  capabilities text[] NOT NULL,
  seen_at      timestamptz NOT NULL
);
ALTER TABLE luxd_instances ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON luxd_instances USING (lux_system()) WITH CHECK (lux_system());

-- An older luxd lists a pool's instances by name: once a provisioned pool
-- has been renamed, instances whose lux:pool still carries an old name
-- (the re-tag is asynchronous) would be missed or taken for another
-- pool's orphans. So once any such pool has a renamed_at, taking the provisioner lease (an INSERT, or an UPDATE that
-- changes its holder or follows its expiry) is refused unless the new
-- holder checked in with the pool-id-discovery capability. A luxd checks
-- in before its first attempt at the lease, and its instance id is new
-- with every process; an older binary never checks in. Before the first
-- rename, a mixed fleet provisions as it always did.
-- A rename makes sure the lease row exists (holder '', expired: a
-- placeholder anyone may take) and locks it before it reads the holder and
-- sets renamed_at, so a lease acquisition waits for the rename's commit and
-- sees renamed_at, or commits first and the rename sees the new holder.
-- SECURITY DEFINER: the check sees every pool whatever the writer's scope.
CREATE FUNCTION lux_provisioner_lease_fence() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
  IF NEW.name <> 'provisioner' OR NEW.holder = '' THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.holder = NEW.holder AND OLD.expires_at > clock_timestamp() THEN
    RETURN NEW; -- a renewal
  END IF;
  IF EXISTS (SELECT 1 FROM pools WHERE renamed_at IS NOT NULL AND provider <> 'static')
     AND NOT EXISTS (SELECT 1 FROM luxd_instances WHERE instance = NEW.holder AND 'pool-id-discovery' = ANY (capabilities)) THEN
    RAISE EXCEPTION 'luxd % discovers instances by pool name: it may not take the provisioner lease once a pool has been renamed (upgrade it)', NEW.holder
      USING ERRCODE = 'LX001';
  END IF;
  RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION lux_provisioner_lease_fence() FROM PUBLIC;
CREATE TRIGGER leases_provisioner_fence BEFORE INSERT OR UPDATE ON leases
  FOR EACH ROW EXECUTE FUNCTION lux_provisioner_lease_fence();

-- The name a Run's pool was renamed to, for the caller's tenant: a live
-- pool of the tenant or the platform that was once named $1, when no live
-- pool, live host or live host token of either answers to $1 now (the
-- tenant's own first). NULL otherwise. submitRun runs in the tenant's
-- scope, where platform pools are out of reach; this runs as the tables'
-- owner and reveals only names of pools the tenant's Runs may use. Only
-- lux_app may call it (store.ensureAppRole).
CREATE FUNCTION lux_pool_renamed_to(text) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
  SELECT p.name FROM pools p
  WHERE (p.tenant_id = lux_tenant() OR p.tenant_id IS NULL) AND NOT p.retired AND $1 = ANY (p.previous_names)
    AND NOT EXISTS (SELECT 1 FROM pools o WHERE (o.tenant_id = lux_tenant() OR o.tenant_id IS NULL) AND o.name = $1 AND NOT o.retired)
    AND NOT EXISTS (SELECT 1 FROM hosts h WHERE (h.tenant_id = lux_tenant() OR h.tenant_id IS NULL) AND h.pool = $1 AND h.state <> 'terminated')
    AND NOT EXISTS (SELECT 1 FROM host_tokens k WHERE (k.tenant_id = lux_tenant() OR k.tenant_id IS NULL) AND k.pool = $1 AND k.revoked_at IS NULL)
  ORDER BY p.tenant_id NULLS LAST, p.renamed_at DESC NULLS LAST
  LIMIT 1
$$;
REVOKE ALL ON FUNCTION lux_pool_renamed_to(text) FROM PUBLIC;
