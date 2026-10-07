-- 055_host_capabilities.sql — the optional features a host's runner said
-- it has (Hello.capabilities), as of its last registration: the scheduler
-- and POST /v1/runs/{id}/sync read them on any luxd, connected or not.
ALTER TABLE hosts ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}';
