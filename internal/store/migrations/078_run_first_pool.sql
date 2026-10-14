-- 078_run_first_pool.sql — the pool a Run first started in (docs/telemetry.md, pools over time).
--
-- runs.first_pool_id: set once, with first_started_at, by the first
-- running report (applyStatusOnly in internal/server/lifecycle.go): the
-- pool of the host it ran on, else runs.pool_id. Never rewritten by a
-- later resume or start; readers count a first start under it without
-- reading placements per Run. NULL for a Run never started, and where 079
-- left it (see 079).
--
-- The column only, without a default: a catalog change, no rewrite and no
-- scan. ADD COLUMN takes ACCESS EXCLUSIVE on runs and the foreign key
-- SHARE ROW EXCLUSIVE on pools until this transaction commits, at once.
-- Its wait for lock_timeout and what queues meanwhile: docs/operations.md
-- (upgrading, step 2).
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs ADD COLUMN first_pool_id text REFERENCES pools(id);
