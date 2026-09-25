-- 015_pool_scale_down.sql — per-pool idle seconds before a provisioned host
-- is released (NULL: luxd's scale_down_after), and warm_while_active: keep
-- the pool's warm hosts only while it has been used within that time, so an
-- idle pool scales down to its minimum.
ALTER TABLE pools ADD COLUMN scale_down_after_s integer CHECK (scale_down_after_s > 0);
ALTER TABLE pools ADD COLUMN warm_while_active boolean NOT NULL DEFAULT false;
