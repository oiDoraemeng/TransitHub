package model_proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"transithub/backend/internal/shared/httpjson"
)

type RecoveryProbeSettings interface {
	GetRecoveryProbeTimeouts(ctx context.Context, userID, accountID string) (first, subsequent int, err error)
}

func (s *Service) SetRecoveryProbeSettings(settings RecoveryProbeSettings) {
	s.probeSettings = settings
}

func (s *Service) recoveryProbeTimeout(ctx context.Context, route Route, attempts int64) (time.Duration, error) {
	first, subsequent := 60, 15
	if s.probeSettings != nil {
		var err error
		first, subsequent, err = s.probeSettings.GetRecoveryProbeTimeouts(ctx, route.UserID, route.AdminAccountID)
		if err != nil {
			return 0, err
		}
	}
	seconds := first
	if attempts > 0 {
		seconds = subsequent
	}
	if seconds < 1 || seconds > 300 {
		seconds = 60
		if attempts > 0 {
			seconds = 15
		}
	}
	return time.Duration(seconds) * time.Second, nil
}

// Keep a tombstone instead of resetting the epoch to zero: requests and probes
// already in flight must never recreate health state for a deleted route.
var removeRouteRuntimeScript = redis.NewScript(`
local epoch = tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') + 1
for _, id in ipairs(redis.call('SMEMBERS', KEYS[3])) do
  redis.call('ZREM', KEYS[4], id)
  redis.call('DEL', ARGV[2] .. id)
end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('DEL', KEYS[1], KEYS[3], KEYS[5], KEYS[6], KEYS[7], KEYS[8])
redis.call('HSET', KEYS[1], 'epoch', epoch, 'deleted', 1)
return 1
`)

func (l *Limiter) removeRouteRuntime(ctx context.Context, routeID string) error {
	return removeRouteRuntimeScript.Run(ctx, l.redis,
		[]string{healthKey(routeID), probeDueKey(), routeModelIndexKey(routeID), modelProbeDueKey(), leaseKey(routeID), latencyKey(routeID), manualTestKey(routeID), schedulerBalanceKey(routeID)},
		routeID, modelHealthKey("")).Err()
}

type RemoveRouteModelRequest struct {
	ModelID  string `json:"modelId"`
	Protocol string `json:"protocol"`
}

type RemovedRouteModel struct {
	ModelID  string `json:"modelId"`
	Protocol string `json:"protocol"`
}

// Removal is a manual exclusion, not recovery. Preserve isolation so catalog
// refreshes and late successes cannot put a permanently broken model in traffic.
var removeRouteModelScript = redis.NewScript(`
if redis.call('HGET', KEYS[3], 'deleted') == '1' then return 0 end
if redis.call('HGET', KEYS[1], 'isolated') ~= '1' then return 0 end
redis.call('HINCRBY', KEYS[1], 'epoch', 1)
redis.call('HSET', KEYS[1], 'probe_disabled', 1)
redis.call('HDEL', KEYS[1], 'probe_token', 'probe_spec')
redis.call('ZREM', KEYS[2], ARGV[1])
return 1
`)

func (l *Limiter) removeRouteModel(ctx context.Context, routeID, id string) (bool, error) {
	value, err := removeRouteModelScript.Run(ctx, l.redis,
		[]string{modelHealthKey(id), modelProbeDueKey(), healthKey(routeID)}, id).Int64()
	return value == 1, err
}

func (s *Service) RemoveRouteModel(ctx context.Context, userID, routeID string, input RemoveRouteModelRequest) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	route, err := s.repository.GetRoute(ctx, routeID)
	if err != nil {
		return err
	}
	if route == nil || route.UserID != userID || route.AdminAccountID != accountID {
		return pgx.ErrNoRows
	}
	input.ModelID = strings.TrimSpace(input.ModelID)
	input.Protocol = strings.TrimSpace(input.Protocol)
	switch input.Protocol {
	case "/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages", "gemini":
	default:
		return &requestError{Status: http.StatusBadRequest, Message: "invalid model protocol"}
	}
	// The UI supplies the actual upstream model, not a group alias.
	id := modelHealthID(Route{ID: routeID}, routeProbeSpec{ModelID: input.ModelID, Path: input.Protocol})
	if id == "" {
		return &requestError{Status: http.StatusBadRequest, Message: "model is required"}
	}
	// Persist the manual exclusion with the route. A cache flush or Redis
	// restart must never reactivate an explicitly removed model.
	tx, err := s.repository.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM proxy_routes WHERE id=$1 AND user_id=$2 AND admin_account_id=$3 FOR UPDATE`, routeID, userID, accountID).Scan(&lockedID); err != nil {
		return err
	}
	encoded, err := json.Marshal(RemovedRouteModel{ModelID: input.ModelID, Protocol: input.Protocol})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE proxy_routes SET removed_models=jsonb_set(removed_models,ARRAY[$2],$3::jsonb,true) WHERE id=$1`, routeID, id, encoded); err != nil {
		return err
	}
	removed, err := s.limiter.removeRouteModel(ctx, routeID, id)
	if err != nil {
		return err
	}
	if !removed {
		return &requestError{Status: http.StatusConflict, Message: "admin.modelProxy.circuit.modelChanged"}
	}
	return tx.Commit(ctx)
}

func (h *Handler) removeRouteModel(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input RemoveRouteModelRequest
	if err := httpjson.Decode(r, &input); err != nil {
		h.writeError(w, &requestError{Status: http.StatusBadRequest, Message: "invalid request body"})
		return
	}
	if err := h.service.RemoveRouteModel(r.Context(), userID, r.PathValue("id"), input); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}
