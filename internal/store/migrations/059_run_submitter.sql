-- 058_run_submitter.sql — who submitted a Run: the API key it came with, or
-- the person (Cloudflare Access email) when signed in without one. Runs from
-- before this migration keep both NULL.
--
-- submitted_by_key holds an api_keys id without a foreign key: keys are
-- revoked (revoked_at), never deleted, and readers LEFT JOIN api_keys, so an
-- id without a row reads as an unnamed key.
--
-- Nullable columns without defaults: catalog-only, no rewrite of runs.
ALTER TABLE runs
  ADD COLUMN submitted_by_key text,
  ADD COLUMN submitted_by_email text;
