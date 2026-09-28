-- Host-hour refresh advances through retained hours and retries open hours.
CREATE TABLE cost_host_refresh (
  host_id text PRIMARY KEY REFERENCES hosts(id),
  next_hour timestamptz NOT NULL,
  retry_at timestamptz
);
ALTER TABLE cost_host_refresh ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_refresh USING (lux_system()) WITH CHECK (lux_system());

-- A single-slot batch alternates current-hour work with older host hours.
CREATE TABLE cost_host_turn (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  current_first boolean NOT NULL DEFAULT true
);
INSERT INTO cost_host_turn (id) VALUES (true);
ALTER TABLE cost_host_turn ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_turn USING (lux_system()) WITH CHECK (lux_system());
