-- 033_snapshot_diffs.sql — what a Run changed in its repositories, as of
-- each snapshot: one row per repository and base kind (clone: from the
-- commit it was cloned at; head: from its HEAD then). The patch is a blob
-- (kind 'diff'), absent when there was no change or the diff failed.

ALTER TABLE blobs DROP CONSTRAINT blobs_kind_check;
ALTER TABLE blobs ADD CONSTRAINT blobs_kind_check CHECK (kind IN ('volume', 'output', 'artifact', 'context', 'diff'));

CREATE TABLE snapshot_diffs (
  tenant_id    text NOT NULL REFERENCES tenants(id),
  run_id       text NOT NULL REFERENCES runs(id),
  snapshot_id  text NOT NULL REFERENCES snapshots(id),
  epoch        int NOT NULL,
  repo         text NOT NULL,
  kind         text NOT NULL CHECK (kind IN ('clone', 'head')),
  base         text NOT NULL DEFAULT '',
  head         text NOT NULL DEFAULT '',
  blob_id      text REFERENCES blobs(id),
  -- The patch's own size and sha256 (the blob is compressed).
  size         bigint NOT NULL DEFAULT 0,
  sha256       text NOT NULL DEFAULT '',
  truncated    boolean NOT NULL DEFAULT false,
  files        int NOT NULL DEFAULT 0,
  insertions   int NOT NULL DEFAULT 0,
  deletions    int NOT NULL DEFAULT 0,
  -- [{path, oldPath?, insertions, deletions, binary?}]
  file_stats   jsonb NOT NULL DEFAULT '[]',
  error        text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (snapshot_id, repo, kind)
);
CREATE INDEX snapshot_diffs_run ON snapshot_diffs (run_id, epoch DESC);

ALTER TABLE snapshot_diffs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON snapshot_diffs USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
