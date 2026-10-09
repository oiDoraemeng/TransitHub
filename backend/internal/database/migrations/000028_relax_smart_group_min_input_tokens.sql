ALTER TABLE proxy_smart_group_members
    DROP CONSTRAINT IF EXISTS proxy_smart_group_members_min_input_tokens_check;

ALTER TABLE proxy_smart_group_members
    ADD CONSTRAINT proxy_smart_group_members_min_input_tokens_check
    CHECK (min_input_tokens >= 0);
