package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository { return &Repository{db: db} }

func (r *Repository) ListRoutes(ctx context.Context, userID, accountID string) ([]Route, error) {
	rows, err := r.db.Query(ctx, `
		SELECT r.id, r.user_id, r.admin_account_id, r.name, r.site_id,
			COALESCE(s.name, ''), r.group_id, r.group_name, r.concurrency_limit, r.enabled,
			COALESCE(r.egress_proxy_id, ''), COALESCE(p.name, ''),
			COALESCE(k.key_preview, ''),
			(SELECT count(*) FROM proxy_route_models m WHERE m.route_id = r.id),
			(SELECT count(*) FROM proxy_cleanup_jobs j WHERE j.route_id = r.id AND j.status <> 'done'),
			r.model_synced_at, r.model_sync_error, r.created_at, r.updated_at
		FROM proxy_routes r
		LEFT JOIN upstream_sites s ON s.id = r.site_id
		LEFT JOIN model_egress_proxies p ON p.id = r.egress_proxy_id
		LEFT JOIN proxy_access_keys k ON k.owner_type = 'route' AND k.owner_id = r.id
		WHERE r.user_id = $1 AND r.admin_account_id = $2
		ORDER BY r.created_at ASC, r.id ASC
	`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Route, 0)
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, route)
	}
	return result, rows.Err()
}

func (r *Repository) GetRoute(ctx context.Context, routeID string) (*Route, error) {
	row := r.db.QueryRow(ctx, `
		SELECT r.id, r.user_id, r.admin_account_id, r.name, r.site_id,
			COALESCE(s.name, ''), r.group_id, r.group_name, r.concurrency_limit, r.enabled,
			COALESCE(r.egress_proxy_id, ''), COALESCE(p.name, ''),
			COALESCE(k.key_preview, ''),
			(SELECT count(*) FROM proxy_route_models m WHERE m.route_id = r.id),
			(SELECT count(*) FROM proxy_cleanup_jobs j WHERE j.route_id = r.id AND j.status <> 'done'),
			r.model_synced_at, r.model_sync_error, r.created_at, r.updated_at
		FROM proxy_routes r
		LEFT JOIN upstream_sites s ON s.id = r.site_id
		LEFT JOIN model_egress_proxies p ON p.id = r.egress_proxy_id
		LEFT JOIN proxy_access_keys k ON k.owner_type = 'route' AND k.owner_id = r.id
		WHERE r.id = $1
	`, routeID)
	route, err := scanRoute(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &route, err
}

type routeScanner interface{ Scan(...any) error }

func scanRoute(row routeScanner) (Route, error) {
	var route Route
	err := row.Scan(&route.ID, &route.UserID, &route.AdminAccountID, &route.Name, &route.SiteID,
		&route.SiteName, &route.GroupID, &route.GroupName, &route.ConcurrencyLimit, &route.Enabled,
		&route.ProxyID, &route.ProxyName,
		&route.KeyPreview, &route.ModelCount, &route.CleanupPending, &route.ModelSyncedAt,
		&route.ModelSyncError, &route.CreatedAt, &route.UpdatedAt)
	return route, err
}

func (r *Repository) CreateRoute(ctx context.Context, route Route, keyID, keyHash, ciphertext, preview string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO proxy_routes (id, user_id, admin_account_id, name, site_id, group_id, group_name, concurrency_limit, enabled, egress_proxy_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''))
	`, route.ID, route.UserID, route.AdminAccountID, route.Name, route.SiteID, route.GroupID, route.GroupName, route.ConcurrencyLimit, route.Enabled, route.ProxyID); err != nil {
		return err
	}
	if err := insertAccessKey(ctx, tx, keyID, route.UserID, route.AdminAccountID, OwnerRoute, route.ID, keyHash, ciphertext, preview); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) UpdateRoute(ctx context.Context, route Route) error {
	result, err := r.db.Exec(ctx, `
		UPDATE proxy_routes SET name=$4, site_id=$5, group_id=$6, group_name=$7,
			concurrency_limit=$8, enabled=$9, egress_proxy_id=NULLIF($10,''), updated_at=now()
		WHERE id=$1 AND user_id=$2 AND admin_account_id=$3
	`, route.ID, route.UserID, route.AdminAccountID, route.Name, route.SiteID, route.GroupID,
		route.GroupName, route.ConcurrencyLimit, route.Enabled, route.ProxyID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) DeleteRoute(ctx context.Context, userID, accountID, routeID string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var references int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM proxy_smart_group_members WHERE route_id=$1`, routeID).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return &requestError{Status: 409, Message: "route is still used by a smart group"}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM proxy_access_keys WHERE owner_type='route' AND owner_id=$1 AND user_id=$2 AND admin_account_id=$3`, routeID, userID, accountID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `DELETE FROM proxy_routes WHERE id=$1 AND user_id=$2 AND admin_account_id=$3`, routeID, userID, accountID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

func (r *Repository) SiteInUse(ctx context.Context, siteID string) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM proxy_routes WHERE site_id=$1 UNION ALL SELECT 1 FROM proxy_cleanup_jobs WHERE site_id=$1 AND status <> 'done')`, siteID).Scan(&exists)
	return exists, err
}

func insertAccessKey(ctx context.Context, tx pgx.Tx, id, userID, accountID, ownerType, ownerID, hash, ciphertext, preview string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO proxy_access_keys (id,user_id,admin_account_id,owner_type,owner_id,key_hash,key_ciphertext,key_preview)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, id, userID, accountID, ownerType, ownerID, hash, ciphertext, preview)
	return err
}

func (r *Repository) FindCredential(ctx context.Context, hash string) (*Credential, error) {
	var value Credential
	err := r.db.QueryRow(ctx, `SELECT owner_type,owner_id,user_id,admin_account_id FROM proxy_access_keys WHERE key_hash=$1`, hash).
		Scan(&value.OwnerType, &value.OwnerID, &value.UserID, &value.AdminAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &value, err
}

func (r *Repository) RevealKey(ctx context.Context, userID, accountID, ownerType, ownerID string) (string, string, error) {
	var ciphertext, preview string
	err := r.db.QueryRow(ctx, `
		SELECT key_ciphertext,key_preview FROM proxy_access_keys
		WHERE user_id=$1 AND admin_account_id=$2 AND owner_type=$3 AND owner_id=$4
	`, userID, accountID, ownerType, ownerID).Scan(&ciphertext, &preview)
	return ciphertext, preview, err
}

func (r *Repository) RotateKey(ctx context.Context, userID, accountID, ownerType, ownerID, hash, ciphertext, preview string) error {
	result, err := r.db.Exec(ctx, `
		UPDATE proxy_access_keys SET key_hash=$5,key_ciphertext=$6,key_preview=$7,updated_at=now()
		WHERE user_id=$1 AND admin_account_id=$2 AND owner_type=$3 AND owner_id=$4
	`, userID, accountID, ownerType, ownerID, hash, ciphertext, preview)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) CreateSmartGroup(ctx context.Context, group SmartGroup, routeIDs []string, keyID, keyHash, ciphertext, preview string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO proxy_smart_groups (id,user_id,admin_account_id,name,enabled) VALUES ($1,$2,$3,$4,$5)`, group.ID, group.UserID, group.AdminAccountID, group.Name, group.Enabled); err != nil {
		return err
	}
	for _, routeID := range routeIDs {
		result, err := tx.Exec(ctx, `
			INSERT INTO proxy_smart_group_members (smart_group_id,route_id)
			SELECT $1,r.id FROM proxy_routes r WHERE r.id=$2 AND r.user_id=$3 AND r.admin_account_id=$4
		`, group.ID, routeID, group.UserID, group.AdminAccountID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return &requestError{Status: 400, Message: "member key does not belong to this workspace"}
		}
	}
	if err := insertAccessKey(ctx, tx, keyID, group.UserID, group.AdminAccountID, OwnerSmartGroup, group.ID, keyHash, ciphertext, preview); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) ListSmartGroups(ctx context.Context, userID, accountID string) ([]SmartGroup, error) {
	rows, err := r.db.Query(ctx, `
		SELECT g.id,g.user_id,g.admin_account_id,g.name,g.enabled,COALESCE(k.key_preview,''),g.created_at,g.updated_at
		FROM proxy_smart_groups g
		LEFT JOIN proxy_access_keys k ON k.owner_type='smart_group' AND k.owner_id=g.id
		WHERE g.user_id=$1 AND g.admin_account_id=$2 ORDER BY g.created_at ASC,g.id ASC
	`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]SmartGroup, 0)
	for rows.Next() {
		var group SmartGroup
		if err := rows.Scan(&group.ID, &group.UserID, &group.AdminAccountID, &group.Name, &group.Enabled, &group.KeyPreview, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range groups {
		members, err := r.ListGroupRoutes(ctx, groups[index].ID, false, "")
		if err != nil {
			return nil, err
		}
		groups[index].Members = members
		models, err := r.GroupModels(ctx, groups[index].ID)
		if err != nil {
			return nil, err
		}
		groups[index].Models = models
		for _, member := range members {
			if member.Enabled {
				groups[index].TotalConcurrency += member.ConcurrencyLimit
			}
		}
	}
	return groups, nil
}

func (r *Repository) GetSmartGroup(ctx context.Context, groupID string) (*SmartGroup, error) {
	var group SmartGroup
	err := r.db.QueryRow(ctx, `
		SELECT g.id,g.user_id,g.admin_account_id,g.name,g.enabled,COALESCE(k.key_preview,''),g.created_at,g.updated_at
		FROM proxy_smart_groups g LEFT JOIN proxy_access_keys k ON k.owner_type='smart_group' AND k.owner_id=g.id
		WHERE g.id=$1
	`, groupID).Scan(&group.ID, &group.UserID, &group.AdminAccountID, &group.Name, &group.Enabled, &group.KeyPreview, &group.CreatedAt, &group.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	group.Members, err = r.ListGroupRoutes(ctx, groupID, false, "")
	if err != nil {
		return nil, err
	}
	group.Models, err = r.GroupModels(ctx, groupID)
	for _, member := range group.Members {
		if member.Enabled {
			group.TotalConcurrency += member.ConcurrencyLimit
		}
	}
	return &group, err
}

func (r *Repository) UpdateSmartGroup(ctx context.Context, group SmartGroup) error {
	result, err := r.db.Exec(ctx, `UPDATE proxy_smart_groups SET name=$4,enabled=$5,updated_at=now() WHERE id=$1 AND user_id=$2 AND admin_account_id=$3`, group.ID, group.UserID, group.AdminAccountID, group.Name, group.Enabled)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) DeleteSmartGroup(ctx context.Context, userID, accountID, groupID string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM proxy_access_keys WHERE owner_type='smart_group' AND owner_id=$1 AND user_id=$2 AND admin_account_id=$3`, groupID, userID, accountID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `DELETE FROM proxy_smart_groups WHERE id=$1 AND user_id=$2 AND admin_account_id=$3`, groupID, userID, accountID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

func (r *Repository) AddMember(ctx context.Context, userID, accountID, groupID, routeID string) error {
	result, err := r.db.Exec(ctx, `
		INSERT INTO proxy_smart_group_members (smart_group_id,route_id)
		SELECT g.id,r.id FROM proxy_smart_groups g JOIN proxy_routes r ON r.id=$4
		WHERE g.id=$3 AND g.user_id=$1 AND g.admin_account_id=$2 AND r.user_id=$1 AND r.admin_account_id=$2
		ON CONFLICT DO NOTHING
	`, userID, accountID, groupID, routeID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return &requestError{Status: 409, Message: "member already exists or is unavailable"}
	}
	return nil
}

func (r *Repository) RemoveMember(ctx context.Context, userID, accountID, groupID, routeID string) error {
	var count int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM proxy_smart_group_members WHERE smart_group_id=$1`, groupID).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return &requestError{Status: 409, Message: "a smart group must keep at least one member"}
	}
	result, err := r.db.Exec(ctx, `
		DELETE FROM proxy_smart_group_members m USING proxy_smart_groups g
		WHERE m.smart_group_id=g.id AND g.id=$1 AND m.route_id=$2 AND g.user_id=$3 AND g.admin_account_id=$4
	`, groupID, routeID, userID, accountID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) ResolveRouteKey(ctx context.Context, hash, userID, accountID string) (string, error) {
	var routeID string
	err := r.db.QueryRow(ctx, `SELECT owner_id FROM proxy_access_keys WHERE key_hash=$1 AND user_id=$2 AND admin_account_id=$3 AND owner_type='route'`, hash, userID, accountID).Scan(&routeID)
	return routeID, err
}

func (r *Repository) ListGroupRoutes(ctx context.Context, groupID string, enabledOnly bool, modelID string) ([]Route, error) {
	query := `
		SELECT r.id,r.user_id,r.admin_account_id,r.name,r.site_id,COALESCE(s.name,''),r.group_id,r.group_name,
			r.concurrency_limit,r.enabled,COALESCE(r.egress_proxy_id,''),COALESCE(p.name,''),COALESCE(k.key_preview,''),
			(SELECT count(*) FROM proxy_route_models mc WHERE mc.route_id=r.id),
			(SELECT count(*) FROM proxy_cleanup_jobs j WHERE j.route_id=r.id AND j.status <> 'done'),
			r.model_synced_at,r.model_sync_error,r.created_at,r.updated_at
		FROM proxy_smart_group_members gm JOIN proxy_routes r ON r.id=gm.route_id
		LEFT JOIN upstream_sites s ON s.id=r.site_id
		LEFT JOIN model_egress_proxies p ON p.id=r.egress_proxy_id
		LEFT JOIN proxy_access_keys k ON k.owner_type='route' AND k.owner_id=r.id
		WHERE gm.smart_group_id=$1`
	args := []any{groupID}
	if enabledOnly {
		query += ` AND r.enabled=true`
	}
	if strings.TrimSpace(modelID) != "" {
		query += ` AND EXISTS(SELECT 1 FROM proxy_route_models m WHERE m.route_id=r.id AND m.model_id=$2)`
		args = append(args, modelID)
	}
	query += ` ORDER BY r.created_at ASC,r.id ASC`
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []Route
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (r *Repository) ReplaceRouteModels(ctx context.Context, routeID string, models []Model) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM proxy_route_models WHERE route_id=$1`, routeID); err != nil {
		return err
	}
	now := time.Now()
	for _, model := range models {
		raw := model.Raw
		if raw == nil {
			raw = map[string]any{"id": model.ID, "object": model.Object, "owned_by": model.OwnedBy}
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO proxy_route_models (route_id,model_id,model_json,refreshed_at) VALUES ($1,$2,$3::jsonb,$4)`, routeID, model.ID, string(encoded), now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE proxy_routes SET model_synced_at=$2,model_sync_error='',updated_at=now() WHERE id=$1`, routeID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) SetModelSyncError(ctx context.Context, routeID string, syncErr error) error {
	message := ""
	if syncErr != nil {
		message = syncErr.Error()
		if len(message) > 500 {
			message = message[:500]
		}
	}
	_, err := r.db.Exec(ctx, `UPDATE proxy_routes SET model_sync_error=$2,updated_at=now() WHERE id=$1`, routeID, message)
	return err
}

func (r *Repository) GroupModels(ctx context.Context, groupID string) ([]Model, error) {
	rows, err := r.db.Query(ctx, `
		SELECT m.model_id, min(m.model_json::text)::jsonb,
			COALESCE(sum(r.concurrency_limit),0)
		FROM proxy_smart_group_members gm
		JOIN proxy_routes r ON r.id=gm.route_id AND r.enabled=true
		JOIN proxy_route_models m ON m.route_id=r.id
		WHERE gm.smart_group_id=$1
		GROUP BY m.model_id ORDER BY m.model_id ASC
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Model
	for rows.Next() {
		var model Model
		var rawJSON []byte
		if err := rows.Scan(&model.ID, &rawJSON, &model.EffectiveConcurrency); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(rawJSON, &model.Raw)
		model.Object, _ = model.Raw["object"].(string)
		model.OwnedBy, _ = model.Raw["owned_by"].(string)
		result = append(result, model)
	}
	return result, rows.Err()
}

func (r *Repository) RoutesNeedingRefresh(ctx context.Context, staleBefore time.Time) ([]Route, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT r.id,r.user_id,r.admin_account_id,r.name,r.site_id,COALESCE(s.name,''),r.group_id,r.group_name,
			r.concurrency_limit,r.enabled,COALESCE(r.egress_proxy_id,''),COALESCE(p.name,''),COALESCE(k.key_preview,''),
			(SELECT count(*) FROM proxy_route_models mc WHERE mc.route_id=r.id),
			(SELECT count(*) FROM proxy_cleanup_jobs j WHERE j.route_id=r.id AND j.status <> 'done'),
			r.model_synced_at,r.model_sync_error,r.created_at,r.updated_at
		FROM proxy_routes r JOIN proxy_smart_group_members gm ON gm.route_id=r.id
		LEFT JOIN upstream_sites s ON s.id=r.site_id
		LEFT JOIN model_egress_proxies p ON p.id=r.egress_proxy_id
		LEFT JOIN proxy_access_keys k ON k.owner_type='route' AND k.owner_id=r.id
		WHERE r.enabled=true AND (r.model_synced_at IS NULL OR r.model_synced_at < $1)
	`, staleBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Route
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, route)
	}
	return result, rows.Err()
}

func (r *Repository) CreateCleanupJob(ctx context.Context, job CleanupJob) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO proxy_cleanup_jobs (id,user_id,admin_account_id,route_id,site_id,remote_key_name,status)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,'creating')
	`, job.ID, job.UserID, job.AdminAccountID, job.RouteID, job.SiteID, job.RemoteKeyName)
	return err
}

func (r *Repository) ActivateCleanupJob(ctx context.Context, id, remoteKeyID string) error {
	_, err := r.db.Exec(ctx, `UPDATE proxy_cleanup_jobs SET remote_key_id=$2,status='active',next_attempt_at=now(),updated_at=now() WHERE id=$1`, id, remoteKeyID)
	return err
}

func (r *Repository) MarkCleanupPending(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, `UPDATE proxy_cleanup_jobs SET status='delete_pending',next_attempt_at=now(),locked_until=NULL,updated_at=now() WHERE id=$1 AND status <> 'done'`, id)
	return err
}

func (r *Repository) CompleteCleanupJob(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, `UPDATE proxy_cleanup_jobs SET status='done',last_error='',locked_until=NULL,completed_at=now(),updated_at=now() WHERE id=$1`, id)
	return err
}

func (r *Repository) RetryCleanupJob(ctx context.Context, id string, attempts int, retryAt time.Time, cleanupErr error) error {
	message := cleanupErr.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := r.db.Exec(ctx, `UPDATE proxy_cleanup_jobs SET status='delete_pending',attempts=$2,next_attempt_at=$3,locked_until=NULL,last_error=$4,updated_at=now() WHERE id=$1`, id, attempts, retryAt, message)
	return err
}

func (r *Repository) ClaimCleanupJobs(ctx context.Context, limit int) ([]CleanupJob, error) {
	rows, err := r.db.Query(ctx, `
		WITH due AS (
			SELECT id FROM proxy_cleanup_jobs
			WHERE (
				(status = 'delete_pending' AND next_attempt_at <= now())
				OR (status IN ('creating','active') AND updated_at < now()-interval '2 minutes')
			)
			AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_attempt_at ASC LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		UPDATE proxy_cleanup_jobs j SET locked_until=now()+interval '30 seconds',updated_at=now()
		FROM due WHERE j.id=due.id
		RETURNING j.id,j.user_id,j.admin_account_id,COALESCE(j.route_id,''),j.site_id,j.remote_key_id,j.remote_key_name,j.status,j.attempts,j.last_error
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CleanupJob
	for rows.Next() {
		var job CleanupJob
		if err := rows.Scan(&job.ID, &job.UserID, &job.AdminAccountID, &job.RouteID, &job.SiteID, &job.RemoteKeyID, &job.RemoteKeyName, &job.Status, &job.Attempts, &job.LastError); err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (r *Repository) PurgeCompletedCleanupJobs(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `DELETE FROM proxy_cleanup_jobs WHERE status='done' AND completed_at < now()-interval '24 hours'`)
	return err
}

func (r *Repository) CleanupSummary(ctx context.Context, userID, accountID string) (map[string]any, error) {
	var pending, retrying int
	var lastError string
	err := r.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status <> 'done'),
			count(*) FILTER (WHERE status <> 'done' AND attempts > 0),
			COALESCE((array_agg(last_error ORDER BY updated_at DESC) FILTER (WHERE last_error <> ''))[1],'')
		FROM proxy_cleanup_jobs WHERE user_id=$1 AND admin_account_id=$2
	`, userID, accountID).Scan(&pending, &retrying, &lastError)
	return map[string]any{"pending": pending, "retrying": retrying, "lastError": lastError}, err
}

func (r *Repository) ValidateOwner(ctx context.Context, userID, accountID, ownerType, ownerID string) error {
	table := "proxy_routes"
	if ownerType == OwnerSmartGroup {
		table = "proxy_smart_groups"
	}
	var exists bool
	query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id=$1 AND user_id=$2 AND admin_account_id=$3)`, table)
	if err := r.db.QueryRow(ctx, query, ownerID, userID, accountID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return pgx.ErrNoRows
	}
	return nil
}
