-- Inbound bytes counted ahead of every filter (docs/telemetry.md,
-- "Network transfer"): {"<key>": {"rx", "tx", "rxDelivered"}}. The merge
-- keeps the greatest of each direction; rxDelivered is left out where both
-- sides lack it, so reports from older runners keep their shape.
CREATE OR REPLACE FUNCTION lux_net_max(a jsonb, b jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
	SELECT jsonb_object_agg(key, jsonb_build_object('rx', rx, 'tx', tx)
		|| CASE WHEN rxd > 0 THEN jsonb_build_object('rxDelivered', rxd) ELSE '{}' END)
	FROM (
		SELECT key, max(coalesce((value->>'rx')::bigint, 0)) AS rx, max(coalesce((value->>'tx')::bigint, 0)) AS tx,
			max(coalesce((value->>'rxDelivered')::bigint, 0)) AS rxd
		FROM (
			SELECT * FROM jsonb_each(CASE WHEN jsonb_typeof(a) = 'object' THEN a ELSE '{}' END)
			UNION ALL
			SELECT * FROM jsonb_each(CASE WHEN jsonb_typeof(b) = 'object' THEN b ELSE '{}' END)
		) v
		GROUP BY key
	) merged
$$;
