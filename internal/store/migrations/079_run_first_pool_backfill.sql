-- 079_run_first_pool_backfill.sql — runs.first_pool_id (078) for the Runs
-- that ended before it, and its index.
--
-- The rule is a frozen copy of runStartPool's lookup in
-- internal/server/history.go: the placement whose started_at is the Run's
-- first_started_at, else its earliest started one; its host's pool. Left
-- NULL where it cannot be told (no started placement, or its host has no
-- pool or is gone).
--
-- Only ended Runs (succeeded, failed, terminated: endedRunStates in
-- internal/server/lifecycle.go); live and resting Runs stay NULL and the
-- readers resolve them with the same lookup. The UPDATE takes ROW
-- EXCLUSIVE on runs (no read, submit or write to a single live Run is
-- blocked) and holds the ended Runs' row locks until commit; pools only
-- get the foreign-key checks' KEY SHARE row locks. Its duration, what
-- waits on it (through writeComputeChunk in internal/server/costqueue.go,
-- also live Runs' reports and their hosts' heartbeats) and the procedure
-- above 300,000 ended Runs: docs/operations.md (upgrading, step 2).
--
-- Then runs_first_pool_started, built as 041 built runs_pool_started: a
-- plain CREATE INDEX, whose SHARE lock on runs holds back writes to runs
-- (reads go on) for the build (its duration: docs/operations.md, upgrading,
-- step 2) until this transaction commits right after.
-- It waits for writes in progress first, up to lock_timeout.
SET LOCAL lock_timeout = '5s';

UPDATE runs r SET first_pool_id = x.pool_id
FROM (SELECT s.id, (SELECT h.pool_id FROM (SELECT pl.host_id FROM placements pl
			WHERE pl.run_id = s.id AND pl.started_at IS NOT NULL
			ORDER BY pl.started_at IS DISTINCT FROM s.first_started_at, pl.started_at, pl.epoch LIMIT 1) pl
		JOIN hosts h ON h.id = pl.host_id) AS pool_id
	FROM runs s WHERE s.first_started_at IS NOT NULL AND s.state IN ('succeeded', 'failed', 'terminated')) x
WHERE x.id = r.id AND x.pool_id IS NOT NULL;

-- A pool's Runs first started in a range.
CREATE INDEX runs_first_pool_started ON runs (first_pool_id, first_started_at) WHERE first_started_at IS NOT NULL;
