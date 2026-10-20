package model_proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func TestMemberNeverAutoPausePersistsAndIsolatesGroups(t *testing.T) {
	repo := memberPauseTestRepository(t)
	route := Route{ID: "never-pause-member"}
	seedPauseMember(t, repo, route, "a", "b")
	service := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	never := true
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{NeverAutoPause: &never}); err != nil {
		t.Fatal(err)
	}
	reloaded := NewRepository(repo.db)
	for _, group := range []string{"a", "b"} {
		members, err := reloaded.ListGroupRoutes(ctx, group, true, "model")
		if err != nil || len(members) != 1 || members[0].NeverAutoPause != (group == "a") {
			t.Fatalf("group %s: %+v %v", group, members, err)
		}
	}
	direct, err := reloaded.GetRoute(ctx, route.ID)
	if err != nil || direct == nil || direct.NeverAutoPause {
		t.Fatalf("direct route affected: %+v %v", direct, err)
	}
	priority := 2
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	members, err := reloaded.ListGroupRoutes(ctx, "a", true, "model")
	if err != nil || len(members) != 1 || !members[0].NeverAutoPause || members[0].Priority != priority {
		t.Fatalf("partial update lost flag: %+v %v", members, err)
	}
	// Previously queued probes must not call a member that now ignores circuits.
	service.repository = reloaded
	probeRoute, err := service.loadProbeRoute(ctx, &routeProbeClaim{routeID: route.ID, spec: routeProbeSpec{GroupID: "a", ModelID: "model", Path: "/v1/responses"}})
	if err != nil || probeRoute != nil {
		t.Fatalf("automatic probe still uses opted-out member: %+v %v", probeRoute, err)
	}
	never = false
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{NeverAutoPause: &never}); err != nil {
		t.Fatal(err)
	}
	members, err = reloaded.ListGroupRoutes(ctx, "a", true, "model")
	if err != nil || len(members) != 1 || members[0].NeverAutoPause {
		t.Fatalf("could not disable flag: %+v %v", members, err)
	}
}

func TestNeverAutoPauseFailuresDoNotChangeSharedHealth(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	route.NeverAutoPause = true
	spec := routeProbeSpec{GroupID: "group", ModelID: "model", Path: "/v1/responses"}
	service := &Service{limiter: limiter}
	for _, status := range []int{503, 401, 429, 503, 401, 503} {
		lease, selected, err := limiter.AcquireBest(ctx, []Route{route}, fmt.Sprintf("failed-%d", status), spec)
		if err != nil || lease == nil || selected == nil {
			t.Fatalf("member did not schedule: %v", err)
		}
		if service.observeRouteRequest(ctx, lease, spec, &http.Response{StatusCode: status}, nil, route) {
			t.Fatal("opted-out member paused")
		}
		if paused, err := limiter.ObserveRouteResult(ctx, lease, false, spec); err != nil || paused {
			t.Fatalf("route observation paused member: %t %v", paused, err)
		}
		if paused, err := limiter.ObserveModelResult(ctx, lease, false, true, route, spec); err != nil || paused {
			t.Fatalf("model observation isolated member: %t %v", paused, err)
		}
		lease.Release(ctx)
	}
	if client.Exists(ctx, healthKey(route.ID), modelHealthKey(modelHealthID(route, spec))).Val() != 0 {
		t.Fatal("opted-out failures changed shared circuit state")
	}
}

func TestNeverAutoPauseBypassesExistingCircuitsButPreservesManualControls(t *testing.T) {
	for _, circuit := range []string{"route", "model", "both"} {
		t.Run(circuit, func(t *testing.T) {
			limiter, client := redisTestLimiter(t)
			ctx := context.Background()
			route := healthTestRoute(t, client)
			spec := routeProbeSpec{ModelID: "model", Path: "/v1/responses"}
			if circuit != "route" {
				failTestModel(t, limiter, route, spec).Release(ctx)
			}
			if circuit != "model" {
				lease := acquireTestLease(t, limiter, route, "route-failures")
				failTestRoute(t, limiter, lease)
				lease.Release(ctx)
			}
			modelKey := modelHealthKey(modelHealthID(route, spec))
			beforeRoute := client.HGetAll(ctx, healthKey(route.ID)).Val()
			beforeModel := client.HGetAll(ctx, modelKey).Val()
			route.NeverAutoPause = true
			route.ConcurrencyLimit = 1
			lease, selected, err := limiter.AcquireBest(ctx, []Route{route}, "ignore-auto-pause", spec)
			if err != nil || lease == nil || selected == nil {
				t.Fatalf("existing circuit blocked opted-out member: %v", err)
			}
			defer lease.Release(ctx)
			if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || !allowed {
				t.Fatalf("automatic circuit blocked retry: %t %v", allowed, err)
			}
			if extra, _, err := limiter.AcquireBest(ctx, []Route{route}, "over-hard-limit", spec); err != nil || extra != nil {
				t.Fatalf("hard concurrency limit bypassed: %+v %v", extra, err)
			}
			lease.Release(ctx)
			// A different membership and the direct route retain their isolation.
			other := route
			other.NeverAutoPause = false
			if _, _, err := limiter.AcquireBest(ctx, []Route{other}, "other-group", spec); !errors.Is(err, errRoutePaused) && !errors.Is(err, errModelPaused) {
				t.Fatalf("other group lost circuit: %v", err)
			}
			if _, _, err := limiter.AcquireForModel(ctx, other, "direct", spec); !errors.Is(err, errRoutePaused) && !errors.Is(err, errModelPaused) {
				t.Fatalf("direct route lost circuit: %v", err)
			}
			if !reflect.DeepEqual(beforeRoute, client.HGetAll(ctx, healthKey(route.ID)).Val()) || !reflect.DeepEqual(beforeModel, client.HGetAll(ctx, modelKey).Val()) {
				t.Fatal("ignoring automatic isolation changed shared circuit state")
			}
			route.SchedulingPaused = true
			if _, _, err := limiter.AcquireBest(ctx, []Route{route}, "manual-pause", spec); !errors.Is(err, errRoutePaused) {
				t.Fatalf("manual pause bypassed: %v", err)
			}
			route.SchedulingPaused = false
			route.RemovedModels = map[string]RemovedRouteModel{modelHealthID(route, spec): {ModelID: "model", Protocol: spec.Path}}
			if _, _, err := limiter.AcquireBest(ctx, []Route{route}, "removed-model", spec); !errors.Is(err, errModelPaused) {
				t.Fatalf("persisted model removal bypassed: %v", err)
			}
			route.RemovedModels = nil
			if err := client.HSet(ctx, modelKey, "isolated", 1, "probe_disabled", 1).Err(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := limiter.AcquireBest(ctx, []Route{route}, "runtime-removed-model", spec); !errors.Is(err, errModelPaused) {
				t.Fatalf("runtime model removal bypassed: %v", err)
			}
			if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || allowed {
				t.Fatalf("retry bypassed manual model removal: %t %v", allowed, err)
			}
			if err := limiter.removeRouteRuntime(ctx, route.ID); err != nil {
				t.Fatal(err)
			}
			if allowed, err := limiter.routeRetryAllowed(ctx, lease); err != nil || allowed {
				t.Fatalf("retry bypassed route deletion: %t %v", allowed, err)
			}
		})
	}
}

func TestNeverAutoPauseCompactRetainsRecentSuccessOrdering(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	spec := routeProbeSpec{ModelID: "model", Path: compactPath}
	failTestModel(t, limiter, route, spec).Release(ctx)
	route.NeverAutoPause = true
	lease, _, err := limiter.AcquireBest(ctx, []Route{route}, "compact-opt-out", spec)
	if err != nil || lease == nil {
		t.Fatalf("Compact isolation blocked member: %v", err)
	}
	defer lease.Release(ctx)
	service := &Service{limiter: limiter}
	if service.observeRouteRequest(ctx, lease, spec, &http.Response{StatusCode: 200}, nil, route) {
		t.Fatal("successful opted-out Compact request paused")
	}
	key := modelHealthKey(modelHealthID(route, spec))
	if !client.HExists(ctx, key, "last_success_at").Val() || client.HGet(ctx, key, "isolated").Val() != "1" {
		t.Fatal("Compact success did not preserve independent history and shared isolation")
	}
	service.observeRouteRequest(ctx, lease, spec, &http.Response{StatusCode: 503}, nil, route)
	if client.HExists(ctx, key, "last_success_at").Val() || client.HGet(ctx, key, "failures").Val() != "3" {
		t.Fatal("Compact failure changed circuit counters or retained stale success")
	}
}
