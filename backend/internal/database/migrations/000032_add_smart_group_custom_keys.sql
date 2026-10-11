ALTER TABLE proxy_access_keys
    ADD COLUMN IF NOT EXISTS key_kind text NOT NULL DEFAULT 'primary';

ALTER TABLE proxy_access_keys
    DROP CONSTRAINT IF EXISTS proxy_access_keys_owner_type_owner_id_key;

ALTER TABLE proxy_access_keys
    ADD CONSTRAINT proxy_access_keys_key_kind_check
    CHECK (key_kind IN ('primary', 'custom'));

CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_access_keys_primary_owner
ON proxy_access_keys (owner_type, owner_id)
WHERE key_kind = 'primary';

CREATE INDEX IF NOT EXISTS idx_proxy_access_keys_owner
ON proxy_access_keys (owner_type, owner_id, key_kind, created_at ASC);
