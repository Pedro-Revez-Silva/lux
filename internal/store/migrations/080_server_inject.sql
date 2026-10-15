-- 080_server_inject.sql — a server's inject: HTML the preview listener adds
-- right after the <head> tag of the HTML pages it proxies for the server
-- ('' adds nothing). Read per request with the server's row, so a change
-- applies to the next request.
--
-- A constant default: a catalog change, no rewrite and no scan. ADD COLUMN
-- takes ACCESS EXCLUSIVE on run_servers until this transaction commits.
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_servers ADD COLUMN inject text NOT NULL DEFAULT '';
