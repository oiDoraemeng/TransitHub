CREATE TABLE IF NOT EXISTS proxy_routes (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    name text NOT NULL,
    site_id text NOT NULL,
    group_id text NOT NULL,
    group_name text NOT NULL,
    concurrency_limit integer NOT NULL DEFAULT 50 CHECK (concurrency_limit > 0),
    enabled boolean NOT NULL DEFAULT true,
    model_synced_at timestamptz NULL,
    model_sync_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_proxy_routes_workspace
ON proxy_routes (user_id, admin_account_id, created_at ASC);

CREATE INDEX IF NOT EXISTS idx_proxy_routes_site
ON proxy_routes (site_id);

CREATE TABLE IF NOT EXISTS proxy_smart_groups (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    name text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_proxy_smart_groups_workspace
ON proxy_smart_groups (user_id, admin_account_id, created_at ASC);

CREATE TABLE IF NOT EXISTS proxy_smart_group_members (
    smart_group_id text NOT NULL REFERENCES proxy_smart_groups(id) ON DELETE CASCADE,
    route_id text NOT NULL REFERENCES proxy_routes(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (smart_group_id, route_id)
);

CREATE INDEX IF NOT EXISTS idx_proxy_smart_group_members_route
ON proxy_smart_group_members (route_id);

CREATE TABLE IF NOT EXISTS proxy_access_keys (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    owner_type text NOT NULL CHECK (owner_type IN ('route', 'smart_group')),
    owner_id text NOT NULL,
    key_hash text NOT NULL UNIQUE,
    key_ciphertext text NOT NULL,
    key_preview text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_type, owner_id)
);

CREATE INDEX IF NOT EXISTS idx_proxy_access_keys_workspace
ON proxy_access_keys (user_id, admin_account_id, owner_type);

CREATE TABLE IF NOT EXISTS proxy_route_models (
    route_id text NOT NULL REFERENCES proxy_routes(id) ON DELETE CASCADE,
    model_id text NOT NULL,
    model_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    refreshed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (route_id, model_id)
);

CREATE INDEX IF NOT EXISTS idx_proxy_route_models_model
ON proxy_route_models (model_id, route_id);

CREATE TABLE IF NOT EXISTS proxy_cleanup_jobs (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    route_id text NULL,
    site_id text NOT NULL,
    remote_key_id text NOT NULL DEFAULT '',
    remote_key_name text NOT NULL,
    status text NOT NULL DEFAULT 'creating',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz NULL,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz NULL
);

CREATE INDEX IF NOT EXISTS idx_proxy_cleanup_jobs_due
ON proxy_cleanup_jobs (status, next_attempt_at ASC)
WHERE status <> 'done';

CREATE INDEX IF NOT EXISTS idx_proxy_cleanup_jobs_site
ON proxy_cleanup_jobs (site_id, status);
