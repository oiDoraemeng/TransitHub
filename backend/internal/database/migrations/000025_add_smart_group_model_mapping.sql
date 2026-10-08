ALTER TABLE proxy_smart_groups
    ADD COLUMN IF NOT EXISTS model_mapping jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS model_mapping_enabled boolean NOT NULL DEFAULT false;
