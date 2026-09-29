-- 035_snapshot_refused.sql — a placement whose snapshot report luxd refused
-- (it did not match its Run's blob records). Its Run keeps its previous
-- snapshot, so when the placement's exit is recorded the Run is not resumed
-- automatically and its state_reason says why.
ALTER TABLE placements ADD COLUMN snapshot_refused boolean NOT NULL DEFAULT false;

-- The snapshot whose report recorded an output or artifact, so that a
-- redelivered report is compared with that report's own records rather than
-- the whole placement's. NULL for volume blobs (a snapshot's manifest lists
-- them) and for rows recorded before this column.
ALTER TABLE blobs ADD COLUMN snapshot_id text;
ALTER TABLE artifacts ADD COLUMN snapshot_id text;
-- False for snapshots recorded before the columns above: their output and
-- artifact rows carry no snapshot_id to compare a redelivery with.
ALTER TABLE snapshots ADD COLUMN owns_records boolean NOT NULL DEFAULT false;
