-- 018_host_facts.sql — what the provider launched, from its RunInstances
-- reply: the instance type it chose (a launch template may pick it), the
-- availability zone, and the market. NULL for hosts that registered
-- themselves (static pools) and for hosts launched before this migration.
ALTER TABLE hosts ADD COLUMN instance_type text;
ALTER TABLE hosts ADD COLUMN zone text;
ALTER TABLE hosts ADD COLUMN market text CHECK (market IN ('on-demand', 'spot'));
