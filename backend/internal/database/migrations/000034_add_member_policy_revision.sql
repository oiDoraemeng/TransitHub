ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS policy_revision bigint NOT NULL DEFAULT 0;
