-- A successful bounded history fetch after settlement is required before
-- finalizing compute on a terminated spot host. Platform data: system only.
CREATE TABLE spot_history_refresh (
  host_id text PRIMARY KEY REFERENCES hosts(id),
  checked_at timestamptz NOT NULL
);
ALTER TABLE spot_history_refresh ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON spot_history_refresh USING (lux_system()) WITH CHECK (lux_system());
