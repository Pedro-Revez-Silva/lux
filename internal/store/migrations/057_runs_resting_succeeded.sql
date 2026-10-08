-- 057_runs_resting_succeeded.sql — storage follows resumability: a succeeded
-- Run is resumable, so it rests (and expires) like a stopped, lost or
-- failed one, and only a terminated Run's blobs go after retention (from
-- terminated_at, added and backfilled by 056).
--
-- reapExpiry's scan: each tenant's resting Runs, oldest first, succeeded
-- among them. Built under the migration's transaction, as 053 built it.
DROP INDEX runs_resting;
CREATE INDEX runs_resting ON runs (tenant_id, state_changed_at) WHERE state IN ('stopped', 'lost', 'failed', 'succeeded');
