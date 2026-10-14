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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func failTestModel(t *testing.T, limiter *Limiter, route Route, spec routeProbeSpec) *Lease {
	t.Helper()
	requestID, _ := randomID("model-failure-")
	lease, ok, err := limiter.AcquireForModel(context.Background(), route, requestID, spec)
	if err != nil || !ok {
		t.Fatalf("model acquire: %v %v", ok, err)
	}
	t.Cleanup(func() { lease.Release(context.Background()) })
	for i := 0; i < routeFailureThreshold; i++ {
		paused, err := limiter.ObserveModelResult(context.Background(), lease, false, false, route, spec)
		if err != nil || paused != (i == routeFailureThreshold-1) {
			t.Fatalf("model failure %d: %v %v", i, paused, err)
		}
	}
	return lease
}

func forceModelProbeDue(t *testing.T, client *redis.Client, id string) {
	t.Helper()
	if err := client.ZAdd(context.Background(), modelProbeDueKey(), redis.Z{Score: float64(time.Now().Add(-time.Minute).UnixMilli()), Member: id}).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestModelHealthRedisBackoffIsolationAndFencing(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
	old := failTestModel(t, limiter, route, spec)
	id := modelHealthID(route, spec)
	checkDueOffset := func(want time.Duration) {
		t.Helper()
		due, err := client.ZScore(ctx, modelProbeDueKey(), id).Result()
		if err != nil {
			t.Fatal(err)
		}
		anchor, err := client.HGet(ctx, modelHealthKey(id), "isolated_at").Int64()
		if err != nil {
			t.Fatal(err)
		}
		offset := time.Duration(int64(due)-anchor) * time.Millisecond
		if offset != want {
			t.Fatalf("probe offset=%s want %s", offset, want)
		}
	}
	checkDueOffset(time.Minute)
	if _, _, err := limiter.AcquireForModel(ctx, route, "bad-traffic", spec); !errors.Is(err, errModelPaused) {
		t.Fatalf("isolated model allowed: %v", err)
	}
	good, ok, err := limiter.AcquireForModel(ctx, route, "good-traffic", routeProbeSpec{ModelID: "good", Path: spec.Path})
	if err != nil || !ok {
		t.Fatalf("other model blocked: %v %v", ok, err)
	}
	good.Release(ctx)
	for _, offset := range []time.Duration{3 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
		forceModelProbeDue(t, client, id)
		claim, err := limiter.claimModelProbe(ctx, id)
		if err != nil || claim == nil {
			t.Fatalf("claim: %+v %v", claim, err)
		}
		if _, err := limiter.finishModelProbe(ctx, claim, false, true); err != nil {
			t.Fatal(err)
		}
		checkDueOffset(offset)
	}
	if _, err := limiter.ObserveModelResult(ctx, old, true, false, route, spec); err != nil {
		t.Fatal(err)
	}
	if isolated, _ := client.HGet(ctx, modelHealthKey(id), "isolated").Result(); isolated != "1" {
		t.Fatal("late success reopened model")
	}
	rows := []Route{route}
	if err := limiter.PopulateRouteRuntime(ctx, rows); err != nil || rows[0].PausedUntil != nil || len(rows[0].ModelHealth) != 1 || rows[0].ModelHealth[0].NextProbeAt == nil || rows[0].ModelHealth[0].ProbeAttempts != 5 {
		t.Fatalf("model UI state=%+v %v", rows, err)
	}
	forceModelProbeDue(t, client, id)
	var claims atomic.Int64
	var claim *modelProbeClaim
	var wait sync.WaitGroup
	for i := 0; i < 64; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got, err := NewLimiter(client).claimModelProbe(ctx, id)
			if err != nil {
				t.Error(err)
			}
			if got != nil {
				claims.Add(1)
				claim = got
			}
		}()
	}
	wait.Wait()
	if claims.Load() != 1 {
		t.Fatalf("distributed model claims=%d", claims.Load())
	}
	forceModelProbeDue(t, client, id)
	replacement, _ := limiter.claimModelProbe(ctx, id)
	if restored, err := limiter.finishModelProbe(ctx, claim, true, true); err != nil || restored {
		t.Fatalf("stale probe restored model: %v %v", restored, err)
	}
	if restored, err := limiter.finishModelProbe(ctx, replacement, true, true); err != nil || !restored {
		t.Fatalf("current probe didn't restore: %v %v", restored, err)
	}
	if _, err := limiter.ObserveModelResult(ctx, old, false, false, route, spec); err != nil {
		t.Fatal(err)
	}
	newLease := failTestModel(t, limiter, route, spec)
	if newLease.modelEpoch == old.modelEpoch {
		t.Fatal("model recovery didn't fence old requests")
	}
	checkDueOffset(time.Minute)
}

func TestModelProbeMilestonesAndDelayedWork(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		attempts int
		elapsed  time.Duration
		next     time.Duration
		fallback time.Duration
		legacy   bool
	}{
		{name: "first probe takes 30 seconds", elapsed: 90 * time.Second, next: 3 * time.Minute},
		{name: "second probe takes 30 seconds", attempts: 1, elapsed: 210 * time.Second, next: 5 * time.Minute},
		{name: "third probe takes 30 seconds", attempts: 2, elapsed: 330 * time.Second, next: 10 * time.Minute},
		{name: "fourth probe takes 30 seconds", attempts: 3, elapsed: 630 * time.Second, next: 20 * time.Minute},
		{name: "missed milestone avoids immediate catch-up", elapsed: 4 * time.Minute, fallback: 2 * time.Minute},
		{name: "legacy isolation uses corrected interval", legacy: true, fallback: 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := healthTestRoute(t, client)
			spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
			failTestModel(t, limiter, route, spec)
			id := modelHealthID(route, spec)
			now, err := client.Time(ctx).Result()
			if err != nil {
				t.Fatal(err)
			}
			anchor := now.Add(-tc.elapsed).UnixMilli()
			if err := client.HSet(ctx, modelHealthKey(id), "isolated_at", anchor, "probe_attempts", tc.attempts).Err(); err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if err := client.HDel(ctx, modelHealthKey(id), "isolated_at").Err(); err != nil {
					t.Fatal(err)
				}
			}
			forceModelProbeDue(t, client, id)
			claim, err := limiter.claimModelProbe(ctx, id)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if _, err := limiter.finishModelProbe(ctx, claim, false, true); err != nil {
				t.Fatal(err)
			}
			due, err := client.ZScore(ctx, modelProbeDueKey(), id).Result()
			if err != nil {
				t.Fatal(err)
			}
			want := anchor + tc.next.Milliseconds()
			if tc.fallback != 0 {
				want = now.Add(tc.fallback).UnixMilli()
				if delta := int64(due) - want; delta < 0 || delta > 1000 {
					t.Fatalf("fallback due=%v want near %d", due, want)
				}
			} else if int64(due) != want {
				t.Fatalf("milestone due=%v want %d", due, want)
			}
		})
	}
}

func TestProxyModel503KeepsNineOtherModelsAvailable(t *testing.T) {
	for _, smart := range []bool{false, true} {
		t.Run(fmt.Sprint("smart=", smart), func(t *testing.T) {
			limiter, client := redisTestLimiter(t)
			var failedCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload.Model == "bad" {
					failedCalls.Add(1)
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":{"message":"no available channel for this model"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"output":[]}`)
			}))
			defer server.Close()
			service, route := preparedProbeService(t, server)
			service.limiter = limiter
			route.ID = healthTestRoute(t, client).ID
			if smart {
				repo := memberPauseTestRepository(t)
				service.repository = repo
				seedPauseMember(t, repo, route, "group")
				models := []Model{{ID: "bad"}}
				for i := 0; i < 9; i++ {
					models = append(models, Model{ID: fmt.Sprint("good-", i)})
				}
				if err := repo.ReplaceRouteModels(context.Background(), route.ID, models); err != nil {
					t.Fatal(err)
				}
			}
			request := func(model string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)))
				if smart {
					service.proxySmartGroup(w, r, SmartGroup{ID: "group"})
				} else {
					service.proxyDirect(w, r, route)
				}
				return w
			}
			for i := 0; i < 3; i++ {
				if w := request("bad"); w.Code != 502 {
					t.Fatalf("bad request=%d %s", w.Code, w.Body)
				}
			}
			wantCalls := int64(3 * directMaxAttempts)
			if smart {
				wantCalls = 3 * smartGroupAttempts
			}
			if failedCalls.Load() != wantCalls {
				t.Fatalf("failed calls=%d want %d", failedCalls.Load(), wantCalls)
			}
			if w := request("bad"); w.Code != 503 || !strings.Contains(w.Body.String(), "model_temporarily_paused") {
				t.Fatalf("paused model response=%d %s", w.Code, w.Body)
			}
			for i := 0; i < 9; i++ {
				if w := request(fmt.Sprint("good-", i)); w.Code != 200 {
					t.Fatalf("healthy model %d blocked: %d %s", i, w.Code, w.Body)
				}
			}
			if exists, _ := client.HExists(context.Background(), healthKey(route.ID), "paused_until").Result(); exists {
				t.Fatal("model 503 paused entire route")
			}
		})
	}
}

func TestProxyMissingModelImmediatelyIsolatesWithoutRetry(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"model_not_found"}}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.limiter = limiter
	route.ID = healthTestRoute(t, client).ID
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		service.proxyDirect(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"bad"}`)), route)
		if i == 1 && !strings.Contains(w.Body.String(), "model_temporarily_paused") {
			t.Fatal(w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("missing model retried %d times", calls.Load())
	}
}

func TestModelHealthAliasesProtocolAndPriorityIsolation(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	route, fallback := healthTestRoute(t, client), healthTestRoute(t, client)
	route.Priority, fallback.Priority = 100, 1
	route.ModelMappingEnabled, route.ModelMapping = true, map[string]string{"alias": "actual", "another-alias": "actual"}
	spec := routeProbeSpec{ModelID: "alias", Path: "/v1/responses"}
	failTestModel(t, limiter, route, spec)
	for _, source := range []string{"alias", "another-alias", "actual"} {
		lease, selected, err := limiter.AcquireBest(context.Background(), []Route{route, fallback}, source, routeProbeSpec{ModelID: source, Path: spec.Path})
		if err != nil || lease == nil || selected.ID != fallback.ID {
			t.Fatalf("alias fallback: %+v %v", selected, err)
		}
		lease.Release(context.Background())
	}
	lease, selected, err := limiter.AcquireBest(context.Background(), []Route{fallback, route}, "chat", routeProbeSpec{ModelID: "alias", Path: "/v1/chat/completions"})
	if err != nil || selected == nil || selected.ID != route.ID {
		t.Fatalf("other protocol priority affected: %+v %v", selected, err)
	}
	lease.Release(context.Background())
}

func TestModelProbeStreamsAndRespectsHardCapacity(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["stream"] != true || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("model probe isn't streaming: %v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.limiter, service.repository = limiter, repo
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "group")
	spec := routeProbeSpec{GroupID: "group", ModelID: "model", Path: "/v1/responses"}
	held := failTestModel(t, limiter, route, spec)
	id := modelHealthID(route, spec)
	for i := 1; i < 50; i++ {
		acquireTestLease(t, limiter, route, fmt.Sprint("full-", i))
	}
	forceModelProbeDue(t, client, id)
	service.probeIsolatedModel(context.Background(), id)
	if calls.Load() != 0 {
		t.Fatal("probe exceeded hard cap")
	}
	held.Release(context.Background())
	forceModelProbeDue(t, client, id)
	service.probeIsolatedModel(context.Background(), id)
	if calls.Load() != 1 {
		t.Fatalf("model probe calls=%d", calls.Load())
	}
	if isolated, _ := client.HGet(context.Background(), modelHealthKey(id), "isolated").Result(); isolated == "1" {
		t.Fatal("successful streaming probe didn't restore model")
	}
}

func TestProxyHiddenStreamErrorIsModelScoped(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"model_not_found\"}}\n\n")
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.limiter = limiter
	route.ID = healthTestRoute(t, client).ID
	spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
	w := httptest.NewRecorder()
	service.proxyDirect(w, httptest.NewRequest("POST", spec.Path, strings.NewReader(`{"model":"bad","stream":true}`)), route)
	if isolated, _ := client.HGet(context.Background(), modelHealthKey(modelHealthID(route, spec)), "isolated").Result(); isolated != "1" {
		t.Fatal("hidden stream error did not isolate model")
	}
	if paused, _ := client.HExists(context.Background(), healthKey(route.ID), "paused_until").Result(); paused {
		t.Fatal("stream model error paused entire route")
	}
}

func TestManualModelRecoveryRestoresOnlyTestedModel(t *testing.T) {
	service, route, client := manualProbeFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"output":[]}`) })
	ctx := context.Background()
	// Model circuits coexist with a route circuit in the fixture. Fenced model
	// requests can still finish, but manual testing must not clear their peers.
	if err := client.HDel(ctx, healthKey(route.ID), "paused_until").Err(); err != nil {
		t.Fatal(err)
	}
	members, err := service.repository.ListGroupRoutes(ctx, "a", false, "")
	if err != nil {
		t.Fatal(err)
	}
	member := members[0]
	one := routeProbeSpec{GroupID: "a", ModelID: "alias", Path: "/v1/responses"}
	two := routeProbeSpec{GroupID: "a", ModelID: "other", Path: "/v1/responses"}
	failTestModel(t, service.limiter, member, one).Release(ctx)
	failTestModel(t, service.limiter, member, two).Release(ctx)
	run := func() RouteTestResult {
		result, err := service.TestRouteModel(ctx, "user", route.ID, RouteTestRequest{GroupID: "a", ModelID: "alias", Protocol: "responses"})
		if err != nil || !result.Success || result.RecoveryToken == "" {
			t.Fatalf("manual model test: %+v %v", result, err)
		}
		return result
	}
	stale := run()
	id := modelHealthID(member, one)
	_ = client.HIncrBy(ctx, modelHealthKey(id), "epoch", 1).Err()
	requireRetest(t, service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: stale.RecoveryToken}))
	result := run()
	if err := service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: result.RecoveryToken}); err != nil {
		t.Fatal(err)
	}
	if isolated, _ := client.HGet(ctx, modelHealthKey(id), "isolated").Result(); isolated == "1" {
		t.Fatal("tested model still isolated")
	}
	if isolated, _ := client.HGet(ctx, modelHealthKey(modelHealthID(member, two)), "isolated").Result(); isolated != "1" {
		t.Fatal("manual recovery restored an untested model")
	}
	if _, err := client.ZScore(ctx, modelProbeDueKey(), id).Result(); !errors.Is(err, redis.Nil) {
		t.Fatal("restored model still queued")
	}
}

func TestProxyCredentialFailuresRemainRouteScoped(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.limiter = limiter
	route.ID = healthTestRoute(t, client).ID
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		service.proxyDirect(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"model-%d"}`, i))), route)
	}
	w := httptest.NewRecorder()
	service.proxyDirect(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"other"}`)), route)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "route_temporarily_paused") {
		t.Fatalf("invalid credential didn't pause route: %d %s", w.Code, w.Body)
	}
}
