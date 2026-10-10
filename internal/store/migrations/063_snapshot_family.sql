-- 063_snapshot_family.sql — a placement's cost snapshot per host-tied
-- family (docs/costs.md, section 2): 'compute' (every existing row) and
-- 'block-storage', each priced, frozen and never repriced on its own.
ALTER TABLE cost_placement_snapshots ADD COLUMN family text NOT NULL DEFAULT 'compute'
  CHECK (family IN ('compute', 'block-storage'));
ALTER TABLE cost_placement_snapshots DROP CONSTRAINT cost_placement_snapshots_pkey;
ALTER TABLE cost_placement_snapshots ADD PRIMARY KEY (placement_id, family);
