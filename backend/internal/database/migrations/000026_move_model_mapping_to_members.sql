ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS model_mapping jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Preserve mappings created by the previous group-level UI for members that
-- had mapping enabled. New writes use the member column exclusively.
UPDATE proxy_smart_group_members m
SET model_mapping = COALESCE(g.model_mapping, '{}'::jsonb)
FROM proxy_smart_groups g
WHERE g.id = m.smart_group_id
  AND m.model_mapping_enabled = true
  AND COALESCE(g.model_mapping, '{}'::jsonb) <> '{}'::jsonb;
