-- 037_snapshot_records.sql — associate output and artifact rows with the
-- snapshot that reported them for redelivery comparison. Volume blobs are
-- identified by the manifest; earlier rows cannot be assigned an owner.
ALTER TABLE blobs ADD COLUMN snapshot_id text;
ALTER TABLE artifacts ADD COLUMN snapshot_id text;
ALTER TABLE snapshots ADD COLUMN owns_records boolean NOT NULL DEFAULT false;
