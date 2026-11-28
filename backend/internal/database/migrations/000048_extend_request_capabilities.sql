ALTER TABLE proxy_smart_group_members
    DROP CONSTRAINT proxy_smart_group_members_request_match_capabilities_check,
    ADD CONSTRAINT proxy_smart_group_members_request_match_capabilities_check
        CHECK (jsonb_typeof(request_match_capabilities) = 'array'
            AND request_match_capabilities <@ '["pdf", "code_execution", "json_schema", "web_search", "native_claude"]'::jsonb);
