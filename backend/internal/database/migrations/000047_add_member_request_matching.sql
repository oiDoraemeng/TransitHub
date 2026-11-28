ALTER TABLE proxy_smart_group_members
    ADD COLUMN request_match_mode text NOT NULL DEFAULT 'disabled'
        CHECK (request_match_mode IN ('disabled', 'skip_on_match', 'require_match')),
    ADD COLUMN request_match_capabilities jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(request_match_capabilities) = 'array'
            AND request_match_capabilities <@ '["pdf", "code_execution"]'::jsonb),
    ADD CONSTRAINT request_matching_requires_capabilities
        CHECK (request_match_mode = 'disabled' OR jsonb_array_length(request_match_capabilities) > 0);
