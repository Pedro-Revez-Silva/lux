-- 016_control_samples.sql — the control host: the machine luxd runs on, and
-- its Postgres. Written with the whole system's sample (same `at`), at the
-- same three resolutions, rolled up and expired with it. Operators only.
-- `instance` is the sampling luxd's hostname: several luxd instances on one
-- database each record their own machine, and CPU counters are only
-- comparable within one instance.

CREATE TABLE control_samples (
  instance       text NOT NULL,
  res            int NOT NULL,
  at             timestamptz NOT NULL,
  cpu_seconds    float8,           -- counter: busy CPU seconds since boot, all cores
  cpus           int,
  mem_bytes      bigint,
  mem_total      bigint,
  db_bytes       bigint,           -- pg_database_size of lux's database
  db_connections int,              -- backends connected to it
  PRIMARY KEY (res, instance, at)
);

-- One row per tracked directory (luxd's history.disk_paths) per sample.
CREATE TABLE control_disk_samples (
  instance    text NOT NULL,
  path        text NOT NULL,
  res         int NOT NULL,
  at          timestamptz NOT NULL,
  used_bytes  bigint NOT NULL,
  free_bytes  bigint NOT NULL,     -- writable by an unprivileged process
  total_bytes bigint NOT NULL,
  PRIMARY KEY (instance, path, res, at)
);

-- Neither primary key orders by (res, at): control_samples' puts instance
-- between them, the disk table's leads with instance and path. History's
-- newest-sample-across-instances lookup and retention's deletes by age
-- need a time-ordered path.
CREATE INDEX control_samples_res_at ON control_samples (res, at);
CREATE INDEX control_disk_samples_res_at ON control_disk_samples (res, at);

ALTER TABLE control_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON control_samples USING (lux_system()) WITH CHECK (lux_system());
ALTER TABLE control_disk_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON control_disk_samples USING (lux_system()) WITH CHECK (lux_system());
