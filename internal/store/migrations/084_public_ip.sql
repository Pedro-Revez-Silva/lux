-- 084_public_ip.sql — a host's public IPv4 addresses as a host-tied cost
-- family (docs/costs.md, section 2, "Public IPv4"): 'public-ip' periods in
-- host_rates (per_hour = the hourly charge × the addresses), shared between
-- the host's placements like compute and block storage, the rest
-- unallocated.
ALTER TABLE host_rates DROP CONSTRAINT host_rates_family_check;
ALTER TABLE host_rates ADD CONSTRAINT host_rates_family_check
  CHECK (family IN ('compute', 'block-storage', 'public-ip'));
ALTER TABLE cost_placement_snapshots DROP CONSTRAINT cost_placement_snapshots_family_check;
ALTER TABLE cost_placement_snapshots ADD CONSTRAINT cost_placement_snapshots_family_check
  CHECK (family IN ('compute', 'block-storage', 'public-ip'));
-- Host rows: the shared families' allocated and unallocated, and the
-- host's own transfer (unallocated: no Run sent it).
ALTER TABLE cost_hourly DROP CONSTRAINT cost_hourly_check;
ALTER TABLE cost_hourly ADD CONSTRAINT cost_hourly_check
  CHECK ((run_id IS NOT NULL AND tenant_id IS NOT NULL AND allocated = 0 AND unallocated = 0)
      OR (run_id IS NULL AND tenant_id IS NULL AND host_id IS NOT NULL AND source = 'compute'
          AND family IN ('compute', 'block-storage', 'public-ip', 'data-transfer') AND amount = 0));
-- When a provider host's addresses were first recorded after it registered
-- (a provider check that saw them late, or 0 that became n): its first
-- public-ip period opens then, not at registration. NULL: known by
-- registration, or none yet.
ALTER TABLE hosts ADD COLUMN public_ip_from timestamptz;
