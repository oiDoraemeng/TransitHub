package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var errModelPaused = errors.New("requested upstream model is temporarily isolated")

func modelProbeDueKey() string                 { return "proxy:model-probe-due:{routes}" }
func modelHealthKey(id string) string          { return "proxy:model-health:{routes}:" + id }
func routeModelIndexKey(routeID string) string { return "proxy:model-index:{routes}:" + routeID }

// Aliases for the same actual upstream model share a circuit. Different API
// protocols remain independent: a model can support Chat but not Responses.
func modelHealthIdentity(route Route, spec routeProbeSpec) (string, string) {
	model := strings.TrimSpace(spec.ModelID)
	if route.ModelMappingEnabled {
		if target := strings.TrimSpace(route.ModelMapping[model]); target != "" {
			model = target
		}
	}
	protocol := spec.Path
	if _, _, ok := nativeGeminiModelAction(protocol); ok {
		protocol = "gemini"
	}
	return model, protocol
}

func modelHealthID(route Route, spec routeProbeSpec) string {
	model, protocol := modelHealthIdentity(route, spec)
	if model == "" || spec.Path == "/v1/models" || spec.Path == "/v1beta/models" || spec.Path == embeddingsPath {
		return ""
	}
	encoded, _ := json.Marshal([]string{route.ID, model, protocol})
	return hashKey(string(encoded))
}

type ModelHealth struct {
	ModelID             string     `json:"modelId"`
	Protocol            string     `json:"protocol"`
	Isolated            bool       `json:"isolated"`
	ConsecutiveFailures int64      `json:"consecutiveFailures"`
	NextProbeAt         *time.Time `json:"nextProbeAt,omitempty"`
	ProbeAttempts       int64      `json:"probeAttempts"`
	ProbeDisabled       bool       `json:"probeDisabled,omitempty"`
}

// The model circuit stays isolated until a validated probe or manual test
// succeeds. A timer never sends a known-broken model back into user traffic.
var observeModelResultScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
if redis.call('HGET', KEYS[3], 'deleted') == '1' then return 0 end
if tonumber(redis.call('HGET', KEYS[3], 'epoch') or '0') ~= tonumber(ARGV[3]) then return 0 end
local epoch = tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0')
if epoch ~= tonumber(ARGV[2]) then return 0 end
if redis.call('HGET', KEYS[1], 'isolated') == '1' then return 1 end
if ARGV[4] == '1' then
  redis.call('HSET', KEYS[1], 'failures', 0)
  redis.call('HSET', KEYS[3], 'failures', 0)
  return 0
end
local failures = redis.call('HINCRBY', KEYS[1], 'failures', 1)
redis.call('HSET', KEYS[1], 'route_id', ARGV[7], 'model_id', ARGV[8], 'protocol', ARGV[9])
redis.call('SADD', KEYS[4], ARGV[1])
if ARGV[5] == '1' or failures >= tonumber(ARGV[6]) then
  redis.call('HINCRBY', KEYS[1], 'epoch', 1)
  redis.call('HSET', KEYS[1], 'isolated', 1, 'isolated_at', now, 'probe_attempts', 0, 'probe_spec', ARGV[10])
  redis.call('ZADD', KEYS[2], now + 60000, ARGV[1])
  return 1
end
return 0
`)

func (l *Limiter) ObserveModelResult(ctx context.Context, lease *Lease, success, immediate bool, route Route, spec routeProbeSpec) (bool, error) {
	if lease.modelHealthID == "" || spec.Path == embeddingsPath {
		return false, nil
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return false, err
	}
	model, protocol := modelHealthIdentity(route, spec)
	value, err := observeModelResultScript.Run(ctx, l.redis,
		[]string{modelHealthKey(lease.modelHealthID), modelProbeDueKey(), healthKey(route.ID), routeModelIndexKey(route.ID)},
		lease.modelHealthID, lease.modelEpoch, lease.epoch, boolInt(success), boolInt(immediate), routeFailureThreshold,
		route.ID, model, protocol, string(encoded)).Int64()
	return value == 1, err
}

var claimModelProbeScript = redis.NewScript(routeHealthLua + embeddingModelHealthLua + `
if discardEmbeddingModel(KEYS[1], KEYS[2], nil, ARGV[1]) then return {} end
local now = clockMS()
if redis.call('HGET', KEYS[1], 'isolated') ~= '1' or redis.call('HGET', KEYS[1], 'probe_disabled') == '1' then
  redis.call('ZREM', KEYS[2], ARGV[1])
  return {}
end
local due = tonumber(redis.call('ZSCORE', KEYS[2], ARGV[1]) or '0')
if due == 0 or due > now then return {} end
redis.call('HSET', KEYS[1], 'probe_token', ARGV[2])
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[3]), ARGV[1])
return {tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0'),
        redis.call('HGET', KEYS[1], 'probe_spec') or '{}', redis.call('HGET', KEYS[1], 'route_id') or '',
        tonumber(redis.call('HGET', KEYS[1], 'probe_attempts') or '0')}
`)

var finishModelProbeScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
if redis.call('HGET', KEYS[1], 'isolated') ~= '1' or redis.call('HGET', KEYS[1], 'probe_disabled') == '1'
   or tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') ~= tonumber(ARGV[2])
   or redis.call('HGET', KEYS[1], 'probe_token') ~= ARGV[3] then return 0 end
redis.call('HDEL', KEYS[1], 'probe_token')
if ARGV[4] == '1' then
  redis.call('HINCRBY', KEYS[1], 'epoch', 1)
  redis.call('HSET', KEYS[1], 'failures', 0, 'probe_attempts', 0)
  redis.call('HDEL', KEYS[1], 'isolated', 'isolated_at', 'probe_spec')
  redis.call('ZREM', KEYS[2], ARGV[1])
  return 1
end
-- Capacity/policy skips are not a failed inference and do not advance backoff.
if ARGV[5] == '0' then
  redis.call('ZADD', KEYS[2], now + 60000, ARGV[1])
  return 0
end
local attempts = redis.call('HINCRBY', KEYS[1], 'probe_attempts', 1)
local interval = 600000
local offset = (attempts - 2) * 600000
if attempts == 1 then interval = 120000; offset = 180000
elseif attempts == 2 then interval = 120000; offset = 300000
elseif attempts == 3 then interval = 300000; offset = 600000 end
-- Anchor the first probes at minutes 1, 3, 5 and 10 of this isolation,
-- followed by minutes 20, 30, ... . Probe duration must not shift the plan.
local isolatedAt = tonumber(redis.call('HGET', KEYS[1], 'isolated_at') or '0')
local nextDue = isolatedAt + offset
-- Legacy states have no anchor. Delayed work must not trigger catch-up bursts.
if isolatedAt == 0 or nextDue <= now then nextDue = now + interval end
redis.call('ZADD', KEYS[2], nextDue, ARGV[1])
return 0
`)

type modelProbeClaim struct {
	id string
	routeProbeClaim
}

func (l *Limiter) claimModelProbe(ctx context.Context, id string) (*modelProbeClaim, error) {
	token, err := randomID("model_probe_")
	if err != nil {
		return nil, err
	}
	values, err := claimModelProbeScript.Run(ctx, l.redis, []string{modelHealthKey(id), modelProbeDueKey()}, id, token, routeProbeClaimTTL.Milliseconds()).Slice()
	if err != nil || len(values) == 0 {
		return nil, err
	}
	if len(values) != 4 {
		return nil, errors.New("invalid model probe claim")
	}
	epoch, epochOK := values[0].(int64)
	encoded, specOK := values[1].(string)
	routeID, routeOK := values[2].(string)
	if !epochOK || !specOK || !routeOK || routeID == "" {
		return nil, errors.New("invalid model probe claim")
	}
	claim := &modelProbeClaim{id: id, routeProbeClaim: routeProbeClaim{routeID: routeID, token: token, epoch: epoch}}
	claim.attempts, _ = values[3].(int64)
	if err := json.Unmarshal([]byte(encoded), &claim.spec); err != nil {
		return nil, err
	}
	return claim, nil
}

func (l *Limiter) finishModelProbe(ctx context.Context, claim *modelProbeClaim, success, attempted bool) (bool, error) {
	value, err := finishModelProbeScript.Run(ctx, l.redis, []string{modelHealthKey(claim.id), modelProbeDueKey()},
		claim.id, claim.epoch, claim.token, boolInt(success), boolInt(attempted)).Int64()
	return value == 1, err
}

func (l *Limiter) dueModelProbes(ctx context.Context) ([]string, error) {
	now, err := l.redis.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	return l.redis.ZRangeByScore(ctx, modelProbeDueKey(), &redis.ZRangeBy{
		Min: "-inf", Max: fmt.Sprint(now.UnixMilli()), Count: routeProbeWorkers * 4,
	}).Result()
}

func (s *Service) probeIsolatedModel(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	claim, err := s.limiter.claimModelProbe(ctx, id)
	if err != nil || claim == nil {
		return
	}
	success, attempted := false, false
	defer func() {
		stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
		defer stateCancel()
		_, _ = s.limiter.finishModelProbe(stateCtx, claim, success, attempted)
	}()
	route, err := s.loadProbeRoute(ctx, &claim.routeProbeClaim)
	if err != nil || route == nil || !route.Enabled {
		if err == nil && route == nil {
			s.cleanMissingProbeRoute(ctx, claim.routeID)
		}
		return
	}
	// A changed mapping must not restore the previously failed target model.
	if _, removed := route.RemovedModels[claim.id]; removed {
		_, _ = s.limiter.removeRouteModel(ctx, route.ID, claim.id)
		return
	}
	if modelHealthID(*route, claim.spec) != claim.id {
		return
	}
	timeout, err := s.recoveryProbeTimeout(ctx, *route, claim.attempts)
	if err != nil {
		return
	}
	probeCtx, probeCancel := context.WithTimeout(parent, timeout)
	defer probeCancel()
	lease, acquired, err := s.limiter.acquireHealthLease(probeCtx, *route, claim.token, route.ConcurrencyLimit, "", claim.id, claim.token)
	if err != nil || !acquired || lease.modelEpoch != claim.epoch {
		if lease != nil {
			lease.Release(context.WithoutCancel(parent))
		}
		return
	}
	defer lease.Release(context.WithoutCancel(parent))
	attempted = true
	success = s.performRouteProbe(probeCtx, *route, claim.spec, timeout)
}

var modelRuntimeScript = redis.NewScript(embeddingModelHealthLua + `
local result = {}
for index=3,#KEYS do
  local key = KEYS[index]
  discardEmbeddingModel(key, KEYS[1], KEYS[2], ARGV[index - 2])
  table.insert(result, redis.call('HGET', key, 'model_id') or '')
  table.insert(result, redis.call('HGET', key, 'protocol') or '')
  table.insert(result, redis.call('HGET', key, 'isolated') or '0')
  table.insert(result, redis.call('HGET', key, 'failures') or '0')
  table.insert(result, redis.call('ZSCORE', KEYS[1], ARGV[index - 2]) or '0')
  table.insert(result, redis.call('HGET', key, 'probe_attempts') or '0')
  table.insert(result, redis.call('HGET', key, 'probe_disabled') or '0')
end
return result
`)

func (l *Limiter) populateModelRuntime(ctx context.Context, routes []Route) error {
	pipeline := l.redis.Pipeline()
	indexes := make([]*redis.StringSliceCmd, len(routes))
	for i, route := range routes {
		indexes[i] = pipeline.SMembers(ctx, routeModelIndexKey(route.ID))
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		return err
	}
	for i, index := range indexes {
		ids := index.Val()
		routes[i].ModelHealth = nil
		for _, removed := range routes[i].RemovedModels {
			if removed.Protocol == embeddingsPath {
				continue
			}
			routes[i].ModelHealth = append(routes[i].ModelHealth, ModelHealth{ModelID: removed.ModelID, Protocol: removed.Protocol, Isolated: true, ProbeDisabled: true})
		}
		if len(ids) == 0 {
			continue
		}
		keys := []string{modelProbeDueKey(), routeModelIndexKey(routes[i].ID)}
		args := make([]any, len(ids))
		for j, id := range ids {
			keys = append(keys, modelHealthKey(id))
			args[j] = id
		}
		values, err := modelRuntimeScript.Run(ctx, l.redis, keys, args...).StringSlice()
		if err != nil {
			return err
		}
		if len(values) != len(ids)*7 {
			return errors.New("invalid model runtime response")
		}
		for j := range ids {
			if _, removed := routes[i].RemovedModels[ids[j]]; removed {
				continue
			}
			offset := j * 7
			if values[offset+2] != "1" {
				continue
			}
			state := ModelHealth{ModelID: values[offset], Protocol: values[offset+1], Isolated: true, ProbeDisabled: values[offset+6] == "1"}
			_, _ = fmt.Sscan(values[offset+3], &state.ConsecutiveFailures)
			_, _ = fmt.Sscan(values[offset+5], &state.ProbeAttempts)
			var due int64
			_, _ = fmt.Sscan(values[offset+4], &due)
			if due > 0 {
				at := time.UnixMilli(due).UTC()
				state.NextProbeAt = &at
			}
			routes[i].ModelHealth = append(routes[i].ModelHealth, state)
		}
	}
	return nil
}
