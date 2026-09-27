-- Final reports predating cost_hourly are copied incrementally, without
-- asking their sources to report again. One cursor advances one hour at a time.
CREATE TABLE cost_final_hour_backfill (
  run_id text NOT NULL REFERENCES runs(id),
  source text NOT NULL,
  next_hour timestamptz NOT NULL,
  PRIMARY KEY (run_id, source)
);
ALTER TABLE cost_final_hour_backfill ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_final_hour_backfill USING (lux_system()) WITH CHECK (lux_system());
INSERT INTO cost_final_hour_backfill (run_id, source, next_hour)
SELECT c.run_id, c.source, date_trunc('hour', min(l.period_from))
FROM cost_sources c JOIN cost_lines l ON l.run_id = c.run_id AND l.source = c.source
WHERE c.status = 'final' AND l.final
GROUP BY c.run_id, c.source;
