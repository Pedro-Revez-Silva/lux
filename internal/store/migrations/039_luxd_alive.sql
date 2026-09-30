-- 039_luxd_alive.sql — when a luxd last recorded that it can hear
-- heartbeats (every tick, apart from the reaper), and when it last came
-- back from a longer gap: every luxd stopped or hung, or Postgres
-- unreachable. Nothing is reaped for lost heartbeats during such a gap, nor
-- for a lease after it, so runners have time to reach luxd again. `at` is
-- NULL until a luxd first records it (older luxds never do), so the time
-- before is not taken for a gap.
CREATE TABLE luxd_alive (
  one        boolean PRIMARY KEY DEFAULT true CHECK (one),
  at         timestamptz,
  resumed_at timestamptz
);
INSERT INTO luxd_alive DEFAULT VALUES;
ALTER TABLE luxd_alive ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON luxd_alive USING (lux_system()) WITH CHECK (lux_system());
