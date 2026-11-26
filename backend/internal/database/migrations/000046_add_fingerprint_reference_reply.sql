ALTER TABLE proxy_smart_groups ADD COLUMN fingerprint_reply_enabled boolean NOT NULL DEFAULT false;

-- Canonical model history is shared across keys and smart groups in a workspace.
-- Reserve under a row lock before responding; restarts do not reset the window.
CREATE TABLE proxy_fingerprint_history (
    user_id text NOT NULL,
    admin_account_id text NOT NULL,
    model text NOT NULL,
    sample_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, admin_account_id, model)
);
