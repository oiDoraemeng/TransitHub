ALTER TABLE proxy_smart_groups
    ADD COLUMN IF NOT EXISTS local_reply_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS local_reply_min_tokens integer NOT NULL DEFAULT 100
        CHECK (local_reply_min_tokens BETWEEN 1 AND 1000000);
