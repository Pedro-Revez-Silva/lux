-- 071_pool_sizes.sql — fractional pool sizes (docs/design/sizes.md, phase 2).
--
-- pools.shape: the operator-declared nominal host shape (family, arch, cpus,
-- memory, disk, minCpus, defaultCpus) a shaped pool's size ladder derives
-- from; NULL for an unshaped pool, every pool before this migration.
--
-- runs.requested_resources: what a submit to a shaped pool asked for in
-- resources ({size?, cpus?, memory?, disk?}, only the fields given), kept
-- for idempotent retries and explanation; runs.resolved_size: the ladder
-- entry the Run has now. Both NULL for a Run submitted to an unshaped pool,
-- every Run before this migration. spec.resources stays the resolved
-- reservation the scheduler and costs read.
--
-- Nullable columns without defaults: catalog-only, no table rewrite. The
-- ALTERs still take ACCESS EXCLUSIVE; lock_timeout fails the migration
-- (store.migrate runs each file in one transaction, so it rolls back and can
-- be retried) instead of queueing every later reader of runs behind a long
-- transaction.
SET LOCAL lock_timeout = '5s';
ALTER TABLE pools ADD COLUMN shape jsonb;
ALTER TABLE runs
  ADD COLUMN requested_resources jsonb,
  ADD COLUMN resolved_size text;
