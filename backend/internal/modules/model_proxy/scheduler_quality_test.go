package model_proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestSchedulerRedisTemporaryPreferenceDependsOnModelSuccess(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	temporary, provided := healthTestRoute(t, client), healthTestRoute(t, client)
	temporary.NeverAutoPause = true
	provided.UseProvidedKey = true
	routes := []Route{provided, temporary}
	spec := routeProbeSpec{ModelID: "model", Path: "/v1/responses"}
	id := modelHealthID(temporary, spec)
	selectRoute := func(want string) {
		t.Helper()
		lease, selected, err := limiter.AcquireBest(ctx, routes, fmt.Sprint(time.Now().UnixNano()), spec)
		if err != nil || lease == nil || selected == nil {
			t.Fatalf("acquire: %v", err)
		}
		defer lease.Release(ctx)
		if selected.ID != want {
			t.Fatalf("selected %s, want %s", selected.ID, want)
		}
	}
	selectRoute(temporary.ID)
	for i := 0; i < 2; i++ {
		if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
			id: {RouteID: temporary.ID, ModelID: id, Failures: 1, ConsecutiveFailures: 1},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		selectRoute(provided.ID)
	}
	// Never-auto-pause is a scheduling preference change, not a new circuit.
	lease, selected, err := limiter.AcquireBest(ctx, []Route{temporary}, "degraded-only", spec)
	if err != nil || selected == nil || lease == nil {
		t.Fatalf("degraded member became paused: %v", err)
	}
	lease.Release(ctx)
	// Inference priorities still precede health and temporary-key preference.
	routes[1].Priority++
	selectRoute(temporary.ID)
	routes[1].Priority--
	// One broken model does not penalize another model or API protocol.
	for _, other := range []routeProbeSpec{
		{ModelID: "other", Path: spec.Path},
		{ModelID: spec.ModelID, Path: "/v1/chat/completions"},
	} {
		lease, selected, err := limiter.AcquireBest(ctx, routes, "different-model", other)
		if err != nil || selected == nil || lease == nil || selected.ID != temporary.ID {
			t.Fatalf("failure leaked across model/protocol: %v %v", selected, err)
		}
		lease.Release(ctx)
	}
	if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
		id: {RouteID: temporary.ID, ModelID: id, Successes: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	selectRoute(temporary.ID)
	if ttl := client.PTTL(ctx, schedulerQualityKey(id)).Val(); ttl <= 0 || ttl > routeLatencyTTL {
		t.Fatalf("quality TTL=%s", ttl)
	}
	if err := limiter.removeRouteRuntime(ctx, temporary.ID); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, schedulerQualityKey(id)).Val() != 0 {
		t.Fatal("route deletion retained model quality")
	}
	if err := limiter.observeSchedulerQuality(ctx, map[string]schedulerQualityAggregate{
		id: {RouteID: temporary.ID, ModelID: id, Successes: 1},
	}, map[string]schedulerCredentialOutcome{temporary.ID: {Outcome: 1}}); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, schedulerQualityKey(id)).Val() != 0 {
		t.Fatal("late observations recreated a deleted route")
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
		status int
		path   string
		want   string
	}{
		{http.StatusUnauthorized, compactPath, temporary.ID},
		{http.StatusUnauthorized, "/v1/responses", provided.ID},
		{http.StatusOK, "/v1/responses", temporary.ID},
	} {
		service.observeSchedulingOutcome(ctx, temporary, &Lease{}, routeProbeSpec{ModelID: "failed", Path: test.path}, &http.Response{StatusCode: test.status}, test.status == 200)
		sample := <-service.latencySamples
		pending, credentials := map[string]schedulerQualityAggregate{}, map[string]schedulerCredentialOutcome{}
		addSchedulerQualitySample(pending, credentials, sample)
		if err := limiter.observeSchedulerQuality(ctx, pending, credentials); err != nil {
			t.Fatal(err)
		}
		lease, selected, err := limiter.AcquireBest(ctx, []Route{temporary, provided}, "other-model", routeProbeSpec{ModelID: "other", Path: "/v1/responses"})
		if err != nil || selected == nil || lease == nil || selected.ID != test.want {
			t.Fatalf("status=%d path=%s selected=%v err=%v", test.status, test.path, selected, err)
		}
		lease.Release(ctx)
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
