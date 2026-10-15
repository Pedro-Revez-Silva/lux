-- Network transfer per billing class (docs/telemetry.md, "Network transfer").
-- {"<class or provider:region>": {"rx": n, "tx": n}}: cumulative bytes.
ALTER TABLE placements ADD COLUMN net_classes jsonb;
-- network: the provider's view of the host ({subnetCidrs, zone, vpcSubnets,
-- publicIPv4}); net_classes: the NetworkClasses built from it, sent to its
-- runner; host_net_classes: the host's own traffic, outside any Run.
ALTER TABLE hosts ADD COLUMN network jsonb, ADD COLUMN net_classes jsonb, ADD COLUMN host_net_classes jsonb;

-- lux_net_max merges two such objects: per key and direction, the greater.
-- Runners report values that only grow (they fold counter resets in), so
-- the greatest report is the latest. Anything not an object (SQL NULL, JSON
-- null from an absent map) counts as empty; both empty is NULL.
CREATE FUNCTION lux_net_max(a jsonb, b jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
	SELECT jsonb_object_agg(key, jsonb_build_object('rx', rx, 'tx', tx))
	FROM (
		SELECT key, max(coalesce((value->>'rx')::bigint, 0)) AS rx, max(coalesce((value->>'tx')::bigint, 0)) AS tx
		FROM (
			SELECT * FROM jsonb_each(CASE WHEN jsonb_typeof(a) = 'object' THEN a ELSE '{}' END)
			UNION ALL
			SELECT * FROM jsonb_each(CASE WHEN jsonb_typeof(b) = 'object' THEN b ELSE '{}' END)
		) v
		GROUP BY key
	) merged
$$;
