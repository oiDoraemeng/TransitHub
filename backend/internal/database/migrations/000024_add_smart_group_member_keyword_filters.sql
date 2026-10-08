ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS keyword_check_enabled boolean NOT NULL DEFAULT false;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS excluded_keywords text[] NOT NULL DEFAULT ARRAY[]::text[];
