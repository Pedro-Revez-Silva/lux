-- 029_process_samples.sql — lux's own processes over time: luxd on the
-- control samples, each host's runner on its host samples (the runner
-- process only, not the podman and conmon processes it starts). All NULL
-- where unknown: older samples, and runners that predate the report.
--
-- proc_started tells one process from the next: proc_cpu_seconds counts
-- from it, so CPU rates are only taken between samples of the same start.

ALTER TABLE control_samples
  ADD COLUMN proc_started     timestamptz,
  ADD COLUMN proc_cpu_seconds float8,      -- counter: user + system seconds since proc_started
  ADD COLUMN proc_rss         bigint,
  ADD COLUMN proc_peak_rss    bigint,      -- highest RSS since the previous sample
  ADD COLUMN proc_heap        bigint,      -- Go heap objects, live or not yet swept
  ADD COLUMN goroutines       bigint,
  -- The machine the sampling luxd runs on. From here on `instance` is the
  -- luxd process's own id (luxd_...), new at each start, so no two
  -- processes share one; before, it was the hostname.
  ADD COLUMN hostname         text;
UPDATE control_samples SET hostname = instance;
ALTER TABLE control_samples ALTER COLUMN hostname SET NOT NULL;

ALTER TABLE host_samples
  ADD COLUMN proc_started     timestamptz,
  ADD COLUMN proc_cpu_seconds float8,
  ADD COLUMN proc_rss         bigint,
  ADD COLUMN proc_peak_rss    bigint,
  ADD COLUMN proc_heap        bigint,
  ADD COLUMN goroutines       bigint;
