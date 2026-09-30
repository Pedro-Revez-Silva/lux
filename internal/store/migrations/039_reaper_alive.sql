-- 039_reaper_alive.sql — when a luxd reaper last ran. A longer gap (every
-- luxd stopped or hung, or Postgres unreachable) is time nobody could hear
-- heartbeats, so it is added back to leases before the next reap rather
-- than held against the hosts. NULL until a reaper first runs: luxds older
-- than this never write it, so time before then is not forgiven.
CREATE TABLE reaper_alive (
  one boolean PRIMARY KEY DEFAULT true CHECK (one),
  at  timestamptz
);
INSERT INTO reaper_alive (at) VALUES (NULL);
ALTER TABLE reaper_alive ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON reaper_alive USING (lux_system()) WITH CHECK (lux_system());
