-- 021_host_rates.sql — what each host costs per hour, as periods
-- (docs/costs.md, sections 2 and 3). A new period opens when the price or
-- the host's advertised capacity changes; the open one is closed at that
-- instant (valid_to set) and never updated again. Compute cost splits a
-- host's time at these boundaries. Platform data: system only.

CREATE TABLE host_rates (
  host_id      text NOT NULL REFERENCES hosts(id),
  valid_from   timestamptz NOT NULL,
  valid_to     timestamptz,             -- NULL: still current
  per_hour     numeric(24, 9) NOT NULL,
  currency     text NOT NULL,
  cap_cpus     float8 NOT NULL,         -- hosts.capacity at the time
  cap_memory   bigint NOT NULL,
  source       text NOT NULL,           -- 'aws-pricing', 'aws-spot-history', 'static', ...
  PRIMARY KEY (host_id, valid_from)
);
-- At most one open period per host.
CREATE UNIQUE INDEX host_rates_open ON host_rates (host_id) WHERE valid_to IS NULL;

ALTER TABLE host_rates ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON host_rates USING (lux_system()) WITH CHECK (lux_system());

-- A static host's flat hourly price: set per host (PUT /v1/hosts/{id}/price)
-- or copied from its pool's default when the host first registers. It
-- drives the host's 'static' periods above. NULL: no price, no period.
ALTER TABLE hosts ADD COLUMN hourly_price numeric(24, 9);
ALTER TABLE hosts ADD COLUMN price_currency text;
ALTER TABLE hosts ADD CONSTRAINT hosts_price
  CHECK ((hourly_price IS NULL) = (price_currency IS NULL) AND hourly_price >= 0);

-- A static pool's default for hosts registering into it. Changing it does
-- not reprice the pool's existing hosts.
ALTER TABLE pools ADD COLUMN hourly_price numeric(24, 9);
ALTER TABLE pools ADD COLUMN price_currency text;
ALTER TABLE pools ADD CONSTRAINT pools_price
  CHECK ((hourly_price IS NULL) = (price_currency IS NULL) AND hourly_price >= 0
    AND (hourly_price IS NULL OR provider = 'static'));
