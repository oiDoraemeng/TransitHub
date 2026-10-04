CREATE TABLE IF NOT EXISTS model_egress_proxies (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    name text NOT NULL,
    protocol text NOT NULL,
    address text NOT NULL,
    url_ciphertext text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    last_test_status text NOT NULL DEFAULT 'untested',
    last_test_latency_ms bigint NULL,
    last_test_exit_ip text NOT NULL DEFAULT '',
    last_test_error text NOT NULL DEFAULT '',
    last_tested_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_model_egress_proxies_workspace
ON model_egress_proxies (user_id, admin_account_id, created_at ASC);

ALTER TABLE proxy_routes
ADD COLUMN IF NOT EXISTS egress_proxy_id text NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'fk_proxy_routes_egress_proxy'
    ) THEN
        ALTER TABLE proxy_routes
        ADD CONSTRAINT fk_proxy_routes_egress_proxy
        FOREIGN KEY (egress_proxy_id) REFERENCES model_egress_proxies(id) ON DELETE RESTRICT;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_proxy_routes_egress_proxy
ON proxy_routes (egress_proxy_id)
WHERE egress_proxy_id IS NOT NULL;
