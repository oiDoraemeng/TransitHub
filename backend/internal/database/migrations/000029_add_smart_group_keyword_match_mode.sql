ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS keyword_match_mode text NOT NULL DEFAULT 'skip_on_match'
    CHECK (keyword_match_mode IN ('skip_on_match', 'require_match'));
