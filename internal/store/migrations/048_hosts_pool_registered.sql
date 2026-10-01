-- 048_hosts_pool_registered.sql — a pool's latest registrations.
--
-- The planner's host expectation and GET /v1/pools' hostSize read a pool's
-- latest 8 registered hosts (ORDER BY registered_at DESC, id DESC): walking
-- this index stops after them instead of reading the pool's whole history.
CREATE INDEX hosts_pool_registered ON hosts (pool_id, registered_at DESC, id DESC) WHERE registered_at IS NOT NULL;
