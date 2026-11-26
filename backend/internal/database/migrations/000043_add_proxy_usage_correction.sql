-- Opt-in estimated input usage caps, isolated by owner and current workspace.
CREATE TABLE IF NOT EXISTS proxy_usage_correction_settings (
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    allowance_percent integer NOT NULL DEFAULT 10 CHECK (allowance_percent BETWEEN 0 AND 1000),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, admin_account_id)
);
