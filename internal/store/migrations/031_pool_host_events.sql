-- 031_pool_host_events.sql — what happened to a pool and to a host
-- (scale-ups, launches and their failures, placements, drains, losses,
-- terminations), written in the transaction of the change each records.
--
-- tenant_id is the pool's or host's: NULL for the platform's. Like pools and
-- hosts, a tenant sees only its own rows; platform events are the
-- operators' (they name other tenants' Runs and the platform's provider
-- errors), as a platform host's history is.
--
-- count and last_at: identical consecutive failures (a launch refused every
-- tick) are one row, counted, rather than one row each; data then holds the
-- latest repeat's (the host it launched). The app role may update only
-- those three columns, and delete nothing (store.go).

CREATE TABLE pool_events (
  id         bigserial PRIMARY KEY,
  tenant_id  text REFERENCES tenants(id),
  pool_id    text NOT NULL REFERENCES pools(id),
  type       text NOT NULL,
  data       jsonb NOT NULL DEFAULT '{}',
  count      int NOT NULL DEFAULT 1,
  last_at    timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now()
);
-- The latest N of a pool, and the page before an id.
CREATE INDEX pool_events_pool ON pool_events (pool_id, id);

CREATE TABLE host_events (
  id         bigserial PRIMARY KEY,
  tenant_id  text REFERENCES tenants(id),
  host_id    text NOT NULL REFERENCES hosts(id),
  type       text NOT NULL,
  data       jsonb NOT NULL DEFAULT '{}',
  count      int NOT NULL DEFAULT 1,
  last_at    timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX host_events_host ON host_events (host_id, id);

ALTER TABLE pool_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON pool_events USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system());
ALTER TABLE host_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON host_events USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system());
