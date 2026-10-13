package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	routeFailureThreshold = 3
	routePauseDuration    = 30 * time.Minute
	routeProbeInterval    = 3 * time.Minute
	routeProbeTimeout     = 45 * time.Second
	routeProbeClaimTTL    = routeProbeTimeout + 5*time.Second
	routeProbeWorkers     = 8
	routeProbeTick        = 5 * time.Second
)

var errRoutePaused = errors.New("proxy route is temporarily paused")

var routeRetryAllowedScript = redis.NewScript(routeHealthLua + `
local untilMS = resumeExpired(KEYS[1], KEYS[2], ARGV[1], clockMS())
if untilMS > 0 or tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') ~= tonumber(ARGV[2]) then return 0 end
return 1
`)

func (l *Limiter) routeRetryAllowed(ctx context.Context, lease *Lease) (bool, error) {
	result, err := routeRetryAllowedScript.Run(ctx, l.redis, []string{healthKey(lease.routeID), probeDueKey()}, lease.routeID, lease.epoch).Int64()
	return result == 1, err
}

type routeProbeSpec struct {
	GroupID string `json:"groupId,omitempty"`
	ModelID string `json:"modelId,omitempty"`
	Path    string `json:"path"`
}

func healthKey(routeID string) string { return "proxy:health:{routes}:" + routeID }
func probeDueKey() string             { return "proxy:probe-due:{routes}" }

// Epochs fence results from requests started before a pause or recovery. A late
// success must never reopen a circuit, and a late failure must never close it.
// All scheduler keys share a hash tag so the scripts also work in Redis Cluster.
const routeHealthLua = `
local function clockMS()
  local t = redis.call('TIME')
  return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local function resumeExpired(key, dueKey, routeID, now)
  local untilMS = tonumber(redis.call('HGET', key, 'paused_until') or '0')
  if untilMS > 0 and untilMS <= now then
    redis.call('HINCRBY', key, 'epoch', 1)
    redis.call('HSET', key, 'failures', 0)
    redis.call('HDEL', key, 'paused_until', 'probe_token', 'probe_spec')
    redis.call('ZREM', dueKey, routeID)
    untilMS = 0
  end
  return untilMS
end
`

var observeRouteResultScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
local untilMS = resumeExpired(KEYS[1], KEYS[2], ARGV[1], now)
local epoch = tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0')
if epoch ~= tonumber(ARGV[2]) then return untilMS end
if untilMS > now then return untilMS end
if ARGV[3] == '1' then
  redis.call('HSET', KEYS[1], 'failures', 0)
  return 0
end
local failures = redis.call('HINCRBY', KEYS[1], 'failures', 1)
if failures >= tonumber(ARGV[4]) then
  untilMS = now + tonumber(ARGV[5])
  redis.call('HINCRBY', KEYS[1], 'epoch', 1)
  redis.call('HSET', KEYS[1], 'paused_until', untilMS, 'probe_spec', ARGV[7])
  redis.call('ZADD', KEYS[2], now + tonumber(ARGV[6]), ARGV[1])
end
return untilMS
`)

var claimRouteProbeScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
local untilMS = resumeExpired(KEYS[1], KEYS[2], ARGV[1], now)
if untilMS == 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
  return {}
end
local due = tonumber(redis.call('ZSCORE', KEYS[2], ARGV[1]) or '0')
if due == 0 or due > now then return {} end
redis.call('HSET', KEYS[1], 'probe_token', ARGV[2])
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[3]), ARGV[1])
return {tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0'),
        redis.call('HGET', KEYS[1], 'probe_spec') or '{}'}
`)

var finishRouteProbeScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
if tonumber(redis.call('HGET', KEYS[1], 'epoch') or '0') ~= tonumber(ARGV[2])
   or redis.call('HGET', KEYS[1], 'probe_token') ~= ARGV[3] then return 0 end
redis.call('HDEL', KEYS[1], 'probe_token')
local untilMS = resumeExpired(KEYS[1], KEYS[2], ARGV[1], now)
if untilMS == 0 then return 0 end
if ARGV[4] == '1' then
  redis.call('HINCRBY', KEYS[1], 'epoch', 1)
  redis.call('HSET', KEYS[1], 'failures', 0)
  redis.call('HDEL', KEYS[1], 'paused_until', 'probe_spec')
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('DEL', KEYS[3])
  return 1
end
-- Failed probes do not extend the original thirty-minute pause.
redis.call('ZADD', KEYS[2], math.min(untilMS, now + tonumber(ARGV[5])), ARGV[1])
return 0
`)

type routeProbeClaim struct {
	routeID string
	token   string
	epoch   int64
	spec    routeProbeSpec
}

var routeRuntimeScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
local result = {}
for index, routeID in ipairs(ARGV) do
  local offset = index * 2
  local untilMS = resumeExpired(KEYS[offset + 1], KEYS[1], routeID, now)
  redis.call('ZREMRANGEBYSCORE', KEYS[offset], '-inf', now)
  table.insert(result, redis.call('ZCARD', KEYS[offset]))
  table.insert(result, untilMS)
  table.insert(result, tonumber(redis.call('ZSCORE', KEYS[1], routeID) or '0'))
  table.insert(result, tonumber(redis.call('HGET', KEYS[offset + 1], 'failures') or '0'))
end
return result
`)

func (l *Limiter) PopulateRouteRuntime(ctx context.Context, routes []Route) error {
	if len(routes) == 0 {
		return nil
	}
	keys := []string{probeDueKey()}
	args := make([]any, 0, len(routes))
	for _, route := range routes {
		keys = append(keys, leaseKey(route.ID), healthKey(route.ID))
		args = append(args, route.ID)
	}
	values, err := routeRuntimeScript.Run(ctx, l.redis, keys, args...).Int64Slice()
	if err != nil {
		return err
	}
	if len(values) != len(routes)*4 {
		return errors.New("invalid route runtime response")
	}
	for index := range routes {
		route := &routes[index]
		route.ActiveConcurrency = values[index*4]
		route.ConsecutiveFailures = values[index*4+3]
		route.PausedUntil, route.NextProbeAt = nil, nil
		if values[index*4+1] > 0 {
			until := time.UnixMilli(values[index*4+1]).UTC()
			route.PausedUntil = &until
			if values[index*4+2] > 0 {
				due := time.UnixMilli(values[index*4+2]).UTC()
				route.NextProbeAt = &due
			}
		}
	}
	return nil
}

func (l *Limiter) ObserveRouteResult(ctx context.Context, lease *Lease, success bool, spec routeProbeSpec) (bool, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return false, err
	}
	value, err := observeRouteResultScript.Run(ctx, l.redis, []string{healthKey(lease.routeID), probeDueKey()},
		lease.routeID, lease.epoch, boolInt(success), routeFailureThreshold, routePauseDuration.Milliseconds(),
		routeProbeInterval.Milliseconds(), string(encoded)).Int64()
	return value > 0, err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (l *Limiter) claimRouteProbe(ctx context.Context, routeID string) (*routeProbeClaim, error) {
	token, err := randomID("probe_")
	if err != nil {
		return nil, err
	}
	values, err := claimRouteProbeScript.Run(ctx, l.redis, []string{healthKey(routeID), probeDueKey()},
		routeID, token, routeProbeClaimTTL.Milliseconds()).Slice()
	if err != nil || len(values) == 0 {
		return nil, err
	}
	if len(values) != 2 {
		return nil, errors.New("invalid route probe claim")
	}
	epoch, ok := values[0].(int64)
	encoded, specOK := values[1].(string)
	if !ok || !specOK {
		return nil, errors.New("invalid route probe claim")
	}
	claim := &routeProbeClaim{routeID: routeID, token: token, epoch: epoch}
	if err := json.Unmarshal([]byte(encoded), &claim.spec); err != nil {
		return nil, err
	}
	return claim, nil
}

func (l *Limiter) finishRouteProbe(ctx context.Context, claim *routeProbeClaim, success bool) (bool, error) {
	result, err := finishRouteProbeScript.Run(ctx, l.redis,
		[]string{healthKey(claim.routeID), probeDueKey(), latencyKey(claim.routeID)},
		claim.routeID, claim.epoch, claim.token, boolInt(success), routeProbeInterval.Milliseconds()).Int64()
	return result == 1, err
}

func (l *Limiter) dueRouteProbes(ctx context.Context) ([]string, error) {
	// Use Redis time rather than individual application hosts' clocks.
	now, err := l.redis.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	return l.redis.ZRangeByScore(ctx, probeDueKey(), &redis.ZRangeBy{
		Min: "-inf", Max: fmt.Sprint(now.UnixMilli()), Count: routeProbeWorkers * 4,
	}).Result()
}
