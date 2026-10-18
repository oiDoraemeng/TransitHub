package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const manualRecoveryTTL = 5 * time.Minute

type RouteTestRequest struct {
	GroupID  string `json:"groupId,omitempty"`
	ModelID  string `json:"modelId"`
	Protocol string `json:"protocol"`
}

type RouteTestResult struct {
	Success       bool       `json:"success"`
	LatencyMS     int64      `json:"latencyMs"`
	StatusCode    int        `json:"statusCode,omitempty"`
	Message       string     `json:"message"`
	RecoveryToken string     `json:"recoveryToken,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
}

type RouteRecoveryRequest struct {
	GroupID string `json:"groupId,omitempty"`
	Token   string `json:"token"`
}

type manualRecoveryProof struct {
	TokenHash              string `json:"tokenHash"`
	Epoch                  int64  `json:"epoch"`
	Fingerprint            string `json:"fingerprint"`
	ModelHealthID          string `json:"modelHealthId,omitempty"`
	ModelEpoch             int64  `json:"modelEpoch"`
	RequestedModelHealthID string `json:"requestedModelHealthId,omitempty"`
	RequestedModelEpoch    int64  `json:"requestedModelEpoch,omitempty"`
}

func manualTestKey(routeID string) string { return "proxy:manual-test:{routes}:" + routeID }

func manualRecoveryKey(userID, accountID, routeID, groupID string) string {
	encoded, _ := json.Marshal([]string{userID, accountID, routeID, groupID})
	return "proxy:manual-recovery:{routes}:" + hashKey(string(encoded))
}

// Manual inference bypasses the circuit, but shares the hard concurrency limit.
// Its own lock never takes over or reschedules an automatic recovery probe.
var acquireManualTestScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
if redis.call('HGET', KEYS[2], 'deleted') == '1' then return {-2, 0} end
resumeExpired(KEYS[2], KEYS[3], ARGV[1], now)
if redis.call('EXISTS', KEYS[4]) == 1 then return {-2, 0} end
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then return {0, 0} end
redis.call('SET', KEYS[4], ARGV[2], 'PX', ARGV[4])
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[4]), ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('DEL', KEYS[5])
return {1, tonumber(redis.call('HGET', KEYS[2], 'epoch') or '0'),
        tonumber(redis.call('HGET', KEYS[6], 'epoch') or '0'),
        tonumber(redis.call('HGET', KEYS[7], 'epoch') or '0')}
`)

var releaseManualTestScript = redis.NewScript(`
redis.call('ZREM', KEYS[1], ARGV[1])
if redis.call('GET', KEYS[2]) == ARGV[2] then redis.call('DEL', KEYS[2]) end
return 1
`)

var saveManualRecoveryScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
resumeExpired(KEYS[1], KEYS[2], ARGV[1], now)
if redis.call('GET', KEYS[3]) ~= ARGV[2]
   or tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') ~= tonumber(ARGV[3])
   or tonumber(redis.call('HGET', KEYS[5], 'epoch') or '0') ~= tonumber(ARGV[6])
   or tonumber(redis.call('HGET', KEYS[6], 'epoch') or '0') ~= tonumber(ARGV[7]) then return 0 end
redis.call('SET', KEYS[4], ARGV[4], 'PX', ARGV[5])
return now + tonumber(ARGV[5])
`)

var restoreManualRecoveryScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
resumeExpired(KEYS[1], KEYS[2], ARGV[1], now)
if redis.call('GET', KEYS[3]) ~= ARGV[2]
   or redis.call('EXISTS', KEYS[4]) == 1
   or tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') ~= tonumber(ARGV[3])
   or tonumber(redis.call('HGET', KEYS[6], 'epoch') or '0') ~= tonumber(ARGV[5])
   or (ARGV[6] ~= '' and tonumber(redis.call('HGET', KEYS[8], 'epoch') or '0') ~= tonumber(ARGV[7])) then return 0 end
redis.call('DEL', KEYS[3], KEYS[5])
redis.call('HINCRBY', KEYS[1], 'epoch', 1)
redis.call('HSET', KEYS[1], 'failures', 0)
redis.call('HDEL', KEYS[1], 'paused_until', 'probe_token', 'probe_spec')
redis.call('ZREM', KEYS[2], ARGV[1])
if ARGV[4] ~= '' then
  redis.call('HINCRBY', KEYS[6], 'epoch', 1)
  redis.call('HSET', KEYS[6], 'failures', 0, 'probe_attempts', 0)
  redis.call('HDEL', KEYS[6], 'isolated', 'isolated_at', 'probe_token', 'probe_spec', 'probe_disabled')
  redis.call('ZREM', KEYS[7], ARGV[4])
end
if ARGV[6] ~= '' then
  redis.call('HINCRBY', KEYS[8], 'epoch', 1)
  redis.call('HSET', KEYS[8], 'failures', 0, 'probe_attempts', 0)
  redis.call('HDEL', KEYS[8], 'isolated', 'isolated_at', 'probe_token', 'probe_spec', 'probe_disabled')
  redis.call('ZREM', KEYS[7], ARGV[6])
end
return 1
`)

func (l *Limiter) acquireManualTest(ctx context.Context, route Route, proofKey string, specs ...routeProbeSpec) (*Lease, error) {
	token, err := randomID("manual_")
	if err != nil {
		return nil, err
	}
	modelID := ""
	requestedID := ""
	if len(specs) > 0 {
		modelID = modelHealthID(route, specs[0])
		requestedID = modelHealthID(Route{ID: route.ID}, specs[0])
		if requestedID == modelID {
			requestedID = ""
		}
	}
	values, err := acquireManualTestScript.Run(ctx, l.redis,
		[]string{leaseKey(route.ID), healthKey(route.ID), probeDueKey(), manualTestKey(route.ID), proofKey, modelHealthKey(modelID), modelHealthKey(requestedID)},
		route.ID, token, route.ConcurrencyLimit, leaseTTL.Milliseconds()).Int64Slice()
	if err != nil {
		return nil, err
	}
	if len(values) < 2 {
		return nil, errors.New("invalid manual test lease response")
	}
	if values[0] != 1 {
		return nil, &requestError{Status: http.StatusConflict, Message: "admin.modelProxy.manualTest.busy"}
	}
	if len(values) != 4 {
		return nil, errors.New("invalid manual model lease response")
	}
	lease := &Lease{limiter: l, routeID: route.ID, id: token, epoch: values[1], modelHealthID: modelID, modelEpoch: values[2], requestedModelHealthID: requestedID, requestedModelEpoch: values[3], manualTestToken: token, stop: make(chan struct{})}
	go lease.renew()
	return lease, nil
}

func (s *Service) manualTestRoute(ctx context.Context, userID, routeID, groupID string) (*Route, string, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	route, err := s.repository.GetRoute(ctx, routeID)
	if err != nil {
		return nil, "", err
	}
	if route == nil || route.UserID != userID || route.AdminAccountID != accountID {
		return nil, "", pgx.ErrNoRows
	}
	if groupID != "" {
		if err := s.repository.ValidateOwner(ctx, userID, accountID, OwnerSmartGroup, groupID); err != nil {
			return nil, "", err
		}
		group, err := s.repository.GetSmartGroup(ctx, groupID)
		if err != nil {
			return nil, "", err
		}
		if group == nil || !group.Enabled {
			return nil, "", &requestError{Status: http.StatusConflict, Message: "admin.modelProxy.manualTest.disabled"}
		}
		members, err := s.repository.ListGroupRoutes(ctx, groupID, false, "")
		if err != nil {
			return nil, "", err
		}
		route = nil
		for index := range members {
			if members[index].ID == routeID {
				route = &members[index]
				break
			}
		}
		if route == nil {
			return nil, "", pgx.ErrNoRows
		}
	}
	if !route.Enabled {
		return nil, "", &requestError{Status: http.StatusConflict, Message: "admin.modelProxy.manualTest.disabled"}
	}
	return route, manualRecoveryKey(userID, accountID, routeID, groupID), nil
}

func (s *Service) manualTestFingerprint(ctx context.Context, route Route) (string, error) {
	site, err := s.sites.GetSite(ctx, route.SiteID)
	if err != nil || site == nil {
		if err == nil {
			err = errors.New("route site unavailable")
		}
		return "", err
	}
	proxyVersion := ""
	if route.ProxyID != "" {
		proxy, err := s.repository.GetEgressProxy(ctx, route.ProxyID)
		if err != nil || proxy == nil {
			if err == nil {
				err = errors.New("route proxy unavailable")
			}
			return "", err
		}
		proxyVersion = proxy.URLCiphertext + ":" + proxy.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	// Ignore catalog-refresh timestamps, which do not change the tested binding.
	encoded, err := json.Marshal([]any{route.ID, route.SiteID, site.BaseURL, route.GroupID,
		route.ProxyID, proxyVersion, route.Enabled, route.ConcurrencyLimit, route.UseProvidedKey,
		route.UpstreamKeyCiphertext, route.ModelMappingEnabled, route.ModelMapping,
		route.SchedulingPaused, route.MemberPolicyRevision})
	if err != nil {
		return "", err
	}
	return hashKey(string(encoded)), nil
}

func (s *Service) ListRouteTestModels(ctx context.Context, userID, routeID, groupID string, refresh bool) ([]Model, error) {
	route, proofKey, err := s.manualTestRoute(ctx, userID, routeID, groupID)
	if err != nil {
		return nil, err
	}
	if refresh {
		if s.limiter == nil {
			return nil, errors.New("route limiter unavailable")
		}
		lease, err := s.limiter.acquireManualTest(ctx, *route, proofKey)
		if err != nil {
			return nil, err
		}
		defer lease.Release(context.WithoutCancel(ctx))
		probeCtx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
		defer cancel()
		// Keep a manual refresh separate from a background operation that may
		// already be using an older credential or proxy configuration.
		if err := s.refreshRouteModelsInScope(probeCtx, *route, proofKey); err != nil {
			return nil, &requestError{Status: http.StatusBadGateway, Message: "admin.modelProxy.manualTest.modelsFailed"}
		}
	}
	rows, err := s.repository.db.Query(ctx, `SELECT model_id FROM proxy_route_models WHERE route_id=$1 ORDER BY model_id`, routeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	available := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		available[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Testing accepts native models as well as every configured member alias.
	// Aliases are virtual public model names and must remain selectable even when
	// the upstream catalog is incomplete or contains only the shared target model.
	// The actual target is still applied by performAttemptWithResponseHeaderTimeout
	// before the request is sent upstream.
	selectable := make(map[string]bool, len(available)+len(route.ModelMapping))
	for id := range available {
		selectable[id] = true
	}
	if route.ModelMappingEnabled {
		for alias, target := range route.ModelMapping {
			if strings.TrimSpace(alias) != "" && strings.TrimSpace(target) != "" {
				selectable[alias] = true
			}
		}
	}
	models := make([]Model, 0, len(selectable))
	for id := range selectable {
		models = append(models, Model{ID: id, Object: "model"})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func manualProbeSpec(input RouteTestRequest) (routeProbeSpec, error) {
	model := strings.TrimSpace(input.ModelID)
	spec := routeProbeSpec{GroupID: input.GroupID, ModelID: model}
	if model == "" || len(model) > 512 {
		return spec, &requestError{Status: 400, Message: "admin.modelProxy.manualTest.modelRequired"}
	}
	switch input.Protocol {
	case "chat":
		spec.Path = "/v1/chat/completions"
	case "responses":
		spec.Path = "/v1/responses"
	case "compact":
		spec.Path = "/v1/responses/compact"
	case "messages":
		spec.Path = "/v1/messages"
	case "gemini":
		name := strings.TrimPrefix(model, "models/")
		if name == "" || strings.ContainsAny(name, "/\\:?#") || strings.Contains(name, "..") {
			return spec, &requestError{Status: 400, Message: "admin.modelProxy.manualTest.modelRequired"}
		}
		spec.Path = "/v1beta/models/" + name + ":generateContent"
	default:
		return spec, &requestError{Status: 400, Message: "admin.modelProxy.manualTest.protocolRequired"}
	}
	return spec, nil
}

func (s *Service) TestRouteModel(ctx context.Context, userID, routeID string, input RouteTestRequest) (RouteTestResult, error) {
	spec, err := manualProbeSpec(input)
	if err != nil {
		return RouteTestResult{}, err
	}
	route, proofKey, err := s.manualTestRoute(ctx, userID, routeID, input.GroupID)
	if err != nil {
		return RouteTestResult{}, err
	}
	fingerprint, err := s.manualTestFingerprint(ctx, *route)
	if err != nil {
		return RouteTestResult{}, err
	}
	if s.limiter == nil {
		return RouteTestResult{}, errors.New("route limiter unavailable")
	}
	lease, err := s.limiter.acquireManualTest(ctx, *route, proofKey, spec)
	if err != nil {
		return RouteTestResult{}, err
	}
	defer lease.Release(context.WithoutCancel(ctx))
	probeCtx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
	defer cancel()
	result := s.executeRouteProbe(probeCtx, *route, spec)
	if !result.Success {
		return result, nil
	}
	token, err := randomID("recovery_")
	if err != nil {
		return RouteTestResult{}, err
	}
	encoded, err := json.Marshal(manualRecoveryProof{
		TokenHash: hashKey(token), Epoch: lease.epoch, Fingerprint: fingerprint,
		ModelHealthID: lease.modelHealthID, ModelEpoch: lease.modelEpoch,
		RequestedModelHealthID: lease.requestedModelHealthID, RequestedModelEpoch: lease.requestedModelEpoch,
	})
	if err != nil {
		return RouteTestResult{}, err
	}
	expires, err := saveManualRecoveryScript.Run(ctx, s.limiter.redis,
		[]string{healthKey(route.ID), probeDueKey(), manualTestKey(route.ID), proofKey, modelHealthKey(lease.modelHealthID), modelHealthKey(lease.requestedModelHealthID)},
		route.ID, lease.manualTestToken, lease.epoch, string(encoded), manualRecoveryTTL.Milliseconds(), lease.modelEpoch, lease.requestedModelEpoch).Int64()
	if err != nil {
		return RouteTestResult{}, err
	}
	if expires == 0 {
		result.Message = "admin.modelProxy.manualTest.stateChanged"
		return result, nil
	}
	expiresAt := time.UnixMilli(expires).UTC()
	result.RecoveryToken, result.ExpiresAt = token, &expiresAt
	return result, nil
}

func (s *Service) RestoreRouteScheduling(ctx context.Context, userID, routeID string, input RouteRecoveryRequest) error {
	if input.Token == "" || len(input.Token) > 128 {
		return &requestError{Status: 409, Message: "admin.modelProxy.manualTest.retest"}
	}
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	// Serialize restoration with route edits, member policy updates and manual pauses.
	tx, err := s.repository.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM proxy_routes WHERE id=$1 AND user_id=$2 AND admin_account_id=$3 FOR UPDATE`, routeID, userID, accountID).Scan(&id); err != nil {
		return err
	}
	if input.GroupID != "" {
		if err := tx.QueryRow(ctx, `SELECT id FROM proxy_smart_groups WHERE id=$1 AND user_id=$2 AND admin_account_id=$3 FOR UPDATE`, input.GroupID, userID, accountID).Scan(&id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT route_id FROM proxy_smart_group_members WHERE smart_group_id=$1 AND route_id=$2 FOR UPDATE`, input.GroupID, routeID).Scan(&id); err != nil {
			return err
		}
	}
	route, proofKey, err := s.manualTestRoute(ctx, userID, routeID, input.GroupID)
	if err != nil {
		return err
	}
	fingerprint, err := s.manualTestFingerprint(ctx, *route)
	if err != nil {
		return err
	}
	if s.limiter == nil {
		return errors.New("route limiter unavailable")
	}
	encoded, err := s.limiter.redis.Get(ctx, proofKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	var proof manualRecoveryProof
	if err := json.Unmarshal([]byte(encoded), &proof); err != nil || proof.TokenHash != hashKey(input.Token) || proof.Fingerprint != fingerprint {
		return &requestError{Status: 409, Message: "admin.modelProxy.manualTest.retest"}
	}
	if proof.ModelHealthID != "" {
		// Successful mapped inference validates both the requested name and the
		// current target for this protocol. Old exclusions may predate the mapping.
		if _, err := tx.Exec(ctx, `UPDATE proxy_routes SET removed_models=removed_models-$2-$3 WHERE id=$1`, routeID, proof.ModelHealthID, proof.RequestedModelHealthID); err != nil {
			return err
		}
	}
	if input.GroupID != "" {
		if _, err := tx.Exec(ctx, `UPDATE proxy_smart_group_members SET scheduling_paused=false,policy_revision=policy_revision+1 WHERE smart_group_id=$1 AND route_id=$2`, input.GroupID, routeID); err != nil {
			return err
		}
	}
	stateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	value, err := restoreManualRecoveryScript.Run(stateCtx, s.limiter.redis,
		[]string{healthKey(routeID), probeDueKey(), proofKey, manualTestKey(routeID), latencyKey(routeID), modelHealthKey(proof.ModelHealthID), modelProbeDueKey(), modelHealthKey(proof.RequestedModelHealthID)},
		routeID, encoded, proof.Epoch, proof.ModelHealthID, proof.ModelEpoch, proof.RequestedModelHealthID, proof.RequestedModelEpoch).Int64()
	if err != nil {
		return err
	}
	if value != 1 {
		return &requestError{Status: 409, Message: "admin.modelProxy.manualTest.retest"}
	}
	return tx.Commit(ctx)
}
