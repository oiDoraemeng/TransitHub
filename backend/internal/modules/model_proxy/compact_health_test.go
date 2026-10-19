package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestCompactFailuresNeverScheduleRecoveryOrPauseOtherEndpoints(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	spec := routeProbeSpec{ModelID: "model", Path: compactPath}
	service := &Service{limiter: limiter}
	for i := 0; i < routeFailureThreshold; i++ {
		lease, ok, err := limiter.AcquireForModel(ctx, route, fmt.Sprint(i), spec)
		if err != nil || !ok {
			t.Fatalf("acquire: %t %v", ok, err)
		}
		// Even a Compact transport failure is local to Compact. It must not
		// create a route-wide probe that would call the Compact endpoint.
		paused := service.observeRouteRequest(ctx, lease, spec, nil, errors.New("network failure"), route)
		lease.Release(ctx)
		if paused != (i == routeFailureThreshold-1) {
			t.Fatalf("failure %d paused=%t", i, paused)
		}
	}
	id := modelHealthID(route, spec)
	if _, err := client.ZScore(ctx, modelProbeDueKey(), id).Result(); err != redis.Nil {
		t.Fatalf("Compact model recovery queued: %v", err)
	}
	if _, err := client.ZScore(ctx, probeDueKey(), route.ID).Result(); err != redis.Nil {
		t.Fatalf("Compact route recovery queued: %v", err)
	}
	if (&Service{}).performRouteProbe(ctx, Route{}, spec) {
		t.Fatal("Compact automatic probe reached inference")
	}
	if _, _, err := limiter.AcquireForModel(ctx, route, "isolated-compact", spec); !errors.Is(err, errModelPaused) {
		t.Fatalf("known Compact failure scheduled: %v", err)
	}
	other, ok, err := limiter.AcquireForModel(ctx, route, "responses", routeProbeSpec{ModelID: spec.ModelID, Path: "/v1/responses"})
	if err != nil || !ok {
		t.Fatalf("Compact failure blocked Responses: %t %v", ok, err)
	}
	other.Release(ctx)
	rows := []Route{route}
	if err := limiter.PopulateRouteRuntime(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].PausedUntil != nil || len(rows[0].ModelHealth) != 1 {
		t.Fatalf("invalid circuit state: %+v", rows[0])
	}
	state := rows[0].ModelHealth[0]
	if !state.AutomaticProbeDisabled || state.ProbeDisabled || state.NextProbeAt != nil {
		t.Fatalf("Compact must show automatic probes disabled, not manual removal: %+v", state)
	}
	w := httptest.NewRecorder()
	writeModelPaused(w, compactPath)
	if w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "automatic Compact probes are disabled") {
		t.Fatalf("Compact error advertises automatic recovery: %s", w.Body.String())
	}
}

func TestLegacyCompactRecoveryCleanup(t *testing.T) {
	for _, mode := range []string{"startup", "runtime", "claim"} {
		t.Run(mode, func(t *testing.T) {
			limiter, client := redisTestLimiter(t)
			ctx := context.Background()
			route := healthTestRoute(t, client)
			spec := routeProbeSpec{ModelID: "model", Path: compactPath}
			encoded, _ := json.Marshal(spec)
			id := modelHealthID(route, spec)
			otherID := modelHealthID(route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"})
			due := float64(time.Now().Add(-time.Minute).UnixMilli())
			pipeline := client.TxPipeline()
			pipeline.HSet(ctx, healthKey(route.ID), "epoch", 7, "paused_until", time.Now().Add(30*time.Minute).UnixMilli(), "probe_spec", string(encoded), "probe_token", "old-route", "failures", 3)
			pipeline.ZAdd(ctx, probeDueKey(), redis.Z{Member: route.ID, Score: due})
			pipeline.SAdd(ctx, routeModelIndexKey(route.ID), id, otherID)
			pipeline.HSet(ctx, modelHealthKey(id), "epoch", 5, "route_id", route.ID, "model_id", "model", "protocol", compactPath, "isolated", 1, "probe_spec", string(encoded), "probe_token", "old-model", "failures", 3)
			pipeline.HSet(ctx, modelHealthKey(otherID), "route_id", route.ID, "model_id", "model", "protocol", "/v1/responses", "isolated", 1, "failures", 3)
			pipeline.ZAdd(ctx, modelProbeDueKey(), redis.Z{Member: id, Score: due}, redis.Z{Member: otherID, Score: due})
			if _, err := pipeline.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "startup":
				if err := limiter.discardCompactRecovery(ctx); err != nil {
					t.Fatal(err)
				}
			case "claim":
				service := &Service{limiter: limiter}
				service.probePausedRoute(ctx, route.ID)
				service.probeIsolatedModel(ctx, id)
			}
			rows := []Route{route}
			if err := limiter.PopulateRouteRuntime(ctx, rows); err != nil {
				t.Fatal(err)
			}
			if rows[0].PausedUntil != nil || len(rows[0].ModelHealth) != 2 {
				t.Fatalf("cleanup changed model exclusions or kept route pause: %+v", rows[0])
			}
			for _, queue := range []struct{ key, member string }{{probeDueKey(), route.ID}, {modelProbeDueKey(), id}} {
				if _, err := client.ZScore(ctx, queue.key, queue.member).Result(); err != redis.Nil {
					t.Fatalf("Compact task remains queued: %v", err)
				}
			}
			if _, err := client.ZScore(ctx, modelProbeDueKey(), otherID).Result(); err != nil {
				t.Fatal("Responses probe was removed")
			}
			// Old workers must neither restore Compact nor recreate its queue.
			_, _ = limiter.finishModelProbe(ctx, &modelProbeClaim{id: id, routeProbeClaim: routeProbeClaim{routeID: route.ID, token: "old-model", epoch: 5, spec: spec}}, true, true)
			_, _ = limiter.finishRouteProbe(ctx, &routeProbeClaim{routeID: route.ID, token: "old-route", epoch: 7, spec: spec}, false, true)
			if client.HGet(ctx, modelHealthKey(id), "isolated").Val() != "1" || client.HExists(ctx, modelHealthKey(id), "probe_spec").Val() {
				t.Fatal("late probe result changed Compact state")
			}
			if epoch, _ := client.HGet(ctx, modelHealthKey(id), "epoch").Int64(); epoch <= 5 {
				t.Fatal("cleanup failed to fence legacy model worker")
			}
			if err := limiter.discardCompactRecovery(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompactSchedulerPrefersLatestActualModelSuccess(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	a, b := healthTestRoute(t, client), healthTestRoute(t, client)
	a.Priority, b.Priority = 5, 5
	// The same mapped upstream model shares its Compact success history.
	a.ModelMappingEnabled, b.ModelMappingEnabled = true, true
	a.ModelMapping = map[string]string{"alias": "target-a"}
	b.ModelMapping = map[string]string{"alias": "target-b"}
	b.UseProvidedKey = true
	b.ConcurrencyLimit = 1
	spec := routeProbeSpec{ModelID: "alias", Path: compactPath}
	for i, route := range []Route{a, b} {
		lease, ok, err := limiter.AcquireForModel(ctx, route, "success", spec)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if _, err := limiter.ObserveModelResult(ctx, lease, true, false, route, spec); err != nil {
			t.Fatal(err)
		}
		lease.Release(ctx)
		if err := client.HSet(ctx, modelHealthKey(modelHealthID(route, spec)), "last_success_at", 100+i).Err(); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(routes []Route, request routeProbeSpec, want string) *Lease {
		t.Helper()
		lease, selected, err := limiter.AcquireBest(ctx, routes, "latest", request)
		if err != nil || selected == nil || selected.ID != want {
			t.Fatalf("selected=%v want=%s err=%v", selected, want, err)
		}
		return lease
	}
	held := acquire([]Route{a, b}, spec, b.ID)
	// Latest success does not bypass hard capacity.
	acquire([]Route{a, b}, spec, a.ID).Release(ctx)
	held.Release(ctx)
	a.Priority = 6
	acquire([]Route{b, a}, spec, a.ID).Release(ctx)
	a.Priority = 5
	// Success from another protocol/model must not influence selection.
	acquire([]Route{a, b}, routeProbeSpec{ModelID: "alias", Path: "/v1/responses"}, a.ID).Release(ctx)
	acquire([]Route{a, b}, routeProbeSpec{ModelID: "different", Path: compactPath}, a.ID).Release(ctx)
	// A new failed request invalidates the preference before isolation at three.
	failed := acquire([]Route{a, b}, spec, b.ID)
	if _, err := limiter.ObserveModelResult(ctx, failed, false, false, b, spec); err != nil {
		t.Fatal(err)
	}
	failed.Release(ctx)
	acquire([]Route{a, b}, spec, a.ID).Release(ctx)
	// Deleted route cleanup also removes its remembered Compact successes.
	if err := limiter.removeRouteRuntime(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, modelHealthKey(modelHealthID(a, spec))).Val() != 0 {
		t.Fatal("deleted route retained Compact history")
	}
}

func TestCompactManualTestRecordsSuccessAndStillAllowsRestore(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != compactPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "compact-group")
	spec := routeProbeSpec{GroupID: "compact-group", ModelID: "model", Path: compactPath}
	_ = failTestModel(t, limiter, route, spec)
	id := modelHealthID(route, spec)
	result, err := service.TestRouteModel(context.Background(), "user", route.ID, RouteTestRequest{GroupID: spec.GroupID, ModelID: spec.ModelID, Protocol: "compact"})
	if err != nil || !result.Success || result.RecoveryToken == "" {
		t.Fatalf("manual Compact test: %+v %v", result, err)
	}
	if timestamp, _ := client.HGet(context.Background(), modelHealthKey(id), "last_success_at").Int64(); timestamp == 0 {
		t.Fatal("successful manual Compact test was not remembered")
	}
	if err := service.RestoreRouteScheduling(context.Background(), "user", route.ID, RouteRecoveryRequest{GroupID: spec.GroupID, Token: result.RecoveryToken}); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := limiter.AcquireForModel(context.Background(), route, "restored", spec)
	if err != nil || !ok {
		t.Fatalf("manual restore blocked: %t %v", ok, err)
	}
	lease.Release(context.Background())
	if client.HExists(context.Background(), modelHealthKey(id), "probe_spec").Val() {
		t.Fatal("manual restore recreated Compact probes")
	}
}

func TestCompactRealRequestRecordsSuccessfulDispatch(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter = repo, limiter
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "compact-live")
	group, err := repo.GetSmartGroup(context.Background(), "compact-live")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, compactPath, strings.NewReader(`{"model":"model","input":[]}`)), *group)
	if w.Code != http.StatusOK {
		t.Fatalf("request=%d %s", w.Code, w.Body.String())
	}
	id := modelHealthID(route, routeProbeSpec{ModelID: "model", Path: compactPath})
	if timestamp, _ := client.HGet(context.Background(), modelHealthKey(id), "last_success_at").Int64(); timestamp == 0 {
		t.Fatal("successful live Compact request was not remembered")
	}
}
