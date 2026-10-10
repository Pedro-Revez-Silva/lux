-- 061_host_volumes.sql — the block-storage volumes that live and die with a
-- provider host (docs/costs.md, section 3): only those deleted on the
-- instance's termination, as the provider's DescribeVolumes reports them, e.g.
-- [{"type":"gp3","sizeGiB":100,"iops":3000,"throughputMiBps":125}].
-- "assumed": true marks volumes an operator supplied for a host launched
-- before luxd recorded them (luxd admin costs backfill-volumes).
-- NULL: not known yet (the provider check fills it in; block storage is
-- missing until then, never zero).
ALTER TABLE hosts ADD COLUMN volumes jsonb CHECK (volumes IS NULL OR jsonb_typeof(volumes) = 'array');
