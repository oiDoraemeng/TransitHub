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

func TestLimiterRedisBalancesMixedKeyTypes(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("scheduler-test-%d-", time.Now().UnixNano())
	routes := []Route{
		{ID: prefix + "temporary-a", Priority: 2, ConcurrencyLimit: 50},
		{ID: prefix + "temporary-b", Priority: 2, ConcurrencyLimit: 50},
		{ID: prefix + "provided", Priority: 2, ConcurrencyLimit: 50, UseProvidedKey: true},
	}
	cleanupRedisTestRoutes(t, client, routes)
	counts := make(map[string]int)
	for index := 0; index < 21; index++ {
		lease, route, err := limiter.AcquireBest(ctx, routes, fmt.Sprintf("request-%d", index))
		if err != nil || lease == nil || route == nil {
			t.Fatalf("acquire %d: %v", index, err)
		}
		t.Cleanup(func() { lease.Release(ctx) })
		counts[route.ID]++
	}
	for _, route := range routes {
		if counts[route.ID] != 7 {
			t.Fatalf("key type affected balancing: %v", counts)
		}
	}
}

func TestLimiterRedisRecordsTodayRequests(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	routeID := fmt.Sprintf("request-count-test-%d", time.Now().UnixNano())
	key, _ := routeRequestCountKey(time.Now())
	t.Cleanup(func() { _ = client.HDel(ctx, key, routeID).Err() })

	for i := 0; i < 3; i++ {
		if err := limiter.RecordRouteRequest(ctx, routeID); err != nil {
			t.Fatal(err)
		}
	}
	count, err := client.HGet(ctx, key, routeID).Int64()
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("route request count=%d want 3", count)
	}
	runtime := []Route{{ID: routeID, ConcurrencyLimit: 50}}
	if err := limiter.PopulateRouteRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if runtime[0].TodayRequests != 3 {
		t.Fatalf("runtime route request count=%d want 3", runtime[0].TodayRequests)
	}
	if ttl, err := client.TTL(ctx, key).Result(); err != nil || ttl <= 0 {
		t.Fatalf("route request count ttl=%s err=%v", ttl, err)
	}
}

func TestLimiterRedisHardCapIsAtomic(t *testing.T) {
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
			lease, ok, err := limiter.AcquireBest(ctx, []Route{route}, fmt.Sprintf("request-%d", index))
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if ok == nil {
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
	if acquired.Load() != int64(route.ConcurrencyLimit) {
		t.Fatalf("acquired=%d want %d", acquired.Load(), route.ConcurrencyLimit)
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
		keys = append(keys, leaseKey(route.ID), latencyKey(route.ID), healthKey(route.ID), schedulerBalanceKey(route.ID), schedulerQualityKey(route.ID))
	}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys...).Err()
		for _, route := range routes {
			_ = client.ZRem(context.Background(), probeDueKey(), route.ID).Err()
			ids, _ := client.SMembers(context.Background(), routeModelIndexKey(route.ID)).Result()
			for _, id := range ids {
				_ = client.Del(context.Background(), modelHealthKey(id)).Err()
				_ = client.Del(context.Background(), schedulerQualityKey(id)).Err()
				_ = client.ZRem(context.Background(), modelProbeDueKey(), id).Err()
			}
			_ = client.Del(context.Background(), routeModelIndexKey(route.ID)).Err()
		}
	})
}
