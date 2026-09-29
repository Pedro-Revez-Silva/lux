-- 033_snapshot_diffs.sql — what a Run changed in its repositories, as of
-- each snapshot: one row per repository and base kind (clone: from the
-- commit it was cloned at; head: from its HEAD then). The patch is a blob
-- (kind 'diff'), absent when there was no change or the diff failed.

ALTER TABLE blobs DROP CONSTRAINT blobs_kind_check;
ALTER TABLE blobs ADD CONSTRAINT blobs_kind_check CHECK (kind IN ('volume', 'output', 'artifact', 'context', 'diff'));

-- What a host's runner offers beyond the protocol version (its hello):
-- 'diff' (live diffs, and a snapshot's diffs).
ALTER TABLE hosts ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}';

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
  -- Changed paths with a clean/smudge filter attribute, compared raw.
  filters_ignored boolean NOT NULL DEFAULT false,
  filtered_paths  jsonb NOT NULL DEFAULT '[]',
  -- Changed paths whose bytes git converts (text, eol, autocrlf), and
  -- submodules with changes of their own: what the patch cannot carry.
  normalized_paths jsonb NOT NULL DEFAULT '[]',
  dirty_submodules jsonb NOT NULL DEFAULT '[]',
  error        text NOT NULL DEFAULT '',
  -- A known cause of error: base_unreachable.
  error_code   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (snapshot_id, repo, kind)
);
CREATE INDEX snapshot_diffs_run ON snapshot_diffs (run_id, epoch DESC);

ALTER TABLE snapshot_diffs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON snapshot_diffs USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());

-- Where each snapshot's diff is: pending (its snapshot is recorded, its
-- report not yet), complete (one row per expected repository and kind in
-- snapshot_diffs), skipped or failed (the report says why; or failed,
-- report_lost, when none came), unsupported (its runner computes none).
CREATE TABLE snapshot_diff_state (
  snapshot_id  text PRIMARY KEY REFERENCES snapshots(id),
  tenant_id    text NOT NULL REFERENCES tenants(id),
  run_id       text NOT NULL REFERENCES runs(id),
  state        text NOT NULL CHECK (state IN ('pending', 'complete', 'skipped', 'failed', 'unsupported')),
  reason       text NOT NULL DEFAULT '',
  -- The repositories whose diffs the report must carry (both kinds each).
  repos        text[] NOT NULL DEFAULT '{}',
  -- sha256 of the report applied: a redelivery must be the same report.
  report_sha256 text NOT NULL DEFAULT '',
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX snapshot_diff_state_pending ON snapshot_diff_state (updated_at) WHERE state = 'pending';

ALTER TABLE snapshot_diff_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON snapshot_diff_state USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
