-- 031_pool_template_tags.sql — an EC2 pool's template may no longer set
-- lux:* tags: lux tags every instance itself (lux:pool, lux:host, …) and
-- finds a pool's instances by them. A template naming one (the Terraform
-- module once generated "lux:pool") made every launch send that key twice,
-- which EC2 refuses. luxd now sends lux's value only, and putPool refuses
-- such a template; this drops the key from pools already stored with one,
-- so they can be saved unchanged. Other tags are kept; a template left with
-- no tags loses its empty "tags" object.
UPDATE pools SET template = CASE
    WHEN kept = '{}'::jsonb THEN template - 'tags'
    ELSE jsonb_set(template, '{tags}', kept)
  END
FROM (
  SELECT p.id AS pid, coalesce(jsonb_object_agg(t.key, t.value) FILTER (WHERE lower(t.key) NOT LIKE 'lux:%'), '{}'::jsonb) AS kept
  FROM pools p, jsonb_each(p.template->'tags') t
  WHERE p.provider = 'ec2' AND jsonb_typeof(p.template->'tags') = 'object'
  GROUP BY p.id
  HAVING bool_or(lower(t.key) LIKE 'lux:%')
) AS legacy
WHERE pools.id = legacy.pid;
