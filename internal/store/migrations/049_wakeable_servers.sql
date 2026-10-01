-- 049_wakeable_servers.sql — servers become a tenant's own resource: an id,
-- a stable preview host, and at most one attached Run (run_id, now
-- nullable). A server of a Run from before keeps its name, its Run and its
-- process state, gets an id and a host (<name>-<8 hex of md5(run_id/name)>),
-- and is lifetime 'run': it goes when its Run finishes for good, so those of
-- Runs that already succeeded or were cancelled go now.
--
-- wake: 'request' asks the owner (an event on the feed) to bring a Run up
-- when someone opens the preview and nothing serves it; 'never' does not.
-- wake_requested_at is the open wake: set once per wake by a conditional
-- update (the first request after the previous wake resolved or timed
-- out), cleared when the server becomes ready. idle_notified_at is the
-- last server.idle, sent once per idle period.
--
-- Desired state is not a column: a server is down when it was stopped by
-- request (state 'stopped', stop_reason 'stopped'), up otherwise.

DELETE FROM run_servers rs USING runs r WHERE r.id = rs.run_id AND r.state IN ('succeeded', 'cancelled');

-- Ids of the rows from before: 16 characters of lower-case base32 (ids.New's
-- alphabet), md5's hex digits with 0, 1, 8, 9 mapped to letters. New rows
-- get theirs from luxd (ids.New): no default once these are set.
ALTER TABLE run_servers ADD COLUMN id text NOT NULL
  DEFAULT 'srv_' || translate(substr(md5(random()::text || clock_timestamp()::text), 1, 16), '0189', 'wxyz');
ALTER TABLE run_servers ALTER COLUMN id DROP DEFAULT;
ALTER TABLE run_servers DROP CONSTRAINT run_servers_pkey;
ALTER TABLE run_servers ADD PRIMARY KEY (id);
ALTER TABLE run_servers ALTER COLUMN run_id DROP NOT NULL;
CREATE UNIQUE INDEX run_servers_run_name ON run_servers (run_id, name);

-- The preview host, relative to the preview domain (web-k3x9ab2c, or an
-- owner's web.t123.p9): unique across luxd, lower case. Left out, it is
-- <name>-<8 characters of the id>.
-- The backfill's suffix comes from the row's unique (run_id, name), so it is
-- the same on every attempt; the rare rows it gives a host already given
-- get -2, -3, ... after it (a suffix of 1-2 characters, never 8 hex like
-- every other host's, so these cannot clash in turn).
ALTER TABLE run_servers ADD COLUMN host text;
UPDATE run_servers rs SET host = b.host || CASE WHEN b.n > 1 THEN '-' || b.n ELSE '' END
  FROM (SELECT run_id, name, h AS host, row_number() OVER (PARTITION BY h ORDER BY run_id) AS n
        FROM (SELECT run_id, name, name || '-' || substr(md5(run_id || '/' || name), 1, 8) AS h FROM run_servers) x) b
  WHERE b.run_id = rs.run_id AND b.name = rs.name;
ALTER TABLE run_servers ALTER COLUMN host SET NOT NULL;
CREATE UNIQUE INDEX run_servers_host ON run_servers (host);
CREATE FUNCTION lux_server_host() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.host IS NULL THEN
    NEW.host := NEW.name || '-' || substr(NEW.id, 5, 8);
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER run_servers_host BEFORE INSERT ON run_servers FOR EACH ROW EXECUTE FUNCTION lux_server_host();

ALTER TABLE run_servers
  ADD COLUMN wake text NOT NULL DEFAULT 'never' CHECK (wake IN ('request', 'never')),
  ADD COLUMN idle_after_s int NOT NULL DEFAULT 600 CHECK (idle_after_s >= 0),     -- 0: never idle
  ADD COLUMN wake_timeout_s int NOT NULL DEFAULT 300 CHECK (wake_timeout_s > 0),
  ADD COLUMN expire_after_s int CHECK (expire_after_s > 0), -- NULL: never (lifetime owner only)
  ADD COLUMN lifetime text NOT NULL DEFAULT 'run' CHECK (lifetime IN ('run', 'owner')),
  ADD COLUMN labels jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN after_sync jsonb,                  -- argv run before its command after a sync
  ADD COLUMN owner text NOT NULL DEFAULT '',    -- who created it: a key id or an email
  ADD COLUMN wake_requested_at timestamptz,
  ADD COLUMN wake_by text,
  ADD COLUMN wake_path text,
  ADD COLUMN wakes int NOT NULL DEFAULT 0,
  ADD COLUMN idle_notified_at timestamptz,
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX run_servers_tenant ON run_servers (tenant_id, created_at);
CREATE INDEX run_servers_ready ON run_servers (run_id) WHERE state = 'ready';

-- Server events are events of the feed like a Run's: server_id names the
-- server, run_id its attached Run (NULL while it has none).
ALTER TABLE run_events ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE run_events ADD COLUMN server_id text;
-- NOT VALID: every row from before has a run_id, and validating would scan
-- run_events under an ACCESS EXCLUSIVE lock; new rows are checked.
ALTER TABLE run_events ADD CONSTRAINT run_events_subject CHECK (run_id IS NOT NULL OR server_id IS NOT NULL) NOT VALID;
CREATE INDEX run_events_server ON run_events (server_id, id) WHERE server_id IS NOT NULL;
-- The waking page's per-poll check of the current placement's sync, and
-- each placement's checkout bases (gitBases): only clone and sync events
-- are in it.
CREATE INDEX run_events_sync ON run_events (run_id, epoch, type) WHERE type IN ('git.clone', 'git.sync', 'sync.requested');

-- An event of a server with no Run notifies 'srv:<id>': no Run's
-- followers match it, only those of every event (the feed). '' stays the
-- listener's own "wake everyone".
CREATE OR REPLACE FUNCTION lux_event_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('lux_events', coalesce(NEW.run_id, 'srv:' || NEW.server_id));
  RETURN NULL;
END $$;

-- A preview ticket may be for a server rather than a Run.
ALTER TABLE stream_tickets ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE stream_tickets ADD COLUMN server_id text;

-- A resume's sync ([{repo, ref}]), handed to the Run's next placement.
ALTER TABLE runs ADD COLUMN pending_sync jsonb;
