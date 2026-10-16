-- 085_host_transfer_samples.sql — a host's own network transfer per hour
-- (docs/costs.md, section 2, "Data transfer"): heartbeat samples carry the
-- host's cumulative per-class counters (hosts.host_net_classes) when they
-- changed since net_sampled, the last value sampled. The hourly refresh
-- prices the differences within each hour as unallocated data-transfer.
ALTER TABLE host_samples ADD COLUMN net_classes jsonb;
ALTER TABLE hosts ADD COLUMN net_sampled jsonb;
