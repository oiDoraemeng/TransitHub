package model_proxy

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	leaseTTL     = 45 * time.Second
	leaseRefresh = 15 * time.Second
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

type Limiter struct {
	redis *redis.Client
}

type Lease struct {
	limiter *Limiter
	routeID string
	id      string
	stop    chan struct{}
}

func NewLimiter(client *redis.Client) *Limiter { return &Limiter{redis: client} }

func leaseKey(routeID string) string { return "proxy:concurrency:{routes}:" + routeID }

func (l *Limiter) Active(ctx context.Context, routeID string) (int64, error) {
	result, err := activeLeaseScript.Run(ctx, l.redis, []string{leaseKey(routeID)}, time.Now().UnixMilli()).Int64()
	return result, err
}

func (l *Limiter) Acquire(ctx context.Context, route Route, leaseID string) (*Lease, bool, error) {
	now := time.Now()
	result, err := acquireLeaseScript.Run(ctx, l.redis, []string{leaseKey(route.ID)},
		now.UnixMilli(), route.ConcurrencyLimit, now.Add(leaseTTL).UnixMilli(), leaseID, leaseTTL.Milliseconds()).Slice()
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
	type availableRoute struct {
		route     Route
		available int64
	}
	available := make([]availableRoute, 0, len(routes))
	for _, route := range routes {
		active, err := l.Active(ctx, route.ID)
		if err != nil {
			return nil, nil, err
		}
		available = append(available, availableRoute{route: route, available: int64(route.ConcurrencyLimit) - active})
	}
	sort.SliceStable(available, func(i, j int) bool {
		if available[i].available == available[j].available {
			return available[i].route.ID < available[j].route.ID
		}
		return available[i].available > available[j].available
	})
	if len(available) > 1 && available[0].available > 0 {
		tied := 1
		for tied < len(available) && available[tied].available == available[0].available {
			tied++
		}
		if tied > 1 {
			cursor, err := l.redis.Incr(ctx, "proxy:round-robin:{routes}").Result()
			if err != nil {
				return nil, nil, err
			}
			selected := int((cursor - 1) % int64(tied))
			available[0], available[selected] = available[selected], available[0]
		}
	}
	for _, candidate := range available {
		if candidate.available <= 0 {
			continue
		}
		lease, ok, err := l.Acquire(ctx, candidate.route, leaseID)
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
