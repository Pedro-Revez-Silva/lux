-- 077_host_disk_pressure.sql — a host's disk pressure (docs/design/sizes.md, phase 5).
--
-- hosts.disk_pressure: the runner's latest heartbeat said its
-- low-free-space guard holds the host under disk pressure (proto.DiskPressure:
-- free, low and recovery marks). While it is set the scheduler places
-- nothing on the host. NULL otherwise, for every host before this migration,
-- and for any heartbeat without it (an older runner).
--
-- Nullable column without a default: catalog-only, no table rewrite.
SET LOCAL lock_timeout = '5s';
ALTER TABLE hosts ADD COLUMN disk_pressure jsonb;
