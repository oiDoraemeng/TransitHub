ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS stream_only boolean NOT NULL DEFAULT false;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS min_input_tokens integer NOT NULL DEFAULT 0;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS requests_per_minute integer NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'proxy_smart_group_members_min_input_tokens_check'
    ) THEN
        ALTER TABLE proxy_smart_group_members
        ADD CONSTRAINT proxy_smart_group_members_min_input_tokens_check
        CHECK (min_input_tokens = 0 OR min_input_tokens >= 2000);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'proxy_smart_group_members_requests_per_minute_check'
    ) THEN
        ALTER TABLE proxy_smart_group_members
        ADD CONSTRAINT proxy_smart_group_members_requests_per_minute_check
        CHECK (requests_per_minute >= 0);
    END IF;
END $$;
