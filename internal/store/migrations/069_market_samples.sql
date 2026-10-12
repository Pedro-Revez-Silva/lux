-- 069_market_samples.sql — what candidate instance types cost and how
-- likely spot capacity is for them, per availability zone of each pool,
-- sampled by one luxd (docs/telemetry.md, "Market data"). Each figure is
-- NULL on its own when its call failed. zone_id is the account-independent
-- zone id (use1-az1) placement scores are keyed by. Kept as long as hourly
-- history (LUX_HISTORY_HOURS).
CREATE TABLE market_samples (
  provider           text NOT NULL,
  region             text NOT NULL,
  zone               text NOT NULL,
  zone_id            text,
  instance_type      text NOT NULL,
  sampled_at         timestamptz NOT NULL,
  spot_per_hour      numeric(24, 9) CHECK (spot_per_hour >= 0),
  on_demand_per_hour numeric(24, 9) CHECK (on_demand_per_hour >= 0),
  currency           text,
  placement_score    smallint CHECK (placement_score BETWEEN 1 AND 10),
  PRIMARY KEY (provider, region, instance_type, zone, sampled_at)
);
-- Retention deletes by age.
CREATE INDEX market_samples_sampled_at ON market_samples (sampled_at);
ALTER TABLE market_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON market_samples USING (lux_system()) WITH CHECK (lux_system());
