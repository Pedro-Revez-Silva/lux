-- Missing host-hour intervals remain visible while rates are pending or absent.
CREATE TABLE cost_host_hour_gaps (
  host_id text NOT NULL REFERENCES hosts(id),
  hour timestamptz NOT NULL,
  missing_from timestamptz NOT NULL,
  missing_to timestamptz NOT NULL,
  reason text NOT NULL CHECK (reason IN ('static_unpriced', 'provider_pre_registration', 'rate_pending')),
  status text NOT NULL DEFAULT 'incomplete' CHECK (status = 'incomplete'),
  retry_at timestamptz,
  PRIMARY KEY (host_id, hour, missing_from),
  CHECK (missing_from < missing_to AND missing_from >= hour AND missing_to <= hour + interval '1 hour'),
  CHECK ((reason = 'rate_pending') = (retry_at IS NOT NULL))
);
CREATE INDEX cost_host_hour_gaps_hour ON cost_host_hour_gaps (hour);
CREATE INDEX cost_host_hour_gaps_retry ON cost_host_hour_gaps (retry_at) WHERE retry_at IS NOT NULL;
ALTER TABLE cost_host_hour_gaps ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON cost_host_hour_gaps USING (lux_system()) WITH CHECK (lux_system());
