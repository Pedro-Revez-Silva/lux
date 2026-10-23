-- 091_pool_preemptible.sql — whether lux may take a pool's Runs away on its
-- own initiative (docs/runspec.md, Non-preemptible Runs).
--
-- pools.preemptible: true (the default) for every pool lux may preempt:
-- spot hosts. false: an ec2 pool launches on-demand instances, and only a
-- pool with false takes Runs submitted with placement.preemptible false.
-- An ec2 pool's market was its template's spot until now: one without
-- spot true launched on-demand, so it is backfilled false. Static pools
-- stay true: they opt in explicitly.
--
-- ADD COLUMN with a constant default is catalog-only; the UPDATE touches
-- only ec2 pools, a small table.
SET LOCAL lock_timeout = '5s';
ALTER TABLE pools ADD COLUMN preemptible boolean NOT NULL DEFAULT true;
UPDATE pools SET preemptible = false
	WHERE provider = 'ec2' AND (template->'spot') IS DISTINCT FROM 'true'::jsonb;
