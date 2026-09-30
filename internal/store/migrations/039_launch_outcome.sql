-- 039_launch_outcome.sql — how a provisioned host's launch went, apart
-- from its operational state.
--
-- A host whose launch the provider refused is still operationally
-- `terminated` (it leaves every active host list; its one-use token is
-- revoked and its copies are gone, as before). launch_outcome records what
-- happened to the launch itself:
--
--   requested  luxd asked the provider; no answer recorded yet
--   launched   the provider started an instance (it may later never
--              register, be lost, or disappear: those stay reasons of
--              the terminated state)
--   failed     the provider refused: no instance ever existed
--   abandoned  luxd never recorded an answer and wrote the row off
--
-- NULL: a host that registered itself (no launch), or one from before this
-- migration whose launch could not be told apart.
ALTER TABLE hosts
  ADD COLUMN launch_outcome text CHECK (launch_outcome IN ('requested', 'launched', 'failed', 'abandoned')),
  ADD COLUMN launch_finished_at timestamptz,
  ADD COLUMN launch_error text;

-- History, only where it is unambiguous. A launch the provider refused
-- left a terminated row whose reason starts "launch failed: ", with no
-- instance id and no registration; its reason text stays as it is.
UPDATE hosts SET launch_outcome = 'failed', launch_finished_at = terminated_at,
    launch_error = substr(state_reason, length('launch failed: ') + 1)
  WHERE state = 'terminated' AND provision_requested_at IS NOT NULL
    AND state_reason LIKE 'launch failed: %' AND provider_id IS NULL AND registered_at IS NULL;
-- A provisioned host with an instance id was launched.
UPDATE hosts SET launch_outcome = 'launched'
  WHERE launch_outcome IS NULL AND provision_requested_at IS NOT NULL AND provider_id IS NOT NULL;
-- A launch in flight now.
UPDATE hosts SET launch_outcome = 'requested'
  WHERE launch_outcome IS NULL AND provision_requested_at IS NOT NULL AND provider_id IS NULL AND state <> 'terminated';

-- Placement time (GET /v1/runs placementSeconds): when a Run started
-- needing a host. Set when it is submitted and each time it is queued
-- again (resume, migration, auto-resume); a placement copies it when it is
-- assigned. Rows from before this migration have none: the Run's creation
-- (first placement) or the previous placement's end stands in.
ALTER TABLE runs ADD COLUMN needs_host_since timestamptz;
UPDATE runs r SET needs_host_since = coalesce((SELECT max(p.ended_at) FROM placements p WHERE p.run_id = r.id), r.created_at)
  WHERE r.state IN ('submitted', 'resuming', 'provisioning');
ALTER TABLE placements ADD COLUMN needed_since timestamptz;

-- Paged lists in their default order (created, newest first; the id breaks
-- ties) without a full scan.
CREATE INDEX runs_created_id ON runs (created_at DESC, id DESC);
CREATE INDEX hosts_created_id ON hosts (created_at DESC, id DESC);
