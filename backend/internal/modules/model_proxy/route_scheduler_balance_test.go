package model_proxy

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestSchedulerRedisBalancesSequentialRequestsAndFavorsLatency(t *testing.T) {
	for _, provided := range []bool{false, true} {
		for _, test := range []struct {
			name    string
			latency []time.Duration
			want    []int
		}{
			{"equal", []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}, []int{40, 40, 40}},
			{"weighted", []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 2 * time.Second}, []int{60, 30, 30}},
			{"unmeasured", []time.Duration{0, 0, 0}, []int{40, 40, 40}},
		} {
			t.Run(fmt.Sprintf("provided=%v/%s", provided, test.name), func(t *testing.T) {
				limiter, client := redisTestLimiter(t)
				ctx := context.Background()
				routes := make([]Route, len(test.latency))
				for i, latency := range test.latency {
					routes[i] = healthTestRoute(t, client)
					routes[i].UseProvidedKey = provided
					if latency > 0 {
						if err := limiter.ObserveResponseHeaderLatency(ctx, routes[i].ID, latency); err != nil {
							t.Fatal(err)
						}
					}
				}
				counts := make(map[string]int)
				for i := 0; i < 120; i++ {
					// Separate limiter objects simulate requests served by different
					// application instances; release before the next request.
					candidates := append([]Route(nil), routes...)
					if i%2 == 0 {
						candidates[0], candidates[2] = candidates[2], candidates[0]
					}
					lease, selected, err := NewLimiter(client).AcquireBest(ctx, candidates, fmt.Sprint(i))
					if err != nil || lease == nil || selected == nil {
						t.Fatalf("acquire %d: %v", i, err)
					}
					counts[selected.ID]++
					lease.Release(ctx)
				}
				for i, route := range routes {
					if got := counts[route.ID]; got != test.want[i] {
						t.Fatalf("route %d got %d requests, want %d; counts=%v", i, got, test.want[i], counts)
					}
					if ttl := client.PTTL(ctx, schedulerBalanceKey(route.ID)).Val(); ttl < 29*time.Minute || ttl > routeLatencyTTL {
						t.Fatalf("balance state TTL=%s", ttl)
					}
				}
			})
		}
	}
}

func TestSchedulerRedisSpreadsBurstBeforeSoftCap(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	routes := []Route{healthTestRoute(t, client), healthTestRoute(t, client), healthTestRoute(t, client)}
	for i := range routes {
		routes[i].UseProvidedKey = true
		if err := limiter.ObserveResponseHeaderLatency(ctx, routes[i].ID, time.Duration(i+1)*100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	counts := make(map[string]int)
	for i := 0; i < 9; i++ {
		lease, selected, err := limiter.AcquireBest(ctx, routes, fmt.Sprint(i))
		if err != nil || lease == nil || selected == nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		t.Cleanup(func() { lease.Release(ctx) })
		counts[selected.ID]++
	}
	for _, route := range routes {
		if counts[route.ID] == 0 || counts[route.ID] >= providedKeySoftConcurrency {
			t.Fatalf("burst did not use all healthy routes before filling one: %v", counts)
		}
	}
	if counts[routes[0].ID] <= counts[routes[1].ID] || counts[routes[0].ID] <= counts[routes[2].ID] {
		t.Fatalf("fast route lost preference: %v", counts)
	}
}

func TestSchedulerRedisSharesConcurrentTrafficAcrossInstances(t *testing.T) {
	_, client := redisTestLimiter(t)
	ctx := context.Background()
	routes := []Route{healthTestRoute(t, client), healthTestRoute(t, client), healthTestRoute(t, client)}
	for i := range routes {
		routes[i].UseProvidedKey = true
		if err := client.Set(ctx, latencyKey(routes[i].ID), 100, routeLatencyTTL).Err(); err != nil {
			t.Fatal(err)
		}
	}
	counts := make(map[string]int)
	var mu sync.Mutex
	var wait sync.WaitGroup
	for i := 0; i < 24; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			lease, selected, err := NewLimiter(client).AcquireBest(ctx, routes, fmt.Sprint(i))
			if err != nil || lease == nil || selected == nil {
				t.Errorf("concurrent acquire: %v", err)
				return
			}
			t.Cleanup(func() { lease.Release(ctx) })
			mu.Lock()
			counts[selected.ID]++
			mu.Unlock()
		}(i)
	}
	wait.Wait()
	for _, route := range routes {
		if counts[route.ID] != 8 {
			t.Fatalf("concurrent shares=%v, want 8 each", counts)
		}
	}
}

func TestSchedulerRedisAdaptsLatencyAndEligibilityWithoutCatchup(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	a, b := healthTestRoute(t, client), healthTestRoute(t, client)
	routes := []Route{a, b}
	run := func(candidates []Route, n int) map[string]int {
		t.Helper()
		counts := make(map[string]int)
		for i := 0; i < n; i++ {
			lease, selected, err := limiter.AcquireBest(ctx, candidates, fmt.Sprint(i))
			if err != nil || lease == nil || selected == nil {
				t.Fatalf("acquire: %v", err)
			}
			counts[selected.ID]++
			lease.Release(ctx)
		}
		return counts
	}
	for phase := 0; phase < 2; phase++ {
		fast, slow := a, b
		if phase == 1 {
			fast, slow = b, a
		}
		if err := client.Set(ctx, latencyKey(fast.ID), 20, routeLatencyTTL).Err(); err != nil {
			t.Fatal(err)
		}
		if err := client.Set(ctx, latencyKey(slow.ID), 200, routeLatencyTTL).Err(); err != nil {
			t.Fatal(err)
		}
		counts := run(routes, 60)
		if counts[fast.ID] < 39 || counts[fast.ID] > 41 || counts[slow.ID] < 19 {
			t.Fatalf("phase %d ignored new latency: %v", phase, counts)
		}
	}
	// Simulate old credit plus a route disappearing from the current model's
	// candidate list for a long period. Rejoining must have bounded catch-up.
	if err := client.Set(ctx, schedulerBalanceKey(b.ID), 1000000, routeLatencyTTL).Err(); err != nil {
		t.Fatal(err)
	}
	_ = run([]Route{a}, 100)
	counts := run(routes, 60)
	if counts[a.ID] < 18 || counts[b.ID] > 42 {
		t.Fatalf("rejoining route monopolized catch-up traffic: %v", counts)
	}
	if err := limiter.removeRouteRuntime(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if exists := client.Exists(ctx, schedulerBalanceKey(b.ID)).Val(); exists != 0 {
		t.Fatal("route deletion retained balancing state")
	}
}

func BenchmarkSchedulerRedisWeightedBalance(b *testing.B) {
	rawURL := os.Getenv("MODEL_PROXY_TEST_REDIS_URL")
	if rawURL == "" {
		b.Skip("MODEL_PROXY_TEST_REDIS_URL is not configured")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		b.Fatal(err)
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		b.Skip("local benchmark Redis is unavailable")
	}
	for _, count := range []int{6, 60} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			routes := make([]Route, count)
			keys := make([]string, 0, count*3)
			for i := range routes {
				routes[i] = Route{ID: fmt.Sprintf("balance-benchmark-%d-%d", time.Now().UnixNano(), i), ConcurrencyLimit: 50, UseProvidedKey: true}
				keys = append(keys, leaseKey(routes[i].ID), schedulerBalanceKey(routes[i].ID), latencyKey(routes[i].ID))
			}
			defer client.Del(ctx, keys...)
			limiter := NewLimiter(client)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease, _, err := limiter.AcquireBest(ctx, routes, fmt.Sprint(i))
				if err != nil || lease == nil {
					b.Fatalf("acquire: %v", err)
				}
				lease.Release(ctx)
			}
		})
	}
}
