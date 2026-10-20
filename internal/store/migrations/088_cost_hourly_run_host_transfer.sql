CREATE INDEX cost_hourly_run_host_transfer ON cost_hourly (host_id, hour) WHERE run_id IS NOT NULL AND family = 'data-transfer';
