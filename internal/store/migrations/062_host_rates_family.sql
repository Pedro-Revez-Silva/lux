-- 062_host_rates_family.sql — a host's rate periods per cost family
-- (docs/costs.md, sections 2 and 3): 'compute' (the machine, every existing
-- row) and 'block-storage' (the volumes deleted with it: one period over the
-- billed window). Each family has its own periods, at most one open at a
-- time. details records what a period was priced from (block storage: the
-- volumes and unit prices).
ALTER TABLE host_rates ADD COLUMN family text NOT NULL DEFAULT 'compute'
  CHECK (family IN ('compute', 'block-storage'));
ALTER TABLE host_rates ADD COLUMN details jsonb;
ALTER TABLE host_rates DROP CONSTRAINT host_rates_pkey;
ALTER TABLE host_rates ADD PRIMARY KEY (host_id, family, valid_from);
DROP INDEX host_rates_open;
CREATE UNIQUE INDEX host_rates_open ON host_rates (host_id, family) WHERE valid_to IS NULL;
