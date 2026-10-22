ALTER TABLE proxy_smart_groups
    ADD COLUMN IF NOT EXISTS local_reply_max_output_tokens integer NOT NULL DEFAULT 20
        CHECK (local_reply_max_output_tokens BETWEEN 0 AND 1000000),
    ADD COLUMN IF NOT EXISTS local_reply_text text NOT NULL DEFAULT 'OK'
        CHECK (char_length(local_reply_text) BETWEEN 1 AND 4000);
