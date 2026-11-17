package model_proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/redis/go-redis/v9"
)

// Quality is scoped to the actual mapped model and protocol. A broken model
// must not lower the scheduling preference of other models on the same route.
func schedulerQualityKey(id string) string { return "proxy:scheduler-quality:{routes}:" + id }

type schedulerQualityAggregate struct {
	RouteID             string `json:"routeID"`
	ModelID             string `json:"modelID"`
	RouteEpoch          int64  `json:"routeEpoch"`
	ModelEpoch          int64  `json:"modelEpoch"`
	LatencyTotal        int64  `json:"latencyTotal"`
	LatencyCount        int64  `json:"latencyCount"`
	Successes           int64  `json:"successes"`
	Failures            int64  `json:"failures"`
	ConsecutiveFailures int64  `json:"consecutiveFailures"`
}

type schedulerCredentialOutcome struct {
	Outcome int   `json:"outcome"`
	Epoch   int64 `json:"epoch"`
}

var observeSchedulerQualityScript = redis.NewScript(routeHealthLua + `
local samples = cjson.decode(ARGV[1])
local credentials = cjson.decode(ARGV[2])
local ttl = tonumber(ARGV[3])
local now = clockMS()
for index, sample in ipairs(samples) do
  local offset = 1 + (index - 1) * 4
  if redis.call('HGET', KEYS[offset + 1], 'deleted') ~= '1'
     and tonumber(redis.call('HGET', KEYS[offset + 1], 'epoch') or '0') == sample.routeEpoch
     and (sample.modelID == '' or tonumber(redis.call('HGET', KEYS[offset + 3], 'epoch') or '0') == sample.modelEpoch) then
    if sample.latencyCount > 0 then
      local latency = sample.latencyTotal / sample.latencyCount
      local previous = tonumber(redis.call('HGET', KEYS[offset], 'latency_ms') or latency)
      redis.call('HSET', KEYS[offset], 'latency_ms', math.floor((previous * 4 + latency) / 5))
    end
    local count = sample.successes + sample.failures
    if count > 0 then
      local rate = sample.successes * 1000 / count
      local previous = tonumber(redis.call('HGET', KEYS[offset], 'success_rate') or '1000')
      -- Recover promptly after a successful test/request; lower preference
      -- after repeated failures without creating an automatic pause/probe.
      local retention = 0.75 ^ count
      if rate > previous then retention = 0.25 ^ count end
      rate = previous * retention + rate * (1 - retention)
      local failures = sample.consecutiveFailures
      if sample.successes == 0 then
        failures = failures + tonumber(redis.call('HGET', KEYS[offset], 'consecutive_errors') or '0')
      end
      redis.call('HSET', KEYS[offset], 'success_rate', math.floor(rate), 'consecutive_errors', failures)
    end
    redis.call('PEXPIRE', KEYS[offset], ttl)
    if sample.modelID ~= '' then redis.call('SADD', KEYS[offset + 2], sample.modelID) end
  end
end
local offset = #samples * 4
for index, outcome in ipairs(credentials) do
  local key = KEYS[offset + index]
  if redis.call('HGET', key, 'deleted') ~= '1'
     and tonumber(redis.call('HGET', key, 'epoch') or '0') == outcome.epoch then
    if outcome.outcome == 1 then redis.call('HDEL', key, 'credential_failed_at')
    else redis.call('HSET', key, 'credential_failed_at', now) end
  end
end
return #samples
`)

func (l *Limiter) observeSchedulerQuality(ctx context.Context, samples map[string]schedulerQualityAggregate, credentials map[string]schedulerCredentialOutcome) error {
	if len(samples) == 0 && len(credentials) == 0 {
		return nil
	}
	keys := make([]string, 0, len(samples)*4+len(credentials))
	ordered := make([]schedulerQualityAggregate, 0, len(samples))
	for id, sample := range samples {
		keys = append(keys, schedulerQualityKey(id), healthKey(sample.RouteID), routeModelIndexKey(sample.RouteID), modelHealthKey(sample.ModelID))
		ordered = append(ordered, sample)
	}
	outcomes := make([]schedulerCredentialOutcome, 0, len(credentials))
	for routeID, outcome := range credentials {
		keys = append(keys, healthKey(routeID))
		outcomes = append(outcomes, outcome)
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return err
	}
	encodedCredentials, err := json.Marshal(outcomes)
	if err != nil {
		return err
	}
	return observeSchedulerQualityScript.Run(ctx, l.redis, keys, string(encoded), string(encodedCredentials), routeLatencyTTL.Milliseconds()).Err()
}

func (s *Service) observeSchedulingOutcome(ctx context.Context, route Route, lease *Lease, spec routeProbeSpec, response *http.Response, success bool) {
	if s.latencySamples == nil || ctx.Err() != nil || spec.Path == embeddingsPath {
		return
	}
	outcome := -1
	credential := 0
	if success {
		outcome = 1
		if spec.Path != compactPath {
			credential = 1
		}
	} else if spec.Path != compactPath && response != nil && (response.StatusCode == http.StatusUnauthorized || responseHealth(response).credentialError) {
		credential = -1
	}
	select {
	case s.latencySamples <- routeLatencyObservation{routeID: route.ID, modelHealthID: modelHealthID(route, spec), routeEpoch: lease.epoch, modelEpoch: lease.modelEpoch, outcome: outcome, credential: credential}:
	default:
	}
}

// Failed attempts affect latency, while success rate counts a logical request
// once, after its retry and complete response. The worker batches both writes.
func addSchedulerQualitySample(pending map[string]schedulerQualityAggregate, credentials map[string]schedulerCredentialOutcome, sample routeLatencyObservation) {
	id := sample.modelHealthID
	if id == "" {
		id = sample.routeID
	}
	aggregate := pending[id]
	if aggregate.RouteEpoch > sample.routeEpoch || aggregate.RouteEpoch == sample.routeEpoch && aggregate.ModelEpoch > sample.modelEpoch {
		return
	}
	if aggregate.RouteEpoch != sample.routeEpoch || aggregate.ModelEpoch != sample.modelEpoch {
		aggregate = schedulerQualityAggregate{}
	}
	aggregate.RouteID, aggregate.ModelID = sample.routeID, sample.modelHealthID
	aggregate.RouteEpoch, aggregate.ModelEpoch = sample.routeEpoch, sample.modelEpoch
	if sample.latency > 0 {
		aggregate.LatencyTotal += boundedLatencyMS(sample.latency)
		aggregate.LatencyCount++
	}
	if sample.outcome > 0 {
		aggregate.Successes++
		aggregate.ConsecutiveFailures = 0
	} else if sample.outcome < 0 {
		aggregate.Failures++
		aggregate.ConsecutiveFailures++
	}
	pending[id] = aggregate
	if sample.credential != 0 && credentials[sample.routeID].Epoch <= sample.routeEpoch {
		credentials[sample.routeID] = schedulerCredentialOutcome{Outcome: sample.credential, Epoch: sample.routeEpoch}
	}
}
