-- 072_host_disk_samples.sql — a host's filesystem on its history (docs/design/sizes.md, phase 3).
--
-- disk_free: bytes unprivileged writers can still use on the filesystem
-- holding the runner's data directory. fs_capacity and disk_headroom: what
-- a runner scaling disk (disk-scale) holds Runs to, read once at its
-- start; NULL from a runner that does not scale. All NULL for older rows
-- and runners.
--
-- Nullable columns without defaults: catalog-only, no table rewrite.
SET LOCAL lock_timeout = '5s';
ALTER TABLE host_samples
  ADD COLUMN disk_free     bigint,
  ADD COLUMN fs_capacity   bigint,
  ADD COLUMN disk_headroom bigint;
