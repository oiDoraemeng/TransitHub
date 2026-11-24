ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS reasoning_effort text NOT NULL DEFAULT ''
    CHECK (reasoning_effort IN ('', 'low', 'medium', 'high', 'xhigh', 'max'));
