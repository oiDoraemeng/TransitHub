DO $$
BEGIN
    -- upstream_sites is created by the module EnsureSchema on fresh installs,
    -- so the migration must also be safe before that table exists.
    IF to_regclass('public.upstream_sites') IS NOT NULL THEN
        ALTER TABLE upstream_sites
            ADD COLUMN IF NOT EXISTS egress_proxy_id text NULL;
        CREATE INDEX IF NOT EXISTS idx_upstream_sites_egress_proxy
            ON upstream_sites (egress_proxy_id)
            WHERE egress_proxy_id IS NOT NULL;
    END IF;
END $$;
