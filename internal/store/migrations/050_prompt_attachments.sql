-- 050_prompt_attachments.sql — the bytes of a Run's prompt images
-- (workload.attachments[*].data), in order, kept out of runs.spec: every
-- read of a spec (views, the scheduler, the capacity plan, previews) would
-- otherwise decode them, and only the first placement needs them. The
-- spec keeps each attachment's name and contentType. Cleared once a
-- placement resumes the Run (it has a session or a snapshot), or when the
-- Run succeeds or is cancelled.
ALTER TABLE runs ADD COLUMN prompt_attachments jsonb;
