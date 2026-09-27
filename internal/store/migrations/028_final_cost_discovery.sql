-- An upgrade from an already installed 027 also needs the ordered cursor index.
CREATE INDEX IF NOT EXISTS cost_final_hour_backfill_next ON cost_final_hour_backfill (next_hour, run_id, source);
CREATE TABLE cost_final_hour_discovery (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  run_id text NOT NULL DEFAULT '',
  source text NOT NULL DEFAULT '',
  completed boolean NOT NULL DEFAULT false
);
INSERT INTO cost_final_hour_discovery (id) VALUES (true);
ALTER TABLE cost_final_hour_discovery ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_final_hour_discovery USING (lux_system()) WITH CHECK (lux_system());
