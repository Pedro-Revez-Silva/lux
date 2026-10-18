ALTER TABLE blobs DROP CONSTRAINT blobs_kind_check;
ALTER TABLE blobs ADD CONSTRAINT blobs_kind_check CHECK (kind IN ('volume', 'output', 'artifact', 'context', 'checkpoint'));

CREATE TABLE git_checkpoints (
  id text PRIMARY KEY REFERENCES blobs(id),
  tenant_id text NOT NULL REFERENCES tenants(id),
  run_id text NOT NULL REFERENCES runs(id),
  repo text NOT NULL,
  head text NOT NULL,
  branch text NOT NULL DEFAULT '',
  refs jsonb NOT NULL,
  size bigint NOT NULL,
  epoch int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  uploaded boolean NOT NULL DEFAULT false,
  latest boolean NOT NULL DEFAULT false,
  UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX git_checkpoints_latest ON git_checkpoints (run_id, repo) WHERE latest;
CREATE TABLE git_checkpoint_refs (
  tenant_id text NOT NULL REFERENCES tenants(id),
  run_id text NOT NULL REFERENCES runs(id),
  repo text NOT NULL,
  checkpoint_id text NOT NULL,
  PRIMARY KEY (run_id, repo),
  FOREIGN KEY (tenant_id, checkpoint_id) REFERENCES git_checkpoints(tenant_id, id)
);
CREATE INDEX git_checkpoint_refs_checkpoint ON git_checkpoint_refs(checkpoint_id);
ALTER TABLE git_checkpoints ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON git_checkpoints USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
ALTER TABLE git_checkpoint_refs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON git_checkpoint_refs USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
