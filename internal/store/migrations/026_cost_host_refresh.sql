-- A durable per-host cursor lets hourly aggregation resume after downtime.
CREATE TABLE cost_host_refresh (
  host_id text PRIMARY KEY REFERENCES hosts(id),
  next_hour timestamptz NOT NULL,
  retry_at timestamptz
);
ALTER TABLE cost_host_refresh ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_refresh USING (lux_system()) WITH CHECK (lux_system());
