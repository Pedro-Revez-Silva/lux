-- 056_run_terminated.sql — the irreversible end of a Run is terminated,
-- no longer cancelled; the request is terminate, no longer cancel; a
-- terminated Run records when it became so (terminated_at).
--
-- Every statement rewrites rows of Runs that already ended (or are being
-- stopped for good), which luxd does not otherwise write, so the row locks
-- taken wait on nothing. Each table is scanned once: runs, placements,
-- cost_pending (plus its CHECK's validation), run_events, host_events and
-- run_servers. The RENAME comes first and holds ACCESS EXCLUSIVE on
-- runs until commit, so no luxd writes a cancelled row behind the rewrite;
-- a luxd older than this migration reads and writes cancel_requested and
-- fails once it is renamed: every luxd sharing the database stops before
-- it runs and moves to the new release together (docs/operations.md).

ALTER TABLE runs RENAME COLUMN cancel_requested TO terminate_requested;

-- terminated_at: retention's clock (reapRetention). finished_at stays when
-- the last placement ended, so a Run terminated long after it finished
-- keeps its tenant's whole retention from the terminate.
ALTER TABLE runs ADD COLUMN terminated_at timestamptz;

-- One rewrite per row. Right-hand sides read the row as it was. state_reason
-- is display text written by luxd: the forms it wrote for a cancel, and an
-- expiry's "expired: …" (which needs no change). terminated_at of a
-- cancelled Run: its state_changed_at (053, NOT NULL), when it became
-- cancelled, as it has not changed state since.
UPDATE runs r SET
	state = CASE WHEN r.state = 'cancelled' THEN 'terminated' ELSE r.state END,
	state_reason = CASE
		WHEN r.state_reason = 'cancelled' OR (r.state_reason = 'cancel' AND r.state = 'cancelled') THEN 'terminated'
		WHEN r.state_reason = 'cancel' THEN 'terminate'
		WHEN r.state_reason LIKE 'cancelled;%' THEN 'terminated' || substr(r.state_reason, length('cancelled') + 1)
		ELSE r.state_reason END,
	terminated_at = CASE WHEN r.state = 'cancelled' THEN r.state_changed_at END
	WHERE r.state = 'cancelled' OR r.state_reason IN ('cancel', 'cancelled') OR r.state_reason LIKE 'cancelled;%';

UPDATE placements SET stop_reason = 'terminate' WHERE stop_reason = 'cancel';

-- The cost queue's reasons: a row queued by a cancel, then the CHECK.
ALTER TABLE cost_pending DROP CONSTRAINT cost_pending_reason_check;
UPDATE cost_pending SET reason = 'state:terminated' WHERE reason = 'state:cancelled';
ALTER TABLE cost_pending ADD CONSTRAINT cost_pending_reason_check CHECK (reason IN ('tick', 'settle', 'retry',
	'state:stopping', 'state:stopped', 'state:lost', 'state:succeeded', 'state:failed', 'state:terminated', 'state:resuming'));

-- Events read back by clients and the console, in one rewrite: the state
-- events (state and reason), the request (cancel.requested), and what a
-- Run's end did to its servers.
UPDATE run_events SET
	type = CASE WHEN type = 'cancel.requested' THEN 'terminate.requested' ELSE type END,
	data = CASE
		WHEN type = 'state' THEN data
			|| CASE WHEN data->>'state' = 'cancelled' THEN '{"state": "terminated"}'::jsonb ELSE '{}'::jsonb END
			|| CASE
				WHEN data->>'reason' = 'cancel' AND data->>'state' = 'stopping' THEN '{"reason": "terminate"}'::jsonb
				WHEN data->>'reason' IN ('cancel', 'cancelled') THEN '{"reason": "terminated"}'::jsonb
				WHEN data->>'reason' LIKE 'cancelled;%' THEN jsonb_build_object('reason', 'terminated' || substr(data->>'reason', length('cancelled') + 1))
				ELSE '{}'::jsonb END
		WHEN type IN ('server.deleted', 'server.detached') THEN data || '{"reason": "run terminated"}'::jsonb
		ELSE data END
	WHERE type = 'cancel.requested'
		OR (type = 'state' AND (data->>'state' = 'cancelled' OR data->>'reason' IN ('cancel', 'cancelled') OR data->>'reason' LIKE 'cancelled;%'))
		OR (type IN ('server.deleted', 'server.detached') AND data->>'reason' = 'run cancelled');

UPDATE host_events SET data = data
		|| CASE WHEN data->>'outcome' = 'cancelled' THEN '{"outcome": "terminated"}'::jsonb ELSE '{}'::jsonb END
		|| CASE WHEN data->>'stopReason' = 'cancel' THEN '{"stopReason": "terminate"}'::jsonb ELSE '{}'::jsonb END
	WHERE type = 'host.placement_ended' AND (data->>'outcome' = 'cancelled' OR data->>'stopReason' = 'cancel');

-- Servers of an ended Run were detached with this reason as their stop.
UPDATE run_servers SET stop_reason = 'run terminated' WHERE stop_reason = 'run cancelled';
