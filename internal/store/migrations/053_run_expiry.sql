-- 053_run_expiry.sql — a resting Run (stopped, lost, failed) expires: once
-- it has rested longer than its tenant's expire_after_days, the reaper
-- cancels it (reapExpiry).
--
-- state_changed_at: when the Run entered its current state. Set by every
-- state change (setRunState, requestResume, assign); updated_at is not the
-- clock, as other writes move it.
ALTER TABLE runs ADD COLUMN state_changed_at timestamptz;

-- Existing Runs: their latest state event, else updated_at.
UPDATE runs r SET state_changed_at = coalesce(
	(SELECT e.created_at FROM run_events e WHERE e.run_id = r.id AND e.type = 'state' ORDER BY e.id DESC LIMIT 1),
	r.updated_at);

ALTER TABLE runs ALTER COLUMN state_changed_at SET DEFAULT now(),
	ALTER COLUMN state_changed_at SET NOT NULL;

-- Days a Run may rest before it is cancelled; 0: never.
ALTER TABLE tenants ADD COLUMN expire_after_days int NOT NULL DEFAULT 90
	CHECK (expire_after_days >= 0);

-- reapExpiry's scan: each tenant's resting Runs, oldest first.
CREATE INDEX runs_resting ON runs (tenant_id, state_changed_at) WHERE state IN ('stopped', 'lost', 'failed');
