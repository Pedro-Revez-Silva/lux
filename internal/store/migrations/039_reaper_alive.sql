-- 039_reaper_alive.sql — when a luxd last ran its reaper. A longer gap
-- (every luxd stopped or hung, or Postgres unreachable) is time nobody
-- could hear heartbeats, so it is added back to leases before the next
-- reap rather than held against the hosts.
CREATE TABLE reaper_alive (
  one boolean PRIMARY KEY DEFAULT true CHECK (one),
  at  timestamptz NOT NULL
);
INSERT INTO reaper_alive (at) VALUES (now());
