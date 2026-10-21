-- 089_network_catalog.sql — a provider region's published address lists:
-- the region half of proto.NetworkClasses (source, fetchedAt and the
-- region-service, region-public, provider-global(-free) and per-region
-- provider lists; never local or zone), as the provisioner last fetched
-- them. A cache: a new host's provisional classes are built from it at
-- registration, with no provider call. version is those provisional
-- classes' Version, which a heartbeat's ack names without reading catalog.
-- Only luxd's system role may read or refresh it.
CREATE TABLE network_catalog (
  provider   text NOT NULL,
  region     text NOT NULL,
  catalog    jsonb NOT NULL,
  fetched_at timestamptz NOT NULL,
  version    text NOT NULL,
  PRIMARY KEY (provider, region)
);
ALTER TABLE network_catalog ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON network_catalog USING (lux_system()) WITH CHECK (lux_system());
