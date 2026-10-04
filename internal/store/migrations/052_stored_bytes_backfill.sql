-- 052_stored_bytes_backfill.sql — the stored bytes of the system samples
-- written before 051, from the blobs' history: a blob is in S3 from its
-- upload (uploaded_at) until its deletion (deleted_at), so a sample at `at`
-- holds the blobs with uploaded_at <= at AND (deleted_at IS NULL OR
-- deleted_at > at), its tenant's (all of them for tenant ''). A rollup row
-- gets the level at its bucket's start.
--
-- One pass, not a join of samples by blobs: each upload is +size and each
-- deletion -size, and a sample is the running sum of the events up to it
-- (events sort before a sample at the same instant: `<=`).
-- A blob in S3 without uploaded_at (none expected) counts from creation.
WITH b AS (
  SELECT tenant_id, kind, size, coalesce(uploaded_at, created_at) AS up, deleted_at
  FROM blobs WHERE uploaded_at IS NOT NULL OR location = 's3'
),
ev AS (
  SELECT tenant_id, kind, up AS t, size AS d FROM b
  UNION ALL SELECT tenant_id, kind, deleted_at, -size FROM b WHERE deleted_at IS NOT NULL
),
-- Each event once for its tenant and once for the whole system ('').
keyed AS (
  SELECT k.id, ev.t, false AS sample, NULL::int AS res,
    CASE WHEN ev.kind = 'volume' THEN ev.d ELSE 0 END AS volume,
    CASE WHEN ev.kind = 'output' THEN ev.d ELSE 0 END AS output,
    CASE WHEN ev.kind = 'artifact' THEN ev.d ELSE 0 END AS artifact,
    CASE WHEN ev.kind = 'context' THEN ev.d ELSE 0 END AS context
  FROM ev CROSS JOIN LATERAL (VALUES (ev.tenant_id), ('')) k(id)
  UNION ALL
  SELECT tenant_id, at, true, res, 0, 0, 0, 0 FROM system_samples
),
running AS (
  SELECT id, t, sample, res,
    sum(volume) OVER w AS volume, sum(output) OVER w AS output,
    sum(artifact) OVER w AS artifact, sum(context) OVER w AS context
  FROM keyed
  WINDOW w AS (PARTITION BY id ORDER BY t, sample ROWS UNBOUNDED PRECEDING)
)
UPDATE system_samples s SET stored_volume = r.volume, stored_output = r.output,
  stored_artifact = r.artifact, stored_context = r.context
FROM running r
WHERE r.sample AND s.tenant_id = r.id AND s.res = r.res AND s.at = r.t
  -- Only rows with bytes to fill: a row with none is left as it is.
  AND (r.volume, r.output, r.artifact, r.context) <> (0, 0, 0, 0)
  AND (s.stored_volume, s.stored_output, s.stored_artifact, s.stored_context) = (0, 0, 0, 0);
