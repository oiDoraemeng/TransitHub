ALTER TABLE proxy_routes
    ADD COLUMN IF NOT EXISTS removed_models jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(removed_models) = 'object');
