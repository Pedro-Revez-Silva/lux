-- Missing host-hour intervals remain visible when the cursor advances past time that cannot be priced.
CREATE TABLE cost_host_hour_gaps (
  host_id text NOT NULL REFERENCES hosts(id),
  hour timestamptz NOT NULL,
  missing_from timestamptz NOT NULL,
  missing_to timestamptz NOT NULL,
  reason text NOT NULL CHECK (reason IN ('static_unpriced', 'provider_pre_registration')),
  status text NOT NULL DEFAULT 'incomplete' CHECK (status = 'incomplete'),
  PRIMARY KEY (host_id, hour, missing_from),
  CHECK (missing_from < missing_to AND missing_from >= hour AND missing_to <= hour + interval '1 hour')
);
CREATE INDEX cost_host_hour_gaps_hour ON cost_host_hour_gaps (hour);
ALTER TABLE cost_host_hour_gaps ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_hour_gaps USING (lux_system()) WITH CHECK (lux_system());
