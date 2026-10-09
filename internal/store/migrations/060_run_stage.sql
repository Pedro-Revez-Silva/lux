-- 060_run_stage.sql — a Run's stage (GET /v1/runs/{id} stage, stageSince).
--
-- repos_ready_at: the runner's reposReady mark, the end of its clones and a
-- resume's sync fetches (at once for a spec without repositories); the
-- start of the container stage.
ALTER TABLE placements ADD COLUMN repos_ready_at timestamptz;

-- stage_announced: the stage the latest stage event announced
-- ({stage, since, reason}), so a report that changes nothing announces
-- nothing. NULL until the first announcement: a Run from before this
-- migration announces its stage at its next change of any input.
ALTER TABLE runs ADD COLUMN stage_announced jsonb;

-- waiting_since: when a requeued Run began waiting for a host, for its
-- stage: the given-up placement's wait, which a requeue continues (its
-- assignment never reached the runner). NULL otherwise, the stage then
-- waiting since needs_host_since. needs_host_since itself restarts, as
-- placement time counts the given-up placement's wait already.
ALTER TABLE runs ADD COLUMN waiting_since timestamptz;
