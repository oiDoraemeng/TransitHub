ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS never_auto_pause boolean NOT NULL DEFAULT false;
