-- 058_artifact_versions.sql — artifacts are versioned per (run_id, path):
-- a file published again under its name, or collected again with other
-- content, is the next version; none is replaced. Published artifacts
-- carry a description.
--
-- Rows recorded before are numbered in the order they were made. luxd
-- assigns max + 1 for the path while it holds the Run's row lock.
--
-- The index is on md5(path): a collected path can be up to PATH_MAX, past
-- a btree tuple's ~2704 bytes. Queries filter md5(path) = md5($n) AND
-- path = $n.
ALTER TABLE artifacts ADD COLUMN version int, ADD COLUMN description text NOT NULL DEFAULT '';

UPDATE artifacts a SET version = v.n
	FROM (SELECT id, row_number() OVER (PARTITION BY run_id, path ORDER BY epoch, created_at, id) AS n FROM artifacts) v
	WHERE a.id = v.id;

ALTER TABLE artifacts ALTER COLUMN version SET NOT NULL;
CREATE UNIQUE INDEX artifacts_run_path_version ON artifacts (run_id, md5(path), version);

-- artifact.published looks the artifact up by the blob that was uploaded.
CREATE INDEX artifacts_blob ON artifacts (blob_id);
