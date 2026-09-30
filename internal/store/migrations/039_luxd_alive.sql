-- 039_luxd_alive.sql — each luxd process records, every few seconds, that
-- it is running and reaches Postgres, and when it came back from a gap: a time
-- no luxd recorded itself (every luxd stopped or hung, or Postgres
-- unreachable). Nothing is reaped for lost heartbeats during such a gap,
-- nor for a lease after it, so runners have time to reach luxd again. A
-- row per process, so one stalled luxd holds up no other's record. Empty
-- until a luxd first records itself (older luxds never do), so the time
-- before is not taken for a gap.
CREATE TABLE luxd_alive (
  instance   text PRIMARY KEY,
  at         timestamptz NOT NULL,
  resumed_at timestamptz
);
ALTER TABLE luxd_alive ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON luxd_alive USING (lux_system()) WITH CHECK (lux_system());
