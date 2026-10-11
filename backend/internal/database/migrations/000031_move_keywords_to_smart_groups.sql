ALTER TABLE proxy_smart_groups
    ADD COLUMN IF NOT EXISTS keywords text[] NOT NULL DEFAULT ARRAY[]::text[];

UPDATE proxy_smart_groups g
SET keywords = legacy.keywords
FROM (
    SELECT gm.smart_group_id, array_agg(DISTINCT expanded.keyword ORDER BY expanded.keyword) AS keywords
    FROM proxy_smart_group_members gm
    CROSS JOIN LATERAL unnest(COALESCE(gm.excluded_keywords, ARRAY[]::text[])) AS expanded(keyword)
    WHERE btrim(expanded.keyword) <> ''
    GROUP BY gm.smart_group_id
) legacy
WHERE g.id = legacy.smart_group_id
  AND cardinality(g.keywords) = 0;

ALTER TABLE proxy_smart_group_members
    DROP CONSTRAINT IF EXISTS proxy_smart_group_members_keyword_match_mode_check;

UPDATE proxy_smart_group_members
SET keyword_match_mode = 'disabled'
WHERE keyword_check_enabled = false;

ALTER TABLE proxy_smart_group_members
    ADD CONSTRAINT proxy_smart_group_members_keyword_match_mode_check
    CHECK (keyword_match_mode IN ('disabled', 'skip_on_match', 'require_match'));
