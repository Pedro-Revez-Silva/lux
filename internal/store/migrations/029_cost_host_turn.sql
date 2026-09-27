CREATE TABLE cost_host_turn (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  current_first boolean NOT NULL DEFAULT true
);
INSERT INTO cost_host_turn (id) VALUES (true);
ALTER TABLE cost_host_turn ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_turn USING (lux_system()) WITH CHECK (lux_system());
