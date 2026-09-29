-- 035_snapshot_refused.sql — a placement whose snapshot report luxd refused
-- (it did not match its Run's blob records). Its Run keeps its previous
-- snapshot, so when the placement's exit is recorded the Run is not resumed
-- automatically and its state_reason says why.
ALTER TABLE placements ADD COLUMN snapshot_refused boolean NOT NULL DEFAULT false;
