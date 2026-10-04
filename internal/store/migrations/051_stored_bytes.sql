-- 051_stored_bytes.sql — the bytes lux keeps in S3, per blob kind, in
-- every system sample: the sum of blobs.size (compressed, what S3 bills)
-- with location 's3', per tenant and for the whole system (tenant '').
ALTER TABLE system_samples
  ADD COLUMN stored_volume   bigint NOT NULL DEFAULT 0,
  ADD COLUMN stored_output   bigint NOT NULL DEFAULT 0,
  ADD COLUMN stored_artifact bigint NOT NULL DEFAULT 0,
  ADD COLUMN stored_context  bigint NOT NULL DEFAULT 0;

-- sampleSystem sums it every tick: an index-only scan grouped by tenant
-- and kind. blobs_tenant_stored has no kind and counts 'host' blobs.
CREATE INDEX blobs_s3_tenant_kind ON blobs (tenant_id, kind) INCLUDE (size) WHERE location = 's3';
