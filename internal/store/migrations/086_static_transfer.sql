-- 086_static_transfer.sql — a static host's network catalog and transfer
-- prices, an operator's (docs/costs.md, section 2, "Static hosts"): no
-- provider describes its network or prices it. network_static holds the
-- operator's lists ({local: [cidr], zone: [cidr]}); with them its classes'
-- Source is 'static' and unlisted traffic is internet. transfer_prices
-- holds per-GB prices per class and direction and the public IPv4 hourly
-- charge (DataTransferPrices). A static pool's transfer_prices is the
-- default its hosts copy when they first register; changing it does not
-- reprice existing hosts.
ALTER TABLE hosts ADD COLUMN network_static jsonb, ADD COLUMN transfer_prices jsonb;
ALTER TABLE pools ADD COLUMN transfer_prices jsonb;
ALTER TABLE pools ADD CONSTRAINT pools_transfer_prices CHECK (transfer_prices IS NULL OR provider = 'static');
