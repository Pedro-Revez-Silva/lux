-- 075_pool_placement.sql — a pool's placement policy (docs/design/sizes.md, phase 4).
--
-- pools.placement: how the scheduler chooses among the hosts a Run fits:
-- 'spread' (fewest Runs first, every pool before this migration) or
-- 'pack' (the most allocated host first). NULL is the pool's default:
-- pack for a shaped pool, spread for an unshaped one.
--
-- A nullable column without a default: catalog-only, no table rewrite.
-- The CHECK is validated against existing rows, all NULL; pools is small.
SET LOCAL lock_timeout = '5s';
ALTER TABLE pools ADD COLUMN placement text CHECK (placement IN ('spread', 'pack'));
