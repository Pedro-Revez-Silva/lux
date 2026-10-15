-- 001_initial.sql — the whole v1 schema.
--
-- Tenant-scoped tables carry tenant_id and are protected by row-level
-- security. luxd connects as lux_app, which has neither SUPERUSER nor
-- BYPASSRLS, so the policies are a real boundary: a query that forgets its
-- tenant filter returns nothing rather than another tenant's rows.
--
-- Two scopes, set per transaction by luxd:
--   lux.tenant_id = <id>  API requests on behalf of one tenant;
--   lux.system    = 'on'  the scheduler, reapers and runner endpoints, which
--                         work across tenants and filter explicitly.

CREATE FUNCTION lux_tenant() RETURNS text
LANGUAGE sql STABLE AS $$ SELECT nullif(current_setting('lux.tenant_id', true), '') $$;

CREATE FUNCTION lux_system() RETURNS boolean
LANGUAGE sql STABLE AS $$ SELECT coalesce(current_setting('lux.system', true), '') = 'on' $$;

CREATE TABLE tenants (
  id                  text PRIMARY KEY,
  name                text NOT NULL UNIQUE,
  -- How long the blobs of a terminal Run are kept.
  retention_days      int  NOT NULL DEFAULT 30,
  -- Quotas, enforced at submission. NULL means unlimited.
  max_concurrent_runs int,
  max_hosts           int,
  max_storage_bytes   bigint,
  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
  id           text PRIMARY KEY,
  tenant_id    text NOT NULL REFERENCES tenants(id),
  name         text NOT NULL,
  key_hash     text NOT NULL UNIQUE,
  scopes       text[] NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz,
  last_used_at timestamptz
);

CREATE TABLE pools (
  id          text PRIMARY KEY,
  -- NULL: a platform pool, whose hosts may serve several tenants if shared.
  tenant_id   text REFERENCES tenants(id),
  name        text NOT NULL,
  provider    text NOT NULL CHECK (provider IN ('static', 'ec2')),
  template    jsonb NOT NULL DEFAULT '{}',
  min_hosts   int NOT NULL DEFAULT 0,
  max_hosts   int NOT NULL DEFAULT 0,
  warm_hosts  int NOT NULL DEFAULT 0,
  -- Several tenants' Runs on one host: only in a platform pool that opts in.
  shared      boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CHECK (NOT shared OR tenant_id IS NULL)
);
CREATE UNIQUE INDEX pools_name ON pools (coalesce(tenant_id, ''), name);

CREATE TABLE host_tokens (
  id          text PRIMARY KEY,
  tenant_id   text REFERENCES tenants(id),
  -- The pool hosts registering with this token join.
  pool        text NOT NULL DEFAULT 'default',
  labels      jsonb NOT NULL DEFAULT '{}',
  token_hash  text NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  revoked_at  timestamptz
);

CREATE TABLE hosts (
  id              text PRIMARY KEY,
  -- NULL: a platform host.
  tenant_id       text REFERENCES tenants(id),
  pool            text NOT NULL DEFAULT 'default',
  token_id        text REFERENCES host_tokens(id),
  name            text NOT NULL,
  state           text NOT NULL CHECK (state IN ('provisioning', 'ready', 'draining', 'lost', 'terminated')),
  labels          jsonb NOT NULL DEFAULT '{}',
  arch            text NOT NULL DEFAULT '',
  capacity        jsonb NOT NULL DEFAULT '{}',
  versions        jsonb NOT NULL DEFAULT '{}',
  caches          jsonb NOT NULL DEFAULT '{}',
  local_snapshots jsonb NOT NULL DEFAULT '[]',
  provider_id     text,
  -- Set by luxd to refuse new placements (drain) and to terminate after.
  draining        boolean NOT NULL DEFAULT false,
  state_reason    text NOT NULL DEFAULT '',
  -- Lifecycle telemetry (docs/telemetry.md).
  provision_requested_at  timestamptz,  -- a provider was asked for this host
  provisioned_at          timestamptz,  -- the provider reports it running
  registered_at           timestamptz,  -- first hello from its runner
  first_placement_at      timestamptz,  -- first container started on it
  last_placement_ended_at timestamptz,  -- idle since; drives scale-down
  drain_requested_at      timestamptz,
  terminate_requested_at  timestamptz,
  terminated_at           timestamptz,
  lost_at                 timestamptz,
  last_heartbeat          timestamptz,
  created_at              timestamptz NOT NULL DEFAULT now()
);
-- A host that comes back after being lost re-registers as the same row.
CREATE UNIQUE INDEX hosts_name ON hosts (coalesce(tenant_id, ''), name) WHERE state <> 'terminated';

CREATE TABLE runs (
  id                   text PRIMARY KEY,
  tenant_id            text NOT NULL REFERENCES tenants(id),
  name                 text NOT NULL DEFAULT '',
  labels               jsonb NOT NULL DEFAULT '{}',
  -- The normalized spec, without secret values.
  spec                 jsonb NOT NULL,
  -- [{name, fingerprint}] so a resume missing one is rejected before scheduling.
  secrets              jsonb NOT NULL DEFAULT '[]',
  state                text NOT NULL,
  state_reason         text NOT NULL DEFAULT '',
  exit_code            int,
  current_epoch        int NOT NULL DEFAULT 0,
  -- The adapter's own session id, passed back on resume.
  session_id           text NOT NULL DEFAULT '',
  -- idle | busy, from the adapter: "waiting for input" rather than "hung".
  activity             text NOT NULL DEFAULT '',
  -- The snapshot a resume starts from.
  snapshot_id          text,
  -- Resolved Containerfile for image.build, so rebuilds use pinned FROMs.
  image_resolved       jsonb,
  cancel_requested     boolean NOT NULL DEFAULT false,
  -- Input to deliver as the first message of the next placement (resume).
  pending_input        jsonb,
  idempotency_key      text,
  -- Lifecycle telemetry (docs/telemetry.md). created_at is "requested".
  first_scheduled_at   timestamptz,
  first_started_at     timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  finished_at          timestamptz,
  UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX runs_schedulable ON runs (created_at) WHERE state IN ('submitted', 'resuming', 'provisioning');
CREATE INDEX runs_tenant_created ON runs (tenant_id, created_at DESC);

CREATE TABLE placements (
  id               text PRIMARY KEY,
  tenant_id        text NOT NULL REFERENCES tenants(id),
  run_id           text NOT NULL REFERENCES runs(id),
  host_id          text NOT NULL REFERENCES hosts(id),
  epoch            int NOT NULL,
  state            text NOT NULL CHECK (state IN ('assigned', 'starting', 'running', 'stopping', 'exited', 'lost')),
  resources        jsonb NOT NULL DEFAULT '{}',
  lease_expires_at timestamptz,
  -- Lifecycle telemetry (docs/telemetry.md). created_at is "assigned".
  created_at           timestamptz NOT NULL DEFAULT now(),
  started_at           timestamptz,  -- reached running
  ended_at             timestamptz,  -- exited or lost
  accepted_at          timestamptz,  -- runner acked the assignment
  image_ready_at       timestamptz,
  volumes_restored_at  timestamptz,
  container_started_at timestamptz,
  workload_started_at  timestamptz,  -- init finished, workload exec'd
  stop_requested_at    timestamptz,
  -- Why luxd asked it to stop: stop | cancel | timeout | drain | preempt.
  stop_reason          text NOT NULL DEFAULT '',
  exited_at            timestamptz,
  snapshot_done_at     timestamptz,
  uploaded_at          timestamptz,
  -- Resource peaks, reported with heartbeats and finally on exit, so a host
  -- that dies still leaves its last-known values.
  peak_memory_bytes    bigint,
  peak_disk_bytes      bigint,
  peak_pids            int,
  cpu_seconds          double precision,
  net_rx_bytes         bigint,
  net_tx_bytes         bigint,
  snapshot_bytes       bigint,
  exit_code        int,
  exit_reason      text NOT NULL DEFAULT '',
  -- Highest output sequence number, known once the placement has exited.
  output_seq       bigint,
  output_blob_id   text,
  UNIQUE (run_id, epoch)
);
CREATE INDEX placements_host_live ON placements (host_id) WHERE state IN ('assigned', 'starting', 'running', 'stopping');

CREATE TABLE blobs (
  id          text PRIMARY KEY,
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  epoch       int NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('volume', 'output', 'artifact', 'context')),
  name        text NOT NULL,
  size        bigint NOT NULL DEFAULT 0,
  sha256      text NOT NULL DEFAULT '',
  -- host: only on the host that wrote it; s3: uploaded. There is no
  -- control-plane disk tier: runners upload to S3 with presigned URLs.
  location    text NOT NULL CHECK (location IN ('host', 's3', 'deleted')),
  host_id     text REFERENCES hosts(id),
  s3_key      text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  uploaded_at timestamptz,
  deleted_at  timestamptz
);
CREATE INDEX blobs_run ON blobs (run_id);

CREATE TABLE snapshots (
  id           text PRIMARY KEY,
  tenant_id    text NOT NULL REFERENCES tenants(id),
  run_id       text NOT NULL REFERENCES runs(id),
  placement_id text NOT NULL REFERENCES placements(id),
  epoch        int NOT NULL,
  -- {volumes: [{name, path, blobId, size, sha256}], sessionId, epoch}
  manifest     jsonb NOT NULL,
  host_id      text REFERENCES hosts(id),
  -- False once no copy is reachable (its host was lost before upload).
  available    boolean NOT NULL DEFAULT true,
  -- The host still keeps the volumes locally (until told to discard them).
  host_copy    boolean NOT NULL DEFAULT true,
  uploaded     boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX snapshots_run ON snapshots (run_id, epoch DESC);

CREATE TABLE artifacts (
  id           text PRIMARY KEY,
  tenant_id    text NOT NULL REFERENCES tenants(id),
  run_id       text NOT NULL REFERENCES runs(id),
  epoch        int NOT NULL,
  path         text NOT NULL,
  blob_id      text NOT NULL REFERENCES blobs(id),
  content_type text NOT NULL DEFAULT 'application/octet-stream',
  size         bigint NOT NULL DEFAULT 0,
  sha256       text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX artifacts_run ON artifacts (run_id, created_at);

-- Lifecycle events, inputs and their acks, audit. Low volume; the workload's
-- own output lives in files on the host and then in S3.
CREATE TABLE run_events (
  id         bigserial PRIMARY KEY,
  tenant_id  text NOT NULL REFERENCES tenants(id),
  run_id     text NOT NULL REFERENCES runs(id),
  epoch      int,
  type       text NOT NULL,
  data       jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_events_run ON run_events (run_id, id);

-- Control messages to runners. Durable so a message sent while a runner is
-- disconnected is delivered when it returns, and so any luxd instance can
-- send one. Never holds secret values.
CREATE TABLE host_messages (
  id           bigserial PRIMARY KEY,
  host_id      text NOT NULL REFERENCES hosts(id),
  run_id       text,
  epoch        int,
  type         text NOT NULL,
  payload      jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  acked_at     timestamptz
);
CREATE INDEX host_messages_pending ON host_messages (host_id, id) WHERE acked_at IS NULL;

-- Side effects that must happen after a commit: provision requests.
CREATE TABLE outbox (
  id         bigserial PRIMARY KEY,
  kind       text NOT NULL,
  payload    jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  done_at    timestamptz
);

-- Row-level security ---------------------------------------------------------

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_self ON tenants USING (id = lux_tenant() OR lux_system());

DO $$
DECLARE t text;
BEGIN
  -- Tables every tenant row belongs to exactly one tenant.
  FOREACH t IN ARRAY ARRAY['api_keys', 'runs', 'placements', 'blobs', 'snapshots', 'artifacts', 'run_events'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system())', t);
  END LOOP;
  -- Tables where NULL means the platform: tenants see only their own rows.
  FOREACH t IN ARRAY ARRAY['pools', 'host_tokens', 'hosts'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system())', t);
  END LOOP;
  -- Runner plumbing: system only.
  FOREACH t IN ARRAY ARRAY['host_messages', 'outbox'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY system_only ON %I USING (lux_system()) WITH CHECK (lux_system())', t);
  END LOOP;
END $$;
