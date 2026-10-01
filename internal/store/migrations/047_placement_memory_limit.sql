-- 047_placement_memory_limit.sql — the memory a placement's container got.
--
-- A Run's resources.memory is in its host's terms (the machine's gross
-- memory); the runner gives the container that share of what Linux can
-- give Runs. memory_limit is that limit in bytes, as the runner reported
-- it when the container started. NULL for placements from before this
-- migration and ones whose container never started.
ALTER TABLE placements ADD COLUMN memory_limit bigint;
