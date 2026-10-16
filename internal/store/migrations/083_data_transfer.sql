-- 083_data_transfer.sql — network transfer priced per placement
-- (docs/costs.md, section 2, "Data transfer"). A placement's snapshot is
-- its bytes per class and direction at the per-GB prices of its host, frozen
-- once the placement has ended and is priced. It has no hourly rate: the
-- amount is bytes × price, item by item (items), spread over the hours the
-- bytes moved in (hours). unpriced: bytes no price applies to (class
-- unknown, a host whose network discovery was partial, a class with no
-- price), shown to the Run's tenant so absence never reads as zero.
CREATE TABLE cost_transfer_snapshots (
  placement_id text PRIMARY KEY REFERENCES placements(id),
  run_id       text NOT NULL REFERENCES runs(id),
  tenant_id    text NOT NULL REFERENCES tenants(id),
  host_id      text NOT NULL REFERENCES hosts(id),
  currency     text,
  amount       numeric(30, 9),
  items        jsonb NOT NULL DEFAULT '[]',
  hours        jsonb NOT NULL DEFAULT '[]',
  unpriced     jsonb NOT NULL DEFAULT '[]',
  priced_to    timestamptz,
  finalized    boolean NOT NULL DEFAULT false,
  CHECK (NOT finalized OR priced_to IS NOT NULL),
  CHECK ((currency IS NULL) = (amount IS NULL))
);
CREATE INDEX cost_transfer_snapshots_run ON cost_transfer_snapshots (run_id);
ALTER TABLE cost_transfer_snapshots ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON cost_transfer_snapshots
  USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system());

-- A placement's per-class counters on the heartbeat samples that changed
-- them (NULL when unchanged since net_sampled, the last value sampled):
-- their differences place its transfer in the hours it happened.
ALTER TABLE placement_samples ADD COLUMN net_classes jsonb;
ALTER TABLE placements ADD COLUMN net_sampled jsonb;
