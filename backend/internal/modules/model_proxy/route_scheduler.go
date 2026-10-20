package model_proxy

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
)

// Selection, health filtering and capacity reservation are one operation. A
// snapshot followed by per-route acquisitions can skip soft tiers or priority
// boundaries when another request releases capacity between the two calls.
var acquireBestRouteScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
local routes = cjson.decode(ARGV[1])
local best = nil
local tied = {}
local eligible = {}
local paused = 0
local modelPaused = 0
local compact = ARGV[7] == '1'
for index, route in ipairs(routes) do
  local offset = 3 + (index - 1) * 5
  local untilMS = resumeExpired(KEYS[offset + 2], KEYS[2], route.id, now)
  local isolated = route.modelRemoved or (route.modelHealthID ~= '' and redis.call('HGET', KEYS[offset + 3], 'isolated') == '1'
    and (not route.neverAutoPause or redis.call('HGET', KEYS[offset + 3], 'probe_disabled') == '1'))
  if (untilMS > now and not route.neverAutoPause) or route.manualPause or redis.call('HGET', KEYS[offset + 2], 'deleted') == '1' then
    paused = paused + 1
  elseif isolated then
    paused = paused + 1
    modelPaused = modelPaused + 1
  else
    redis.call('ZREMRANGEBYSCORE', KEYS[offset], '-inf', now)
    local active = redis.call('ZCARD', KEYS[offset])
    local remaining = route.limit - active
    if remaining > 0 then
      local tier = 2
      if not route.provided and active < math.min(route.limit, tonumber(ARGV[4])) then tier = 0 end
      if route.provided and active < math.min(route.limit, tonumber(ARGV[5])) then tier = 1 end
      local latency = tonumber(redis.call('GET', KEYS[offset + 1]) or '-1')
      local lastSuccess = 0
      if compact then lastSuccess = tonumber(redis.call('HGET', KEYS[offset + 3], 'last_success_at') or '0') end
      local item = {index=index, offset=offset, priority=route.priority, tier=tier,
                    active=active, remaining=remaining, latency=latency, id=route.id, lastSuccess=lastSuccess}
      table.insert(eligible, item)
      local better = best == nil
      if best ~= nil then
        if item.priority ~= best.priority then better = item.priority > best.priority
        elseif compact and item.lastSuccess ~= best.lastSuccess then better = item.lastSuccess > best.lastSuccess
        elseif item.tier ~= best.tier then better = item.tier < best.tier
        elseif tier == 2 then better = remaining > best.remaining
        elseif latency ~= best.latency then better = latency < best.latency
        elseif active ~= best.active then better = active < best.active
        else better = item.id < best.id end
      end
      if better then best = item; tied = {item}
      elseif best.priority == item.priority and best.lastSuccess == item.lastSuccess and best.tier == 2 and tier == 2
             and best.remaining == remaining then table.insert(tied, item) end
    end
  end
end
if best == nil then return {0, 0, paused, 0, modelPaused} end
if compact and best.lastSuccess > 0 then
  -- Compact never sends recovery probes. Reuse the most recently successful
  -- actual model/endpoint, respecting priority, exclusion and hard capacity.
elseif best.tier == 2 then
  table.sort(tied, function(a,b) return a.id < b.id end)
  local cursor = redis.call('INCR', KEYS[1])
  best = tied[(cursor - 1) % #tied + 1]
else
  -- Smooth weighted round robin shares even sequential requests (whose leases
  -- may all finish before the next request). Latency alone or least-active
  -- selection would keep sending such traffic to a single route.
  local pool = {}
  local fastest = nil
  for _, item in ipairs(eligible) do
    if item.priority == best.priority and item.tier == best.tier then
      table.insert(pool, item)
      if item.latency > 0 and (fastest == nil or item.latency < fastest) then
        fastest = item.latency
      end
    end
  end
  local totalWeight = 0
  for _, item in ipairs(pool) do
    -- Inverse-latency weights are bounded to 1..2: slow but healthy routes
    -- retain traffic, while unmeasured routes receive sampling opportunities.
    item.weight = 2
    if fastest ~= nil and item.latency > 0 then
      item.weight = math.max(1, 2 * fastest / item.latency)
    end
    totalWeight = totalWeight + item.weight
  end
  local totalCredit = 0
  for _, item in ipairs(pool) do
    local credit = tonumber(redis.call('GET', KEYS[item.offset + 4]) or '0')
    -- Changing model/health/capacity filters must not accumulate unbounded
    -- credit or cause a recovering route to monopolize catch-up traffic.
    item.credit = math.max(-totalWeight, math.min(totalWeight, credit))
    totalCredit = totalCredit + item.credit
  end
  local meanCredit = totalCredit / #pool
  local selected = nil
  for _, item in ipairs(pool) do
    item.credit = item.credit - meanCredit + item.weight
    local better = selected == nil
    if selected ~= nil then
      if math.abs(item.credit - selected.credit) > 0.000001 then better = item.credit > selected.credit
      elseif item.active ~= selected.active then better = item.active < selected.active
      elseif item.latency ~= selected.latency then better = item.latency < selected.latency
      else better = item.id < selected.id end
    end
    if better then selected = item end
  end
  best = selected
  for _, item in ipairs(pool) do
    if item.index == best.index then item.credit = item.credit - totalWeight end
    redis.call('SET', KEYS[item.offset + 4], tostring(item.credit), 'PX', ARGV[6])
  end
end
redis.call('ZADD', KEYS[best.offset], now + tonumber(ARGV[3]), ARGV[2])
redis.call('PEXPIRE', KEYS[best.offset], ARGV[3])
return {best.index, tonumber(redis.call('HGET', KEYS[best.offset + 2], 'epoch') or '0'), paused,
        tonumber(redis.call('HGET', KEYS[best.offset + 3], 'epoch') or '0'), modelPaused}
`)

func schedulerBalanceKey(routeID string) string { return "proxy:scheduler-balance:{routes}:" + routeID }

type schedulerRoute struct {
	ID             string `json:"id"`
	Priority       int    `json:"priority"`
	Limit          int    `json:"limit"`
	Provided       bool   `json:"provided"`
	ManualPause    bool   `json:"manualPause"`
	ModelHealthID  string `json:"modelHealthID"`
	ModelRemoved   bool   `json:"modelRemoved"`
	NeverAutoPause bool   `json:"neverAutoPause"`
}

func (l *Limiter) acquireBestAtomic(ctx context.Context, routes []Route, leaseID string, specs ...routeProbeSpec) (*Lease, *Route, error) {
	if len(routes) == 0 {
		return nil, nil, nil
	}
	keys := []string{roundRobinKey(), probeDueKey()}
	input := make([]schedulerRoute, 0, len(routes))
	for _, route := range routes {
		modelID := ""
		if len(specs) > 0 {
			modelID = modelHealthID(route, specs[0])
		}
		keys = append(keys, leaseKey(route.ID), latencyKey(route.ID), healthKey(route.ID), modelHealthKey(modelID), schedulerBalanceKey(route.ID))
		_, removed := route.RemovedModels[modelID]
		input = append(input, schedulerRoute{route.ID, route.Priority, route.ConcurrencyLimit, route.UseProvidedKey, route.SchedulingPaused, modelID, removed, route.NeverAutoPause})
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	compact := len(specs) > 0 && specs[0].Path == compactPath
	values, err := acquireBestRouteScript.Run(ctx, l.redis, keys, string(encoded), leaseID,
		leaseTTL.Milliseconds(), temporaryKeySoftConcurrency, providedKeySoftConcurrency, routeLatencyTTL.Milliseconds(), boolInt(compact)).Int64Slice()
	if err != nil {
		return nil, nil, err
	}
	if len(values) != 5 {
		return nil, nil, errors.New("invalid route acquisition response")
	}
	if values[0] == 0 {
		if values[2] == int64(len(routes)) {
			if values[4] > 0 {
				return nil, nil, errModelPaused
			}
			return nil, nil, errRoutePaused
		}
		return nil, nil, nil
	}
	index := values[0] - 1
	if index < 0 || index >= int64(len(routes)) {
		return nil, nil, errors.New("invalid selected route index")
	}
	route := routes[index]
	lease := l.newLease(route.ID, leaseID, values[1], "", modelLeaseState{input[index].ModelHealthID, values[3], ""})
	lease.neverAutoPause = route.NeverAutoPause
	return lease, &route, nil
}
