-- 030_servers.sql — a Run's servers, stream tickets, and luxd's own keys.
--
-- run_servers: a named port of a Run, optionally with a command lux starts
-- in its container. A record outlives placements; its process does not.
-- The columns name, port, command, workdir and env are what the server is
-- now (PUT changes them); active is what it was started with (null while
-- stopped), so an edit applies at its next start. gen names its current
-- start (or stop): the runner reports states per gen, and a report for
-- another gen is stale. Gens come from one sequence, so a server removed
-- and added again under its name never reuses one.

CREATE SEQUENCE run_servers_gen;

CREATE TABLE run_servers (
  tenant_id       text NOT NULL REFERENCES tenants(id),
  run_id          text NOT NULL REFERENCES runs(id),
  name            text NOT NULL,
  port            int  NOT NULL CHECK (port BETWEEN 1 AND 65535),
  command         jsonb,                        -- argv, or NULL: a port only
  workdir         text NOT NULL DEFAULT '',
  env             jsonb NOT NULL DEFAULT '{}',
  from_spec       boolean NOT NULL DEFAULT false,
  state           text NOT NULL DEFAULT 'stopped'
                  CHECK (state IN ('stopped', 'starting', 'ready', 'unreachable', 'exited')),
  active          jsonb,                        -- {port, command, workdir, env} as started
  gen             bigint NOT NULL DEFAULT nextval('run_servers_gen'),
  exit_code       int,
  error           text,
  since           timestamptz NOT NULL DEFAULT now(),
  ready_since     timestamptz,
  stop_reason     text,                         -- stopped | run stopped | migrated | host lost
  stopped_epoch   int,
  epoch           int,
  last_request_at timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, name)
);

-- The desired set a placement was last sent: every change bumps it, and a
-- runner ignores a set older than one it has.
ALTER TABLE runs ADD COLUMN servers_rev bigint NOT NULL DEFAULT 0;

-- Single-use, short-lived credentials for what a browser cannot put an
-- Authorization header on (a WebSocket, a redirect): bound to a Run, a
-- kind and the principal that minted them. Only their hash is stored.
CREATE TABLE stream_tickets (
  token_hash  text PRIMARY KEY,
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  kind        text NOT NULL CHECK (kind IN ('exec', 'preview')),
  principal   jsonb NOT NULL,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stream_tickets_expires ON stream_tickets (expires_at);

-- Keys luxd makes for itself and shares between its instances (the
-- preview cookie's HMAC key). Never a tenant's.
CREATE TABLE luxd_keys (
  name       text PRIMARY KEY,
  key        bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE run_servers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON run_servers USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());
ALTER TABLE stream_tickets ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON stream_tickets USING (lux_system()) WITH CHECK (lux_system());
ALTER TABLE luxd_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON luxd_keys USING (lux_system()) WITH CHECK (lux_system());
