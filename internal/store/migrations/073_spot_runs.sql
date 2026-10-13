-- 073_spot_runs.sql — spot Runs (docs/design/spot-runs.md).
--
-- hosts.capacity_epoch: bumped on every runner hello (reconnect, restart,
-- capacity change), so neither aged capacity nor a pledge survives one.
--
-- host_capacity_age: per host, a bounded history of its free vector (cpus,
-- memory, disk, run slots) at each change, oldest first. One primary-key read
-- answers whether a vector has been continuously free for more than 60s.
-- key names the capacity_epoch, capacity, pool and pool shape the history
-- holds for; any luxd appends to it, and one reading another key starts over.
--
-- placements.reclaim_for: the normal Run a spot placement is being reclaimed
-- for; set once, never cleared (a signalled reclamation is irreversible).
--
-- spot_pledges: the resources a normal Run was promised on one host by
-- reclaiming spot placements there, until it is placed or leaves the queue.
SET LOCAL lock_timeout = '5s';
ALTER TABLE hosts ADD COLUMN capacity_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE placements ADD COLUMN reclaim_for text;
CREATE INDEX placements_reclaim_for ON placements (reclaim_for) WHERE reclaim_for IS NOT NULL;

CREATE TABLE host_capacity_age (
  host_id text PRIMARY KEY REFERENCES hosts (id) ON DELETE CASCADE,
  key     text NOT NULL,
  history jsonb NOT NULL
);
ALTER TABLE host_capacity_age ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON host_capacity_age USING (lux_system()) WITH CHECK (lux_system());

CREATE TABLE spot_pledges (
  run_id     text PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  host_id    text NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
  -- host_epoch: the host's capacity_epoch when pledged; a runner hello
  -- since (its spot placements may be gone) voids the pledge.
  host_epoch bigint NOT NULL,
  tenant_id  text NOT NULL,
  resources  jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX spot_pledges_host ON spot_pledges (host_id);
ALTER TABLE spot_pledges ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON spot_pledges USING (lux_system()) WITH CHECK (lux_system());
