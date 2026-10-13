package model_proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func healthTestRoute(t *testing.T, client *redis.Client) Route {
	t.Helper()
	id, err := randomID(t.Name() + "-")
	if err != nil {
		t.Fatal(err)
	}
	route := Route{ID: id, ConcurrencyLimit: 50, Enabled: true}
	cleanupRedisTestRoutes(t, client, []Route{route})
	return route
}

func acquireTestLease(t *testing.T, limiter *Limiter, route Route, id string) *Lease {
	t.Helper()
	lease, ok, err := limiter.Acquire(context.Background(), route, id)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	t.Cleanup(func() { lease.Release(context.Background()) })
	return lease
}

func failTestRoute(t *testing.T, limiter *Limiter, lease *Lease) {
	t.Helper()
	for index := 0; index < routeFailureThreshold; index++ {
		paused, err := limiter.ObserveRouteResult(context.Background(), lease, false,
			routeProbeSpec{GroupID: "group", ModelID: "test-model", Path: "/v1/responses"})
		if err != nil || paused != (index == routeFailureThreshold-1) {
			t.Fatalf("failure %d: paused=%v err=%v", index, paused, err)
		}
	}
}

func forceProbeDue(t *testing.T, client *redis.Client, routeID string) {
	t.Helper()
	if err := client.ZAdd(context.Background(), probeDueKey(), redis.Z{Score: float64(time.Now().Add(-time.Minute).UnixMilli()), Member: routeID}).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRouteHealthRedisPauseRecoveryAndStaleResults(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	lease := acquireTestLease(t, limiter, route, "original")
	if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || !allowed {
		t.Fatalf("healthy retry blocked: %v %v", allowed, err)
	}
	failTestRoute(t, limiter, lease)
	if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || allowed {
		t.Fatalf("paused retry allowed: %v %v", allowed, err)
	}
	routes := []Route{route}
	if err := limiter.PopulateRouteRuntime(ctx, routes); err != nil {
		t.Fatal(err)
	}
	if routes[0].PausedUntil == nil || routes[0].NextProbeAt == nil || routes[0].ConsecutiveFailures != 3 {
		t.Fatalf("missing paused runtime: %+v", routes[0])
	}
	if delay := time.Until(*routes[0].PausedUntil); delay < 29*time.Minute || delay > 30*time.Minute+time.Second {
		t.Fatalf("pause=%s", delay)
	}
	if delay := time.Until(*routes[0].NextProbeAt); delay < 179*time.Second || delay > 3*time.Minute+time.Second {
		t.Fatalf("probe delay=%s", delay)
	}
	if _, _, err := limiter.AcquireBest(ctx, routes, "blocked"); !errors.Is(err, errRoutePaused) {
		t.Fatalf("best acquisition must be paused: %v", err)
	}
	if _, _, err := limiter.Acquire(ctx, route, "direct-blocked"); !errors.Is(err, errRoutePaused) {
		t.Fatalf("direct acquisition must be paused: %v", err)
	}
	if claim, err := limiter.claimRouteProbe(ctx, route.ID); err != nil || claim != nil {
		t.Fatalf("probe claimed too early: %+v %v", claim, err)
	}
	if paused, err := limiter.ObserveRouteResult(ctx, lease, true, routeProbeSpec{}); err != nil || !paused {
		t.Fatalf("late success reopened route: %v %v", paused, err)
	}
	forceProbeDue(t, client, route.ID)
	claim, err := limiter.claimRouteProbe(ctx, route.ID)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if claim.spec.GroupID != "group" || claim.spec.ModelID != "test-model" {
		t.Fatalf("lost probe target: %+v", claim.spec)
	}
	until := routes[0].PausedUntil.UnixMilli()
	if restored, err := limiter.finishRouteProbe(ctx, claim, false); err != nil || restored {
		t.Fatalf("failed probe restored route: %v %v", restored, err)
	}
	if err := limiter.PopulateRouteRuntime(ctx, routes); err != nil {
		t.Fatal(err)
	}
	if routes[0].PausedUntil == nil || routes[0].PausedUntil.UnixMilli() != until {
		t.Fatal("failed probe extended pause")
	}
	forceProbeDue(t, client, route.ID)
	claim, err = limiter.claimRouteProbe(ctx, route.ID)
	if err != nil || claim == nil {
		t.Fatalf("second claim: %+v %v", claim, err)
	}
	if restored, err := limiter.finishRouteProbe(ctx, claim, true); err != nil || !restored {
		t.Fatalf("successful probe did not restore: %v %v", restored, err)
	}
	if _, err := limiter.ObserveRouteResult(ctx, lease, false, routeProbeSpec{}); err != nil {
		t.Fatal(err)
	}
	if err := limiter.PopulateRouteRuntime(ctx, routes); err != nil {
		t.Fatal(err)
	}
	if routes[0].PausedUntil != nil || routes[0].ConsecutiveFailures != 0 {
		t.Fatalf("late failure damaged recovered route: %+v", routes[0])
	}
	newLease := acquireTestLease(t, limiter, route, "recovered")
	if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || allowed {
		t.Fatalf("stale retry after recovery allowed: %v %v", allowed, err)
	}
	if newLease.epoch == lease.epoch {
		t.Fatal("recovery did not change epoch")
	}
}

func TestRouteLatencyRedisBatchUpdatesEWMA(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	a, b := healthTestRoute(t, client), healthTestRoute(t, client)
	ctx := context.Background()
	if err := limiter.ObserveResponseHeaderLatency(ctx, a.ID, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := limiter.observeResponseHeaderLatencies(ctx, map[string]routeLatencyAggregate{
		a.ID: {total: 400 * time.Millisecond, count: 2},
		b.ID: {total: time.Second, count: 2},
	}); err != nil {
		t.Fatal(err)
	}
	for routeID, want := range map[string]int64{a.ID: 120, b.ID: 500} {
		got, err := client.Get(ctx, latencyKey(routeID)).Int64()
		if err != nil || got != want {
			t.Fatalf("latency %s=%d want %d: %v", routeID, got, want, err)
		}
	}
}

func TestRouteHealthRedisSuccessResetsConsecutiveFailures(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	route := healthTestRoute(t, client)
	lease := acquireTestLease(t, limiter, route, "reset")
	for _, success := range []bool{false, false, true, false, false} {
		if paused, err := limiter.ObserveRouteResult(context.Background(), lease, success, routeProbeSpec{}); err != nil || paused {
			t.Fatalf("premature pause: %v %v", paused, err)
		}
	}
	if paused, err := limiter.ObserveRouteResult(context.Background(), lease, false, routeProbeSpec{}); err != nil || !paused {
		t.Fatalf("third consecutive failure should pause: %v %v", paused, err)
	}
}

func TestRouteHealthRedisDistributedProbeClaimsAndFencing(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	failTestRoute(t, limiter, acquireTestLease(t, limiter, route, "fence"))
	forceProbeDue(t, client, route.ID)
	var claims atomic.Int64
	var winner *routeProbeClaim
	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claim, err := NewLimiter(client).claimRouteProbe(ctx, route.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claim != nil {
				claims.Add(1)
				winner = claim
			}
		}()
	}
	wait.Wait()
	if claims.Load() != 1 {
		t.Fatalf("distributed claims=%d want 1", claims.Load())
	}
	forceProbeDue(t, client, route.ID) // Simulate an expired worker's lock.
	replacement, err := limiter.claimRouteProbe(ctx, route.ID)
	if err != nil || replacement == nil {
		t.Fatalf("replacement: %+v %v", replacement, err)
	}
	if restored, err := limiter.finishRouteProbe(ctx, winner, true); err != nil || restored {
		t.Fatalf("stale worker restored route: %v %v", restored, err)
	}
	if restored, err := limiter.finishRouteProbe(ctx, replacement, true); err != nil || !restored {
		t.Fatalf("current worker failed to restore: %v %v", restored, err)
	}
}

func TestRouteHealthRedisPauseExpiresWithoutTrafficOrProbeSuccess(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	oldLease := acquireTestLease(t, limiter, route, "expiry")
	failTestRoute(t, limiter, oldLease)
	if err := client.HSet(ctx, healthKey(route.ID), "paused_until", time.Now().Add(-time.Second).UnixMilli()).Err(); err != nil {
		t.Fatal(err)
	}
	forceProbeDue(t, client, route.ID)
	if claim, err := limiter.claimRouteProbe(ctx, route.ID); err != nil || claim != nil {
		t.Fatalf("expired pause still probed: %+v %v", claim, err)
	}
	if _, err := client.ZScore(ctx, probeDueKey(), route.ID).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expired route remains in probe queue: %v", err)
	}
	acquireTestLease(t, limiter, route, "after-expiry")
}

func TestRouteHealthRedisProbeRespectsHardCapacity(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	leases := make([]*Lease, route.ConcurrencyLimit)
	for index := range leases {
		leases[index] = acquireTestLease(t, limiter, route, fmt.Sprint(index))
	}
	failTestRoute(t, limiter, leases[0])
	forceProbeDue(t, client, route.ID)
	claim, err := limiter.claimRouteProbe(ctx, route.ID)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, ok, err := limiter.acquireRouteLease(ctx, route, claim.token, 50, claim.token); err != nil || ok {
		t.Fatalf("probe exceeded hard cap: %v %v", ok, err)
	}
	leases[0].Release(ctx)
	probeLease, ok, err := limiter.acquireRouteLease(ctx, route, claim.token, 50, claim.token)
	if err != nil || !ok {
		t.Fatalf("probe couldn't use free hard capacity: %v %v", ok, err)
	}
	defer probeLease.Release(ctx)
	if active, err := limiter.Active(ctx, route.ID); err != nil || active != 50 {
		t.Fatalf("active=%d %v", active, err)
	}
}

func TestAtomicSchedulerRedisConcurrentHardCapsAndPriority(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	high := healthTestRoute(t, client)
	high.Priority = 2
	low := healthTestRoute(t, client)
	low.Priority, low.UseProvidedKey = 1, true
	var acquired atomic.Int64
	var mu sync.Mutex
	var leases []*Lease
	var wait sync.WaitGroup
	for index := 0; index < 256; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			lease, selected, err := NewLimiter(client).AcquireBest(ctx, []Route{low, high}, fmt.Sprintf("parallel-%d", index))
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if lease == nil {
				return
			}
			acquired.Add(1)
			if selected.ID == low.ID {
				if active, err := limiter.Active(ctx, high.ID); err != nil || active != 50 {
					t.Errorf("lower priority selected while high had %d slots: %v", active, err)
				}
			}
			mu.Lock()
			leases = append(leases, lease)
			mu.Unlock()
		}(index)
	}
	wait.Wait()
	defer func() {
		for _, lease := range leases {
			lease.Release(ctx)
		}
	}()
	if acquired.Load() != 100 {
		t.Fatalf("acquired=%d want 100", acquired.Load())
	}
	for _, route := range []Route{high, low} {
		if active, err := limiter.Active(ctx, route.ID); err != nil || active != 50 {
			t.Fatalf("route %s active=%d %v", route.ID, active, err)
		}
	}
}

func TestAtomicSchedulerRedisSkipsPausedHighPriority(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	high, low := healthTestRoute(t, client), healthTestRoute(t, client)
	high.Priority, low.Priority = 100, 1
	failTestRoute(t, limiter, acquireTestLease(t, limiter, high, "fail-high"))
	lease, selected, err := limiter.AcquireBest(context.Background(), []Route{high, low}, "healthy-fallback")
	if err != nil || lease == nil || selected.ID != low.ID {
		t.Fatalf("healthy fallback=%+v %v", selected, err)
	}
	lease.Release(context.Background())
}
