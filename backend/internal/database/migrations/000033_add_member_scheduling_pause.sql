ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS scheduling_paused boolean NOT NULL DEFAULT false;
