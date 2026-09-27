-- On-demand list prices shared by hosts of the same provider/region/type/OS.
-- Only luxd's system role may read or refresh this cache.
CREATE TABLE price_cache (
  provider      text NOT NULL,
  region        text NOT NULL,
  instance_type text NOT NULL,
  os            text NOT NULL,
  per_hour      numeric(24, 9) NOT NULL CHECK (per_hour >= 0),
  currency      text NOT NULL,
  fetched_at    timestamptz NOT NULL,
  PRIMARY KEY (provider, region, instance_type, os)
);
ALTER TABLE price_cache ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON price_cache USING (lux_system()) WITH CHECK (lux_system());
