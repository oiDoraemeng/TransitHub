ALTER TABLE proxy_routes
    ADD COLUMN IF NOT EXISTS upstream_key_ciphertext text;

ALTER TABLE proxy_routes
    ADD COLUMN IF NOT EXISTS upstream_key_preview text NOT NULL DEFAULT '';
