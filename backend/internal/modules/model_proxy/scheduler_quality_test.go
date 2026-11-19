package model_proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestSchedulerRedisScoresModelSuccessWithoutKeyPreference(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	temporary, provided := healthTestRoute(t, client), healthTestRoute(t, client)
	temporary.NeverAutoPause = true
	provided.UseProvidedKey = true
	routes := []Route{provided, temporary}
	spec := routeProbeSpec{ModelID: "model", Path: "/v1/responses"}
	id := modelHealthID(temporary, spec)
	run := func(spec routeProbeSpec, n int) map[string]int {
		t.Helper()
		client.Del(ctx, schedulerBalanceKey(temporary.ID), schedulerBalanceKey(provided.ID))
		counts := map[string]int{}
		for i := 0; i < n; i++ {
			lease, selected, err := limiter.AcquireBest(ctx, routes, fmt.Sprint(i), spec)
			if err != nil || lease == nil || selected == nil {
				t.Fatalf("acquire: %v", err)
			}
			counts[selected.ID]++
			lease.Release(ctx)
		}
		return counts
	}
	if counts := run(spec, 100); counts[temporary.ID] != 50 || counts[provided.ID] != 50 {
		t.Fatalf("key type changed equal quality shares: %v", counts)
	}
	for i := 0; i < 2; i++ {
		if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
			id: {RouteID: temporary.ID, ModelID: id, Failures: 1, ConsecutiveFailures: 1},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if counts := run(spec, 100); counts[provided.ID] < 90 || counts[temporary.ID] == 0 {
		t.Fatalf("low success rate did not lower share: %v", counts)
	}
	lease, selected, err := limiter.AcquireBest(ctx, []Route{temporary}, "degraded-only", spec)
	if err != nil || selected == nil || lease == nil {
		t.Fatalf("never-pause member became unavailable: %v", err)
	}
	lease.Release(ctx)
	// Priority remains a strict boundary, even for degraded never-pause members.
	routes[1].Priority++
	if counts := run(spec, 10); counts[temporary.ID] != 10 {
		t.Fatalf("priority boundary: %v", counts)
	}
	routes[1].Priority--
	for _, other := range []routeProbeSpec{
		{ModelID: "other", Path: spec.Path}, {ModelID: spec.ModelID, Path: "/v1/chat/completions"},
	} {
		if counts := run(other, 100); counts[temporary.ID] != 50 {
			t.Fatalf("failure leaked across model/protocol: %v", counts)
		}
	}
	for i := 0; i < 12; i++ {
		if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
			id: {RouteID: temporary.ID, ModelID: id, Successes: 1},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if counts := run(spec, 100); counts[temporary.ID] < 45 {
		t.Fatalf("success did not recover share: %v", counts)
	}
	if ttl := client.PTTL(ctx, schedulerQualityKey(id)).Val(); ttl <= 0 || ttl > routeLatencyTTL {
		t.Fatalf("quality TTL=%s", ttl)
	}
	if err := limiter.removeRouteRuntime(ctx, temporary.ID); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, schedulerQualityKey(id)).Val() != 0 {
		t.Fatal("route deletion retained quality")
	}
	if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
		id: {RouteID: temporary.ID, ModelID: id, Successes: 1},
	}, map[string]schedulerCredentialOutcome{temporary.ID: {Outcome: 1}}); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, schedulerQualityKey(id)).Val() != 0 {
		t.Fatal("late observations recreated deleted quality")
	}
}

func TestSchedulerQualitySeparatesAttemptsFromLogicalOutcomes(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	route.ModelMappingEnabled = true
	route.ModelMapping = map[string]string{"alias": "actual"}
	spec := routeProbeSpec{ModelID: "alias", Path: "/v1/responses"}
	id := modelHealthID(route, spec)
	if id != modelHealthID(Route{ID: route.ID}, routeProbeSpec{ModelID: "actual", Path: spec.Path}) {
		t.Fatal("alias does not share actual-model quality")
	}
	pending := map[string]schedulerQualityAggregate{}
	credentials := map[string]schedulerCredentialOutcome{}
	for _, sample := range []routeLatencyObservation{
		{routeID: route.ID, modelHealthID: id, latency: 90 * time.Second},
		{routeID: route.ID, modelHealthID: id, latency: time.Second},
		{routeID: route.ID, modelHealthID: id, outcome: 1, credential: 1},
	} {
		addSchedulerQualitySample(pending, credentials, sample)
	}
	if err := limiter.observeSchedulerQuality(ctx, pending, credentials); err != nil {
		t.Fatal(err)
	}
	if rate := client.HGet(ctx, schedulerQualityKey(id), "success_rate").Val(); rate != "1000" {
		t.Fatalf("successful retry lowered request success rate: %s", rate)
	}
	if streak := client.HGet(ctx, schedulerQualityKey(id), "consecutive_errors").Val(); streak != "0" {
		t.Fatalf("successful retry counted as a failed request: %s", streak)
	}
}

func TestSchedulerCredentialFailureChangesPreferenceAcrossModels(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	temporary, provided := healthTestRoute(t, client), healthTestRoute(t, client)
	temporary.NeverAutoPause = true
	provided.UseProvidedKey = true
	service := &Service{latencySamples: make(chan routeLatencyObservation, 1)}
	for _, test := range []struct {
		status   int
		path     string
		degraded bool
	}{
		{http.StatusUnauthorized, compactPath, false},
		{http.StatusUnauthorized, "/v1/responses", true},
		{http.StatusOK, "/v1/responses", false},
	} {
		service.observeSchedulingOutcome(ctx, temporary, &Lease{}, routeProbeSpec{ModelID: "failed", Path: test.path}, &http.Response{StatusCode: test.status}, test.status == 200)
		sample := <-service.latencySamples
		pending, credentials := map[string]schedulerQualityAggregate{}, map[string]schedulerCredentialOutcome{}
		addSchedulerQualitySample(pending, credentials, sample)
		if err := limiter.observeSchedulerQuality(ctx, pending, credentials); err != nil {
			t.Fatal(err)
		}
		client.Del(ctx, schedulerBalanceKey(temporary.ID), schedulerBalanceKey(provided.ID))
		counts := map[string]int{}
		for i := 0; i < 100; i++ {
			lease, selected, err := limiter.AcquireBest(ctx, []Route{temporary, provided}, fmt.Sprint(i), routeProbeSpec{ModelID: "other", Path: "/v1/responses"})
			if err != nil || lease == nil || selected == nil {
				t.Fatalf("acquire: %v", err)
			}
			counts[selected.ID]++
			lease.Release(ctx)
		}
		if test.degraded && counts[temporary.ID] > 5 || !test.degraded && counts[temporary.ID] != 50 {
			t.Fatalf("status=%d path=%s shares=%v", test.status, test.path, counts)
		}
	}
}

func TestSchedulerQualityWorkerBatchesNeverPauseOutcomesAndFencesRecovery(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{limiter: limiter, latencySamples: make(chan routeLatencyObservation, latencyObservationBuffer)}
	service.wg.Add(1)
	go service.latencyObservationLoop(ctx)
	defer func() { cancel(); service.wg.Wait() }()
	route := healthTestRoute(t, client)
	route.NeverAutoPause = true
	spec := routeProbeSpec{ModelID: "model", Path: "/v1/responses"}
	id := modelHealthID(route, spec)
	lease := &Lease{modelHealthID: id}
	for i := 0; i < 2; i++ {
		service.observeRouteRequest(ctx, lease, spec, &http.Response{StatusCode: http.StatusServiceUnavailable}, nil, route)
	}
	deadline := time.Now().Add(3 * time.Second)
	for client.HGet(ctx, schedulerQualityKey(id), "consecutive_errors").Val() != "2" {
		if time.Now().After(deadline) {
			t.Fatal("worker did not persist request outcomes")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rate := client.HGet(ctx, schedulerQualityKey(id), "success_rate").Val(); rate != "562" {
		t.Fatalf("batched failures lost sample weight: %s", rate)
	}
	// Restore advances the epoch. Already queued observations must not undo it.
	client.HIncrBy(ctx, healthKey(route.ID), "epoch", 1)
	client.Del(ctx, schedulerQualityKey(id))
	service.observeRouteRequest(ctx, lease, spec, &http.Response{StatusCode: http.StatusUnauthorized}, nil, route)
	// A synchronous flush of an old batch exercises the same fence as the worker.
	if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
		id: {RouteID: route.ID, ModelID: id, Failures: 1, ConsecutiveFailures: 1},
	}, map[string]schedulerCredentialOutcome{route.ID: {Outcome: -1}}); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, schedulerQualityKey(id)).Val() != 0 || client.HExists(ctx, healthKey(route.ID), "credential_failed_at").Val() {
		t.Fatal("stale observation changed restored scheduling quality")
	}
}
