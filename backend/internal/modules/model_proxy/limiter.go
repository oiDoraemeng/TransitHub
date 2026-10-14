package model_proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	leaseTTL                    = 45 * time.Second
	leaseRefresh                = 15 * time.Second
	temporaryKeySoftConcurrency = 5
	providedKeySoftConcurrency  = 10
	routeLatencyTTL             = 30 * time.Minute
	maxObservedLatency          = 2 * time.Minute
)

var acquireLeaseScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
local untilMS = resumeExpired(KEYS[2], KEYS[3], ARGV[4], now)
local probe = ARGV[5] ~= '' and redis.call('HGET', KEYS[2], 'probe_token') == ARGV[5]
if untilMS > now and not probe then return {-1, 0} end
if ARGV[5] ~= '' and not probe then return {-1, 0} end
local modelEpoch = 0
if ARGV[6] ~= '' then
  modelEpoch = tonumber(redis.call('HGET', KEYS[4], 'epoch') or '0')
  local modelProbe = ARGV[7] ~= '' and redis.call('HGET', KEYS[4], 'probe_token') == ARGV[7]
  if redis.call('HGET', KEYS[4], 'isolated') == '1' and not modelProbe then return {-2, 0} end
  if ARGV[7] ~= '' and not modelProbe then return {-2, 0} end
end
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local count = redis.call('ZCARD', KEYS[1])
if count >= tonumber(ARGV[1]) then
  return {0, count}
end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return {1, tonumber(redis.call('HGET', KEYS[2], 'epoch') or '0'), modelEpoch}
`)

var activeLeaseScript = redis.NewScript(routeHealthLua + `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', clockMS())
return redis.call('ZCARD', KEYS[1])
`)

var renewLeaseScript = redis.NewScript(routeHealthLua + `
local now = clockMS()
redis.call('ZADD', KEYS[1], 'XX', now + tonumber(ARGV[1]), ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
-- Temporary-key management calls may outlive their context's deadline. Keep
-- the probe claim exclusive until that worker has actually finished.
if ARGV[4] ~= '' and redis.call('HGET', KEYS[2], 'probe_token') == ARGV[4] then
  redis.call('ZADD', KEYS[3], now + tonumber(ARGV[5]), ARGV[3])
end
if ARGV[6] ~= '' and redis.call('GET', KEYS[4]) == ARGV[6] then
  redis.call('PEXPIRE', KEYS[4], ARGV[1])
end
if ARGV[8] ~= '' and redis.call('HGET', KEYS[5], 'probe_token') == ARGV[8] then
  redis.call('ZADD', KEYS[6], now + tonumber(ARGV[5]), ARGV[7])
end
return 1
`)

var observeLatencyScript = redis.NewScript(`
for index, key in ipairs(KEYS) do
  local sample = tonumber(ARGV[index + 1])
  local previous = redis.call('GET', key)
  local value = sample
  if previous then
    value = math.floor((tonumber(previous) * 4 + sample + 2) / 5)
  end
  redis.call('SET', key, value, 'PX', ARGV[1])
end
return #KEYS
`)

var allowRequestScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local cutoff = now - tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', cutoff)
local count = redis.call('ZCARD', KEYS[1])
if count >= tonumber(ARGV[3]) then
  return 0
end
redis.call('ZADD', KEYS[1], now, ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

type Limiter struct {
	redis *redis.Client
}

type Lease struct {
	limiter         *Limiter
	routeID         string
	id              string
	stop            chan struct{}
	epoch           int64
	probeToken      string
	manualTestToken string
	modelHealthID   string
	modelEpoch      int64
	modelProbeToken string
	once            sync.Once
}

func NewLimiter(client *redis.Client) *Limiter { return &Limiter{redis: client} }

func leaseKey(routeID string) string { return "proxy:concurrency:{routes}:" + routeID }

func latencyKey(routeID string) string { return "proxy:latency:{routes}:" + routeID }

func roundRobinKey() string { return "proxy:round-robin:{routes}" }

func requestRateKey(groupID, routeID string) string {
	return "proxy:requests:{" + groupID + ":" + routeID + "}"
}

func (l *Limiter) AllowRequestsPerMinute(ctx context.Context, groupID, routeID string, limit int, requestID string) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	result, err := allowRequestScript.Run(ctx, l.redis, []string{requestRateKey(groupID, routeID)},
		time.Now().UnixMilli(), (time.Minute).Milliseconds(), limit, requestID).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (l *Limiter) Active(ctx context.Context, routeID string) (int64, error) {
	result, err := activeLeaseScript.Run(ctx, l.redis, []string{leaseKey(routeID)}).Int64()
	return result, err
}

func (l *Limiter) Acquire(ctx context.Context, route Route, leaseID string) (*Lease, bool, error) {
	return l.acquireWithLimit(ctx, route, leaseID, route.ConcurrencyLimit)
}

func (l *Limiter) AcquireForModel(ctx context.Context, route Route, leaseID string, spec routeProbeSpec) (*Lease, bool, error) {
	return l.acquireHealthLease(ctx, route, leaseID, route.ConcurrencyLimit, "", modelHealthID(route, spec), "")
}

func (l *Limiter) acquireWithLimit(ctx context.Context, route Route, leaseID string, limit int) (*Lease, bool, error) {
	return l.acquireRouteLease(ctx, route, leaseID, limit, "")
}

func (l *Limiter) acquireRouteLease(ctx context.Context, route Route, leaseID string, limit int, probeToken string) (*Lease, bool, error) {
	return l.acquireHealthLease(ctx, route, leaseID, limit, probeToken, "", "")
}

func (l *Limiter) acquireHealthLease(ctx context.Context, route Route, leaseID string, limit int, probeToken, modelID, modelProbeToken string) (*Lease, bool, error) {
	if limit <= 0 || limit > route.ConcurrencyLimit {
		limit = route.ConcurrencyLimit
	}
	result, err := acquireLeaseScript.Run(ctx, l.redis, []string{leaseKey(route.ID), healthKey(route.ID), probeDueKey(), modelHealthKey(modelID)},
		limit, leaseID, leaseTTL.Milliseconds(), route.ID, probeToken, modelID, modelProbeToken).Int64Slice()
	if err != nil {
		return nil, false, err
	}
	if len(result) < 2 {
		return nil, false, errors.New("invalid lease response")
	}
	if result[0] == -1 {
		return nil, false, errRoutePaused
	}
	if result[0] == -2 {
		return nil, false, errModelPaused
	}
	if result[0] != 1 {
		return nil, false, nil
	}
	if len(result) != 3 {
		return nil, false, errors.New("invalid health lease response")
	}
	return l.newLease(route.ID, leaseID, result[1], probeToken, modelLeaseState{modelID, result[2], modelProbeToken}), true, nil
}

type modelLeaseState struct {
	id         string
	epoch      int64
	probeToken string
}

func (l *Limiter) newLease(routeID, leaseID string, epoch int64, probeToken string, model ...modelLeaseState) *Lease {
	lease := &Lease{limiter: l, routeID: routeID, id: leaseID, epoch: epoch, probeToken: probeToken, stop: make(chan struct{})}
	if len(model) > 0 {
		lease.modelHealthID, lease.modelEpoch, lease.modelProbeToken = model[0].id, model[0].epoch, model[0].probeToken
	}
	go lease.renew()
	return lease
}

func (l *Limiter) AcquireBest(ctx context.Context, routes []Route, leaseID string, spec ...routeProbeSpec) (*Lease, *Route, error) {
	return l.acquireBestAtomic(ctx, routes, leaseID, spec...)
}

func (l *Limiter) ObserveResponseHeaderLatency(ctx context.Context, routeID string, latency time.Duration) error {
	return observeLatencyScript.Run(ctx, l.redis, []string{latencyKey(routeID)}, routeLatencyTTL.Milliseconds(), boundedLatencyMS(latency)).Err()
}

func boundedLatencyMS(latency time.Duration) int64 {
	if latency < time.Millisecond {
		latency = time.Millisecond
	}
	if latency > maxObservedLatency {
		latency = maxObservedLatency
	}
	return latency.Milliseconds()
}

func (l *Limiter) observeResponseHeaderLatencies(ctx context.Context, samples map[string]routeLatencyAggregate) error {
	keys := make([]string, 0, len(samples))
	args := []any{routeLatencyTTL.Milliseconds()}
	for routeID, aggregate := range samples {
		if aggregate.count <= 0 {
			continue
		}
		keys = append(keys, latencyKey(routeID))
		args = append(args, boundedLatencyMS(aggregate.total/time.Duration(aggregate.count)))
	}
	if len(keys) == 0 {
		return nil
	}
	return observeLatencyScript.Run(ctx, l.redis, keys, args...).Err()
}

func (l *Lease) renew() {
	ticker := time.NewTicker(leaseRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = renewLeaseScript.Run(renewCtx, l.limiter.redis, []string{leaseKey(l.routeID), healthKey(l.routeID), probeDueKey(), manualTestKey(l.routeID), modelHealthKey(l.modelHealthID), modelProbeDueKey()},
				leaseTTL.Milliseconds(), l.id, l.routeID, l.probeToken, routeProbeClaimTTL.Milliseconds(), l.manualTestToken, l.modelHealthID, l.modelProbeToken).Err()
			cancel()
		}
	}
}

func (l *Lease) Release(ctx context.Context) {
	if l == nil || l.limiter == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		releaseCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if l.manualTestToken != "" {
			_ = releaseManualTestScript.Run(releaseCtx, l.limiter.redis, []string{leaseKey(l.routeID), manualTestKey(l.routeID)}, l.id, l.manualTestToken).Err()
		} else {
			_ = l.limiter.redis.ZRem(releaseCtx, leaseKey(l.routeID), l.id).Err()
		}
	})
}

func newLeaseID(requestID, routeID string) string { return fmt.Sprintf("%s:%s", requestID, routeID) }
