ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS priority integer NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'proxy_smart_group_members_priority_check'
    ) THEN
        ALTER TABLE proxy_smart_group_members
        ADD CONSTRAINT proxy_smart_group_members_priority_check
        CHECK (priority >= 0);
    END IF;
END $$;
