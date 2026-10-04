-- 054_snapshot_cleanup.sql — the reaper's storage passes, index-backed.
--
-- snapshots_superseded: the Run may have an available snapshot other than
-- its current one, for reapSuperseded to delete. Set when a snapshot stops
-- being current or arrives late (insertSnapshot, resume --from-snapshot);
-- cleared by the reaper once none is left. Only a hint for the scan: the
-- reaper re-checks everything under the Run's lock, and a missed flag only
-- keeps a snapshot longer.
ALTER TABLE runs ADD COLUMN snapshots_superseded boolean NOT NULL DEFAULT false;
UPDATE runs r SET snapshots_superseded = true
WHERE EXISTS (SELECT 1 FROM snapshots s WHERE s.run_id = r.id AND s.available AND s.id IS DISTINCT FROM r.snapshot_id);
CREATE INDEX runs_snapshots_superseded ON runs (id) WHERE snapshots_superseded;

-- reapRetention's per-Run test, "has a blob it may delete": only blobs in
-- S3 other than artifacts, which retention keeps.
CREATE INDEX blobs_retainable ON blobs (run_id) WHERE location = 's3' AND kind <> 'artifact';
