-- 022_cost_queue.sql — the queue of Runs whose costs are due
-- (docs/costs.md, section 5). A Run is queued when it leaves running (by
-- setRunState, in the same transaction) and by the cost tick; any number
-- of triggers for one Run merge into its one row. Platform data: system
-- only.

CREATE TABLE cost_pending (
  run_id        text PRIMARY KEY REFERENCES runs(id),
  due_at        timestamptz NOT NULL,
  reason        text NOT NULL CHECK (reason IN ('tick', 'settle', 'retry', 'state:stopping', 'state:stopped',
                  'state:lost', 'state:succeeded', 'state:failed', 'state:cancelled', 'state:resuming')),
  claimed_by    text,                   -- the luxd working it
  claimed_until timestamptz             -- past: the claim is free to take again
);
CREATE INDEX cost_pending_due ON cost_pending (due_at);

-- One row per tick bucket: only the luxd whose insert lands runs that tick.
-- Rows older than a day are deleted by the tick.
CREATE TABLE cost_ticks (
  tick_at timestamptz PRIMARY KEY
);

ALTER TABLE cost_pending ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_pending USING (lux_system()) WITH CHECK (lux_system());
ALTER TABLE cost_ticks ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_ticks USING (lux_system()) WITH CHECK (lux_system());

-- setRunState also runs in a tenant's scope (a stop or cancel through the
-- API), where cost_pending is out of reach: it queues through this
-- function, which runs as the table's owner and queues only a Run the
-- caller's scope can see. A trigger arriving while a luxd works the Run
-- frees its claim: that luxd's result, read before the change, is dropped
-- (the drainer writes only under its own claim) and the Run is evaluated
-- again. Only lux_app may call it (granted with its other privileges, in
-- store.ensureAppRole: the role may not exist yet here), and only with a
-- reason the queue knows (the CHECK above).
CREATE FUNCTION lux_cost_enqueue(run text, why text) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp AS $$
  INSERT INTO cost_pending (run_id, due_at, reason)
    SELECT id, now(), why FROM runs WHERE id = run AND (tenant_id = lux_tenant() OR lux_system())
  ON CONFLICT (run_id) DO UPDATE SET due_at = least(cost_pending.due_at, EXCLUDED.due_at),
    reason = EXCLUDED.reason, claimed_by = NULL, claimed_until = NULL
$$;
REVOKE ALL ON FUNCTION lux_cost_enqueue(text, text) FROM PUBLIC;

-- The tick queues every live Run, and every Run whose source is owed
-- another attempt (next_at passed): both found by index, not a scan.
CREATE INDEX runs_live ON runs (id) WHERE state IN ('scheduled', 'starting', 'running', 'stopping');
CREATE INDEX cost_sources_next ON cost_sources (next_at) WHERE status <> 'final';
