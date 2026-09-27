-- Final reports predating cost_hourly are copied incrementally, without
-- asking their sources to report again. Runtime discovery seeds these cursors.
CREATE TABLE cost_final_hour_backfill (
  run_id text NOT NULL REFERENCES runs(id),
  source text NOT NULL,
  next_hour timestamptz NOT NULL,
  PRIMARY KEY (run_id, source)
);
ALTER TABLE cost_final_hour_backfill ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_final_hour_backfill USING (lux_system()) WITH CHECK (lux_system());
CREATE INDEX cost_final_hour_backfill_next ON cost_final_hour_backfill (next_hour, run_id, source);
