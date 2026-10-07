ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS use_upstream_key boolean NOT NULL DEFAULT false;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS upstream_key_ciphertext text;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS upstream_key_preview text NOT NULL DEFAULT '';
