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

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"transithub/backend/internal/shared/authctx"
)

func TestManualProbeAcceptsOnlyInferenceProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "compact", "messages", "gemini"} {
		spec, err := manualProbeSpec(RouteTestRequest{ModelID: "model", Protocol: protocol})
		if err != nil || spec.Path == "/v1/models" {
			t.Fatalf("protocol=%s spec=%+v err=%v", protocol, spec, err)
		}
		request, _, err := newRouteProbeRequest(context.Background(), spec)
		if err != nil || request.Method != http.MethodPost {
			t.Fatalf("inference required: %v", err)
		}
	}
	for _, input := range []RouteTestRequest{{Protocol: "chat"}, {ModelID: "model", Protocol: "models"}, {ModelID: "model", Protocol: "embeddings"}, {ModelID: "../escape", Protocol: "gemini"}} {
		if _, err := manualProbeSpec(input); err == nil {
			t.Fatalf("invalid input accepted: %+v", input)
		}
	}
}

func TestManualProbeRedisExclusiveAndHardCapacity(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	route := healthTestRoute(t, client)
	route.ConcurrencyLimit = 1
	proofKey := manualRecoveryKey("user", "workspace", route.ID, "a")
	t.Cleanup(func() { _ = client.Del(context.Background(), manualTestKey(route.ID), proofKey).Err() })
	lease := acquireTestLease(t, limiter, route, "traffic")
	if _, err := limiter.acquireManualTest(context.Background(), route, proofKey); err == nil {
		t.Fatal("manual probe exceeded hard capacity")
	}
	failTestRoute(t, limiter, lease)
	lease.Release(context.Background())
	var winners atomic.Int64
	var wait sync.WaitGroup
	var held *Lease
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			probe, err := limiter.acquireManualTest(context.Background(), route, proofKey)
			if err == nil {
				winners.Add(1)
				mu.Lock()
				held = probe
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("parallel manual probes=%d", winners.Load())
	}
	defer held.Release(context.Background())
	if active, err := limiter.Active(context.Background(), route.ID); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	if _, _, err := limiter.Acquire(context.Background(), route, "business"); !errors.Is(err, errRoutePaused) {
		t.Fatalf("manual test reopened traffic: %v", err)
	}
	state, _ := client.HGetAll(context.Background(), healthKey(route.ID)).Result()
	if state["failures"] != "3" || state["paused_until"] == "" {
		t.Fatalf("manual test changed circuit: %v", state)
	}
}

func manualProbeFixture(t *testing.T, serve http.HandlerFunc) (*Service, Route, *redis.Client) {
	t.Helper()
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(serve)
	t.Cleanup(server.Close)
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "a", "b")
	paused, provided, mapping := true, true, true
	aliases := map[string]string{"alias": "model"}
	for _, group := range []string{"a", "b"} {
		if err := service.UpdateMemberPolicy(context.Background(), "user", group, route.ID, UpdateMemberPolicyRequest{
			SchedulingPaused: &paused, UseProvidedKey: &provided, UpstreamKey: "sk-member-test", ModelMappingEnabled: &mapping, ModelMapping: &aliases,
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		keys := []string{manualTestKey(route.ID)}
		for _, group := range []string{"", "a", "b"} {
			keys = append(keys, manualRecoveryKey("user", "workspace", route.ID, group))
		}
		_ = client.Del(context.Background(), keys...).Err()
	})
	lease := acquireTestLease(t, limiter, route, "failed-traffic")
	failTestRoute(t, limiter, lease)
	lease.Release(context.Background())
	return service, route, client
}

func requireRetest(t *testing.T, err error) {
	t.Helper()
	var request *requestError
	if !errors.As(err, &request) || request.Status != http.StatusConflict {
		t.Fatalf("want rejected recovery, got %v", err)
	}
}

func TestManualProbePostgresTestsPausedMemberThenRestoresOnlySelectedScope(t *testing.T) {
	service, route, client := manualProbeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer sk-member-test" {
			t.Errorf("wrong member binding: %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "model" {
			t.Errorf("mapping not applied: %v", payload)
		}
		_, _ = io.WriteString(w, `{"output":[{"type":"message"}]}`)
	})
	ctx := context.Background()
	models, err := service.ListRouteTestModels(ctx, "user", route.ID, "a", false)
	if err != nil || len(models) != 2 || models[0].ID != "alias" || models[1].ID != "model" {
		t.Fatalf("paused model selector=%+v %v", models, err)
	}
	result, err := service.TestRouteModel(ctx, "user", route.ID, RouteTestRequest{GroupID: "a", ModelID: "alias", Protocol: "responses"})
	if err != nil || !result.Success || result.RecoveryToken == "" || result.ExpiresAt == nil {
		t.Fatalf("test=%+v %v", result, err)
	}
	if ttl := time.Until(*result.ExpiresAt); ttl < 4*time.Minute || ttl > manualRecoveryTTL+time.Second {
		t.Fatalf("proof TTL=%s", ttl)
	}
	if until, _ := client.HGet(ctx, healthKey(route.ID), "paused_until").Result(); until == "" {
		t.Fatal("testing automatically restored circuit")
	}
	for _, group := range []string{"a", "b"} {
		rows, err := service.repository.ListGroupRoutes(ctx, group, false, "")
		if err != nil || !rows[0].SchedulingPaused {
			t.Fatal("testing automatically resumed a member")
		}
	}
	requireRetest(t, service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "b", Token: result.RecoveryToken}))
	requireRetest(t, service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{Token: result.RecoveryToken}))
	if err := service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: result.RecoveryToken}); err != nil {
		t.Fatal(err)
	}
	rows, err := service.repository.ListGroupRoutes(ctx, "a", true, "alias")
	if err != nil || len(rows) != 1 {
		t.Fatalf("restored member=%+v %v", rows, err)
	}
	other, _ := service.repository.ListGroupRoutes(ctx, "b", false, "")
	if !other[0].SchedulingPaused {
		t.Fatal("restoration resumed another manually paused member")
	}
	if until, _ := client.HGet(ctx, healthKey(route.ID), "paused_until").Result(); until != "" {
		t.Fatal("circuit still paused")
	}
	requireRetest(t, service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: result.RecoveryToken}))
}

func TestManualProbePostgresRejectsFailedExpiredChangedAndStaleTests(t *testing.T) {
	var fail atomic.Bool
	service, route, client := manualProbeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			_, _ = io.WriteString(w, `{"error":{"message":"secret must not reach UI"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	ctx := context.Background()
	input := RouteTestRequest{GroupID: "a", ModelID: "alias", Protocol: "chat"}
	run := func() RouteTestResult {
		result, err := service.TestRouteModel(ctx, "user", route.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	recover := func(result RouteTestResult) error {
		return service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: result.RecoveryToken})
	}
	first := run()
	fail.Store(true)
	failed := run()
	if failed.Success || failed.RecoveryToken != "" || strings.Contains(failed.Message, "secret") {
		t.Fatalf("failure leaked or gave recovery: %+v", failed)
	}
	requireRetest(t, recover(first))
	fail.Store(false)
	expired := run()
	_ = client.PExpireAt(ctx, manualRecoveryKey("user", "workspace", route.ID, "a"), time.Now().Add(-time.Second)).Err()
	requireRetest(t, recover(expired))
	changed := run()
	paused := true
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	requireRetest(t, recover(changed))
	stale := run()
	_ = client.HIncrBy(ctx, healthKey(route.ID), "epoch", 1).Err()
	requireRetest(t, recover(stale))
	rows, _ := service.repository.ListGroupRoutes(ctx, "a", false, "")
	if !rows[0].SchedulingPaused {
		t.Fatal("rejected recovery changed member state")
	}
	if _, err := service.TestRouteModel(ctx, "another-user", route.ID, input); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-workspace testing: %v", err)
	}
}

func TestManualProbeHTTPAuthenticationAndRecovery(t *testing.T) {
	service, route, _ := manualProbeFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"choices":[]}`) })
	mux := http.NewServeMux()
	RegisterRoutes(mux, service)
	call := func(method, suffix, body, user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/proxy-routes/"+route.ID+suffix, strings.NewReader(body))
		if user != "" {
			r = r.WithContext(authctx.WithUserID(r.Context(), user))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, endpoint := range []struct{ method, path string }{{"GET", "/test-models"}, {"POST", "/test"}, {"POST", "/restore-scheduling"}} {
		if w := call(endpoint.method, endpoint.path, `{}`, ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s status=%d", endpoint.path, w.Code)
		}
	}
	w := call("POST", "/test", `{"groupId":"a","modelId":"alias","protocol":"chat"}`, "user")
	var result RouteTestResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Success {
		t.Fatalf("HTTP test status=%d result=%+v %v", w.Code, result, err)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("test proof can be cached")
	}
	w = call("POST", "/restore-scheduling", fmt.Sprintf(`{"groupId":"a","token":%q}`, result.RecoveryToken), "user")
	if w.Code != 200 {
		t.Fatalf("HTTP recovery status=%d body=%s", w.Code, w.Body)
	}
}

func TestManualProbeModelRefreshUsesSelectedMemberDuringBackgroundRefresh(t *testing.T) {
	backgroundStarted := make(chan struct{})
	memberStarted := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	service, route, _ := manualProbeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected catalog path %s", r.URL.Path)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer sk-probe-test":
			close(backgroundStarted)
		case "Bearer sk-member-test":
			close(memberStarted)
		default:
			t.Error("wrong catalog credential")
		}
		<-finish
		_, _ = io.WriteString(w, `{"data":[{"id":"model","object":"model"}]}`)
	})
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	backgroundDone := make(chan error, 1)
	go func() { backgroundDone <- service.refreshRouteModelsWithRoute(context.Background(), route) }()
	select {
	case <-backgroundStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("background refresh did not start")
	}
	memberDone := make(chan error, 1)
	go func() {
		models, err := service.ListRouteTestModels(context.Background(), "user", route.ID, "a", true)
		if err == nil && (len(models) != 2 || models[0].ID != "alias" || models[1].ID != "model") {
			err = fmt.Errorf("member catalog=%+v", models)
		}
		memberDone <- err
	}()
	select {
	case <-memberStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("member catalog reused a background refresh with another key")
	}
	finishOnce.Do(func() { close(finish) })
	if err := <-backgroundDone; err != nil {
		t.Fatal(err)
	}
	if err := <-memberDone; err != nil {
		t.Fatal(err)
	}
}

func TestManualProbeCatalogIncludesAllNativeModelsAndConfiguredAliases(t *testing.T) {
	service, route, _ := manualProbeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"model"},{"id":"another-model"},{"id":"third-model"}]}`)
	})
	ctx := context.Background()
	mapping := map[string]string{
		"alias":             "model",
		"alias-copy-a":      "model",
		"alias-copy-b":      "model",
		"another-model":     "another-model",
		"unavailable-alias": "missing-model",
	}
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{ModelMapping: &mapping}); err != nil {
		t.Fatal(err)
	}
	models, err := service.ListRouteTestModels(ctx, "user", route.ID, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alias", "alias-copy-a", "alias-copy-b", "another-model", "model", "third-model", "unavailable-alias"}
	if len(models) != len(want) {
		t.Fatalf("catalog=%+v want %v", models, want)
	}
	for i := range want {
		if models[i].ID != want[i] {
			t.Fatalf("catalog=%+v want %v", models, want)
		}
	}
	// The test-only catalog must not broaden the member's scheduling policy.
	eligible, err := service.repository.ListGroupRoutes(ctx, "a", false, "third-model")
	if err != nil || len(eligible) != 0 {
		t.Fatalf("native test model changed scheduling: %+v %v", eligible, err)
	}
}
