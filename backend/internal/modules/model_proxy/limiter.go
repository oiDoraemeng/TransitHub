package model_proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

var acquireLeaseScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
local count = redis.call('ZCARD', KEYS[1])
if count >= tonumber(ARGV[2]) then
  return {0, count}
end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return {1, count + 1}
`)

var activeLeaseScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
return redis.call('ZCARD', KEYS[1])
`)

var routeStatsScript = redis.NewScript(`
local result = {redis.call('INCR', KEYS[1])}
for index = 2, #KEYS, 2 do
  redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', ARGV[1])
  table.insert(result, redis.call('ZCARD', KEYS[index]))
  local latency = redis.call('GET', KEYS[index + 1])
  if latency then
    table.insert(result, tonumber(latency))
  else
    table.insert(result, -1)
  end
end
return result
`)

var observeLatencyScript = redis.NewScript(`
local sample = tonumber(ARGV[1])
local previous = redis.call('GET', KEYS[1])
local value = sample
if previous then
  value = math.floor((tonumber(previous) * 4 + sample + 2) / 5)
end
redis.call('SET', KEYS[1], value, 'PX', ARGV[2])
return value
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
	limiter *Limiter
	routeID string
	id      string
	stop    chan struct{}
}

type availableRoute struct {
	route        Route
	active       int64
	available    int64
	latencyMS    int64
	latencyKnown bool
}

type acquireCandidate struct {
	availableRoute
	limit int
}

func sortAvailableRoutes(routes []availableRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].route.Priority == routes[j].route.Priority {
			if routes[i].available == routes[j].available {
				return routes[i].route.ID < routes[j].route.ID
			}
			return routes[i].available > routes[j].available
		}
		return routes[i].route.Priority > routes[j].route.Priority
	})
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
	result, err := activeLeaseScript.Run(ctx, l.redis, []string{leaseKey(routeID)}, time.Now().UnixMilli()).Int64()
	return result, err
}

func (l *Limiter) Acquire(ctx context.Context, route Route, leaseID string) (*Lease, bool, error) {
	return l.acquireWithLimit(ctx, route, leaseID, route.ConcurrencyLimit)
}

func (l *Limiter) acquireWithLimit(ctx context.Context, route Route, leaseID string, limit int) (*Lease, bool, error) {
	if limit <= 0 || limit > route.ConcurrencyLimit {
		limit = route.ConcurrencyLimit
	}
	now := time.Now()
	result, err := acquireLeaseScript.Run(ctx, l.redis, []string{leaseKey(route.ID)},
		now.UnixMilli(), limit, now.Add(leaseTTL).UnixMilli(), leaseID, leaseTTL.Milliseconds()).Slice()
	if err != nil {
		return nil, false, err
	}
	acquired, ok := result[0].(int64)
	if !ok || acquired != 1 {
		return nil, false, nil
	}
	lease := &Lease{limiter: l, routeID: route.ID, id: leaseID, stop: make(chan struct{})}
	go lease.renew()
	return lease, true, nil
}

func (l *Limiter) AcquireBest(ctx context.Context, routes []Route, leaseID string) (*Lease, *Route, error) {
	available, cursor, err := l.routeStats(ctx, routes)
	if err != nil {
		return nil, nil, err
	}
	for _, candidate := range rankAcquireCandidates(available, cursor) {
		if candidate.available <= 0 {
			continue
		}
		lease, ok, err := l.acquireWithLimit(ctx, candidate.route, leaseID, candidate.limit)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			route := candidate.route
			return lease, &route, nil
		}
	}
	return nil, nil, nil
}

func (l *Limiter) routeStats(ctx context.Context, routes []Route) ([]availableRoute, int64, error) {
	if len(routes) == 0 {
		return nil, 0, nil
	}
	keys := make([]string, 0, len(routes)*2+1)
	keys = append(keys, roundRobinKey())
	for _, route := range routes {
		keys = append(keys, leaseKey(route.ID), latencyKey(route.ID))
	}
	values, err := routeStatsScript.Run(ctx, l.redis, keys, time.Now().UnixMilli()).Slice()
	if err != nil {
		return nil, 0, err
	}
	if len(values) != len(routes)*2+1 {
		return nil, 0, fmt.Errorf("unexpected route stats length: got %d want %d", len(values), len(routes)*2+1)
	}
	cursor, cursorOK := values[0].(int64)
	if !cursorOK {
		return nil, 0, errors.New("invalid route stats cursor")
	}
	result := make([]availableRoute, 0, len(routes))
	for index, route := range routes {
		active, activeOK := values[index*2+1].(int64)
		latency, latencyOK := values[index*2+2].(int64)
		if !activeOK || !latencyOK {
			return nil, 0, errors.New("invalid route stats response")
		}
		result = append(result, availableRoute{
			route: route, active: active, available: int64(route.ConcurrencyLimit) - active,
			latencyMS: latency, latencyKnown: latency >= 0,
		})
	}
	return result, cursor, nil
}

func (l *Limiter) ObserveResponseHeaderLatency(ctx context.Context, routeID string, latency time.Duration) error {
	if latency < time.Millisecond {
		latency = time.Millisecond
	}
	if latency > maxObservedLatency {
		latency = maxObservedLatency
	}
	return observeLatencyScript.Run(ctx, l.redis, []string{latencyKey(routeID)}, latency.Milliseconds(), routeLatencyTTL.Milliseconds()).Err()
}

func rankAcquireCandidates(routes []availableRoute, cursor int64) []acquireCandidate {
	priorities := make([]int, 0, len(routes))
	seen := make(map[int]struct{}, len(routes))
	for _, route := range routes {
		if route.available <= 0 {
			continue
		}
		if _, ok := seen[route.route.Priority]; !ok {
			seen[route.route.Priority] = struct{}{}
			priorities = append(priorities, route.route.Priority)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	result := make([]acquireCandidate, 0, len(routes)*2)
	for _, priority := range priorities {
		priorityRoutes := make([]availableRoute, 0, len(routes))
		for _, route := range routes {
			if route.route.Priority == priority && route.available > 0 {
				priorityRoutes = append(priorityRoutes, route)
			}
		}
		result = appendSoftTier(result, priorityRoutes, false, temporaryKeySoftConcurrency)
		result = appendSoftTier(result, priorityRoutes, true, providedKeySoftConcurrency)
		sortAvailableRoutes(priorityRoutes)
		rotateLeadingCapacityTie(priorityRoutes, cursor)
		for _, route := range priorityRoutes {
			result = append(result, acquireCandidate{availableRoute: route, limit: route.route.ConcurrencyLimit})
		}
	}
	return result
}

func rotateLeadingCapacityTie(routes []availableRoute, cursor int64) {
	if len(routes) < 2 || routes[0].available <= 0 {
		return
	}
	tied := 1
	for tied < len(routes) && routes[tied].available == routes[0].available {
		tied++
	}
	if tied > 1 {
		selected := int((cursor - 1) % int64(tied))
		if selected < 0 {
			selected = 0
		}
		routes[0], routes[selected] = routes[selected], routes[0]
	}
}

func appendSoftTier(result []acquireCandidate, routes []availableRoute, providedKey bool, softLimit int) []acquireCandidate {
	tier := make([]availableRoute, 0, len(routes))
	for _, route := range routes {
		limit := softLimit
		if route.route.ConcurrencyLimit < limit {
			limit = route.route.ConcurrencyLimit
		}
		if route.route.UseProvidedKey == providedKey && route.active < int64(limit) {
			tier = append(tier, route)
		}
	}
	sort.SliceStable(tier, func(i, j int) bool {
		if tier[i].latencyKnown != tier[j].latencyKnown {
			return !tier[i].latencyKnown
		}
		if tier[i].latencyKnown && tier[i].latencyMS != tier[j].latencyMS {
			return tier[i].latencyMS < tier[j].latencyMS
		}
		if tier[i].active != tier[j].active {
			return tier[i].active < tier[j].active
		}
		return tier[i].route.ID < tier[j].route.ID
	})
	for _, route := range tier {
		limit := softLimit
		if route.route.ConcurrencyLimit < limit {
			limit = route.route.ConcurrencyLimit
		}
		result = append(result, acquireCandidate{availableRoute: route, limit: limit})
	}
	return result
}

func (l *Lease) renew() {
	ticker := time.NewTicker(leaseRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			expires := time.Now().Add(leaseTTL).UnixMilli()
			pipe := l.limiter.redis.TxPipeline()
			pipe.ZAddXX(context.Background(), leaseKey(l.routeID), redis.Z{Score: float64(expires), Member: l.id})
			pipe.PExpire(context.Background(), leaseKey(l.routeID), leaseTTL)
			_, _ = pipe.Exec(context.Background())
		}
	}
}

func (l *Lease) Release(ctx context.Context) {
	if l == nil || l.limiter == nil {
		return
	}
	select {
	case <-l.stop:
		return
	default:
		close(l.stop)
	}
	_ = l.limiter.redis.ZRem(ctx, leaseKey(l.routeID), l.id).Err()
}

func newLeaseID(requestID, routeID string) string { return fmt.Sprintf("%s:%s", requestID, routeID) }
