ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS sync_key_delete_enabled boolean NOT NULL DEFAULT false;

ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS sync_key_delete_delay_ms integer NOT NULL DEFAULT 500;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'proxy_smart_group_members_sync_key_delete_delay_check'
    ) THEN
        ALTER TABLE proxy_smart_group_members
        ADD CONSTRAINT proxy_smart_group_members_sync_key_delete_delay_check
        CHECK (sync_key_delete_delay_ms BETWEEN 1 AND 60000);
    END IF;
END $$;

-- Retire historical cleanup records that already exhausted the new limit.
DELETE FROM proxy_cleanup_jobs
WHERE status <> 'done' AND attempts > 3;
