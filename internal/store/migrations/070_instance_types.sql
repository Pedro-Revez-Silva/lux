-- 070_instance_types.sql — what each candidate instance type is, from the
-- provider (EC2: DescribeInstanceTypes), cached by the market sampler and
-- refreshed daily (docs/telemetry.md, "Market data"). NULL: the provider
-- did not say. Rows are replaced, never deleted: a type is fixed.
CREATE TABLE instance_types (
  provider              text NOT NULL,
  region                text NOT NULL,
  instance_type         text NOT NULL,
  vcpus                 int,
  memory_bytes          bigint,
  architectures         text[],
  nested_virtualization boolean,
  burstable             boolean,
  network_baseline_gbps float8,
  network_performance   text,
  fetched_at            timestamptz NOT NULL,
  PRIMARY KEY (provider, region, instance_type)
);
ALTER TABLE instance_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON instance_types USING (lux_system()) WITH CHECK (lux_system());
