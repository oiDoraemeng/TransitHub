ALTER TABLE proxy_smart_group_members
    ADD COLUMN thinking_enabled boolean NOT NULL DEFAULT true,
    ADD COLUMN simulate_thinking boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT proxy_member_simulated_thinking_requires_mapping
        CHECK (NOT simulate_thinking OR (thinking_enabled AND model_mapping_enabled));
