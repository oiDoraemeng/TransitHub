ALTER TABLE proxy_smart_group_members
    DROP CONSTRAINT proxy_smart_group_members_request_match_mode_check,
    ADD CONSTRAINT proxy_smart_group_members_request_match_mode_check
        CHECK (request_match_mode IN ('disabled', 'skip_on_match', 'require_match', 'match_or_fallback'));
