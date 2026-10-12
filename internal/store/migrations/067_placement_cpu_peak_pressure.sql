-- 067_placement_cpu_peak_pressure.sql — a placement's peak CPU rate and its
-- cgroup's pressure stall (PSI) totals (docs/telemetry.md). peak_cpu_cores
-- is the highest rate in cores between two consecutive runner readings; the
-- stall columns are PSI "some" totals (and "full" for memory and io), in
-- seconds. NULL: not reported (an older runner, or a kernel without PSI).
ALTER TABLE placements
  ADD COLUMN peak_cpu_cores            float8,
  ADD COLUMN cpu_stall_seconds         float8,
  ADD COLUMN memory_stall_seconds      float8,
  ADD COLUMN memory_full_stall_seconds float8,
  ADD COLUMN io_stall_seconds          float8,
  ADD COLUMN io_full_stall_seconds     float8;

-- In samples: the peak a rollup's maximum, the stall totals counters.
ALTER TABLE placement_samples
  ADD COLUMN peak_cpu_cores  float8,
  ADD COLUMN cpu_stall       float8,
  ADD COLUMN mem_stall       float8,
  ADD COLUMN mem_full_stall  float8,
  ADD COLUMN io_stall        float8,
  ADD COLUMN io_full_stall   float8;
