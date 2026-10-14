-- 076_run_size_raise.sql — why a Run's size is larger than its resume asked (docs/design/sizes.md, phase 4).
--
-- runs.size_raised_from, size_raised_for and size_raised_to: set when a
-- resume in a shaped pool asked for a size (size_raised_from: the one its
-- request resolved to) whose disk could not hold the Run's saved state,
-- and luxd gave it the smallest larger size that could (size_raised_to,
-- size_raised_for 'saved state'). All NULL otherwise, every Run before
-- this migration. Rewritten by every resume that sizes the Run, cleared
-- with resolved_size. A raise is shown only while size_raised_to equals
-- resolved_size: a luxd without these columns rewrites resolved_size alone.
--
-- Nullable columns without defaults: catalog-only, no table rewrite.
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs
  ADD COLUMN size_raised_from text,
  ADD COLUMN size_raised_for  text,
  ADD COLUMN size_raised_to   text;
