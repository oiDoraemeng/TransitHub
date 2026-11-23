ALTER TABLE proxy_smart_group_members
    ADD COLUMN IF NOT EXISTS media_input_enabled boolean NOT NULL DEFAULT false;

-- Preserve the existing media destination once while making future choices
-- explicit member policies rather than route-name conventions.
UPDATE proxy_smart_group_members m
SET media_input_enabled = true, policy_revision = policy_revision + 1
FROM proxy_routes r
WHERE r.id = m.route_id
  AND lower(regexp_replace(btrim(r.name), '\s+', ' ', 'g')) = 'gemini ultra';
