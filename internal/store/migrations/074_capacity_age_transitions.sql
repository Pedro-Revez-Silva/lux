-- 074_capacity_age_transitions.sql — every placement resource transition
-- appends to its host's capacity history (073) in the same transaction, so
-- a release between scheduler passes, by any luxd, is recorded when it
-- happens. The history's row is only ever written under its host's
-- cost-host advisory lock (infraevents.go's lock order, step 3): every
-- production writer of a placement's live state or resources takes it first.
-- This trigger takes no lock of its own; a write without that lock (none in
-- luxd; tests' fixtures) drops the history instead, which is conservative.
-- Invalidation (reconnect, capacity, pool, shape) is by the history's key,
-- which the scheduler compares on read; nothing here locks a host row.
CREATE FUNCTION lux_capacity_age_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  hid text;
  k bigint;
  hist jsonb;
  cap jsonb;
  cpu float8;
  mem bigint;
  disk bigint;
  slots bigint;
  free jsonb;
  merged jsonb;
  at timestamptz := clock_timestamp();
BEGIN
  hid := CASE WHEN TG_OP = 'DELETE' THEN OLD.host_id ELSE NEW.host_id END;
  k := hashtextextended('cost-host:' || hid, 0);
  IF NOT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
      AND classid::bigint = ((k >> 32) & 4294967295) AND objid::bigint = (k & 4294967295) AND objsubid = 1) THEN
    -- A writer outside the lock order cannot be serialised with the
    -- scheduler's read: forget the history, which only delays spot work.
    DELETE FROM host_capacity_age WHERE host_id = hid;
    RETURN NULL;
  END IF;
  SELECT history INTO hist FROM host_capacity_age WHERE host_id = hid;
  IF hist IS NULL THEN RETURN NULL; END IF;
  SELECT capacity INTO cap FROM hosts WHERE id = hid;
  SELECT coalesce(sum((resources->>'cpus')::float8), 0), coalesce(sum((resources->>'memory')::bigint), 0),
         coalesce(sum((resources->>'disk')::bigint), 0), count(*)
    INTO cpu, mem, disk, slots FROM placements
    WHERE host_id = hid AND state IN ('assigned', 'starting', 'running', 'stopping');
  free := jsonb_build_object(
    'cpus',   CASE WHEN coalesce((cap->>'cpus')::float8, 0) > 0 THEN (cap->>'cpus')::float8 - cpu ELSE 1152921504606846976 END,
    'memory', CASE WHEN coalesce((cap->>'memory')::bigint, 0) > 0 THEN (cap->>'memory')::bigint - mem ELSE 1152921504606846976 END,
    'disk',   CASE WHEN coalesce((cap->>'disk')::bigint, 0) > 0 THEN (cap->>'disk')::bigint - disk ELSE 1152921504606846976 END,
    'runs',   CASE WHEN coalesce((cap->>'runs')::bigint, 0) > 0 THEN (cap->>'runs')::bigint - slots ELSE 1152921504606846976 END);
  IF hist->(jsonb_array_length(hist) - 1)->'free' = free THEN RETURN NULL; END IF;
  hist := hist || jsonb_build_array(jsonb_build_object('at', at, 'free', free));
  -- Drop intervals that ended before the window, keeping its boundary entry.
  WHILE jsonb_array_length(hist) > 1 AND (hist->1->>'at')::timestamptz < at - interval '60 seconds' LOOP
    hist := hist - 0;
  END LOOP;
  -- Bounded: merge the two oldest by their minimum, which only understates.
  WHILE jsonb_array_length(hist) > 32 LOOP
    SELECT jsonb_object_agg(f, least((hist->0->'free'->>f)::numeric, (hist->1->'free'->>f)::numeric))
      INTO merged FROM unnest(ARRAY['cpus', 'memory', 'disk', 'runs']) f;
    hist := jsonb_set(hist, '{1}', jsonb_build_object('at', hist->0->'at', 'free', merged)) - 0;
  END LOOP;
  UPDATE host_capacity_age SET history = hist WHERE host_id = hid;
  RETURN NULL;
END $$;
-- Only transitions that change what a host holds: a placement created,
-- entering or leaving the live states, or resized.
CREATE TRIGGER capacity_age_transition AFTER INSERT OR DELETE ON placements
  FOR EACH ROW EXECUTE FUNCTION lux_capacity_age_transition();
CREATE TRIGGER capacity_age_transition_update AFTER UPDATE OF resources, state ON placements
  FOR EACH ROW WHEN (OLD.resources IS DISTINCT FROM NEW.resources
    OR (OLD.state IN ('assigned', 'starting', 'running', 'stopping')) <> (NEW.state IN ('assigned', 'starting', 'running', 'stopping')))
  EXECUTE FUNCTION lux_capacity_age_transition();
