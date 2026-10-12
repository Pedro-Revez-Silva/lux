-- 068_placement_image_platforms.sql — the platforms (os/arch[/variant])
-- a placement's image reference offers: every entry of a manifest list or
-- OCI index, a single-arch image's own, a built image's host platform.
-- NULL: unknown (an older runner, or the runner could not tell).
ALTER TABLE placements ADD COLUMN image_platforms text[];
