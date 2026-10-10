package model_proxy

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestLimiterRedisSchedulingStages(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("scheduler-test-%d-", time.Now().UnixNano())
	routes := []Route{
		{ID: prefix + "temporary-fast", Priority: 2, ConcurrencyLimit: 50},
		{ID: prefix + "temporary-slow", Priority: 2, ConcurrencyLimit: 50},
		{ID: prefix + "provided", Priority: 2, ConcurrencyLimit: 50, UseProvidedKey: true},
	}
	cleanupRedisTestRoutes(t, client, routes)
	if err := limiter.ObserveResponseHeaderLatency(ctx, routes[0].ID, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := limiter.ObserveResponseHeaderLatency(ctx, routes[1].ID, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := limiter.ObserveResponseHeaderLatency(ctx, routes[2].ID, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)
	leases := make([]*Lease, 0, 21)
	for index := 0; index < 21; index++ {
		lease, route, err := limiter.AcquireBest(ctx, routes, fmt.Sprintf("request-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		if lease == nil || route == nil {
			t.Fatalf("request %d did not acquire a route", index)
		}
		leases = append(leases, lease)
		counts[route.ID]++
	}
	for _, lease := range leases {
		lease.Release(ctx)
	}
	temporaryTotal := counts[routes[0].ID] + counts[routes[1].ID]
	if temporaryTotal != 11 || counts[routes[0].ID] < 5 || counts[routes[1].ID] < 5 || counts[routes[2].ID] != 10 {
		t.Fatalf("route counts=%v want both temporary routes at least 5, temporary total 11, provided 10", counts)
	}
}

func TestLimiterRedisSoftCapIsAtomic(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := Route{ID: fmt.Sprintf("soft-cap-test-%d", time.Now().UnixNano()), ConcurrencyLimit: 50}
	cleanupRedisTestRoutes(t, client, []Route{route})

	var acquired atomic.Int64
	var leasesMu sync.Mutex
	var leases []*Lease
	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			lease, ok, err := limiter.acquireWithLimit(ctx, route, fmt.Sprintf("request-%d", index), temporaryKeySoftConcurrency)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if !ok {
				return
			}
			acquired.Add(1)
			leasesMu.Lock()
			leases = append(leases, lease)
			leasesMu.Unlock()
		}(index)
	}
	wait.Wait()
	for _, lease := range leases {
		lease.Release(ctx)
	}
	if acquired.Load() != temporaryKeySoftConcurrency {
		t.Fatalf("acquired=%d want %d", acquired.Load(), temporaryKeySoftConcurrency)
	}
}

func redisTestLimiter(t *testing.T) (*Limiter, *redis.Client) {
	t.Helper()
	rawURL := os.Getenv("MODEL_PROXY_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("MODEL_PROXY_TEST_REDIS_URL is not configured")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return NewLimiter(client), client
}

func cleanupRedisTestRoutes(t *testing.T, client *redis.Client, routes []Route) {
	t.Helper()
	keys := make([]string, 0, len(routes)*2)
	for _, route := range routes {
		keys = append(keys, leaseKey(route.ID), latencyKey(route.ID))
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
}
