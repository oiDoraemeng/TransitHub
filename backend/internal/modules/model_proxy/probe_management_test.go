package model_proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"transithub/backend/internal/modules/settings"
)

func TestDeleteRouteCleansPendingAndOrphanedProbes(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	service := &Service{repository: repo, limiter: limiter, accounts: proxyTestAccount{}}
	ctx := context.Background()
	for _, orphan := range []bool{false, true} {
		route := healthTestRoute(t, client)
		seedPauseMember(t, repo, route)
		spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
		_ = failTestModel(t, limiter, route, spec)
		id := modelHealthID(route, spec)
		if orphan {
			if err := repo.DeleteRoute(ctx, "user", "workspace", route.ID); err != nil {
				t.Fatal(err)
			}
			forceModelProbeDue(t, client, id)
			service.probeIsolatedModel(ctx, id)
		} else {
			if err := service.DeleteRoute(ctx, "other-user", route.ID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("unauthorized delete: %v", err)
			}
			if !client.HExists(ctx, modelHealthKey(id), "isolated").Val() {
				t.Fatal("unauthorized deletion removed health state")
			}
			if err := service.DeleteRoute(ctx, "user", route.ID); err != nil {
				t.Fatal(err)
			}
		}
		if deleted, err := repo.GetRoute(ctx, route.ID); err != nil || deleted != nil {
			t.Fatalf("route not deleted: %v %v", deleted, err)
		}
		if client.Exists(ctx, modelHealthKey(id)).Val() != 0 {
			t.Fatal("model task remains after route deletion")
		}
		if _, err := client.ZScore(ctx, modelProbeDueKey(), id).Result(); !errors.Is(err, redis.Nil) {
			t.Fatal("orphan probe remains queued")
		}
	}
}

func TestRecoveryProbeSettingsRemainWorkspaceScoped(t *testing.T) {
	repo := memberPauseTestRepository(t)
	ctx := context.Background()
	_, err := repo.db.Exec(ctx, `CREATE TABLE strategy_settings (user_id text, admin_account_id text, settings jsonb, updated_at timestamptz, PRIMARY KEY(user_id,admin_account_id))`)
	if err != nil {
		t.Fatal(err)
	}
	settingsRepo := settings.NewRepository(repo.db)
	service := settings.NewService(nil, settingsRepo)
	for _, row := range []struct {
		account           string
		first, subsequent int
	}{{"one", 90, 20}, {"two", 45, 10}} {
		if err := settingsRepo.SaveStrategy(ctx, "user", row.account, settings.StrategySettings{RecoveryProbeFirstTimeoutSeconds: row.first, RecoveryProbeSubsequentTimeoutSeconds: row.subsequent}); err != nil {
			t.Fatal(err)
		}
		first, subsequent, err := service.GetRecoveryProbeTimeouts(ctx, "user", row.account)
		if err != nil || first != row.first || subsequent != row.subsequent {
			t.Fatalf("workspace %s: %d %d %v", row.account, first, subsequent, err)
		}
	}
	first, subsequent, err := service.GetRecoveryProbeTimeouts(ctx, "other-user", "one")
	if err != nil || first != 60 || subsequent != 15 {
		t.Fatalf("cross-user settings leak: %d %d %v", first, subsequent, err)
	}
}

func TestRemoveModelStopsProbesAndFencesLateResults(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
	old := failTestModel(t, limiter, route, spec)
	id := modelHealthID(route, spec)
	forceModelProbeDue(t, client, id)
	claim, err := limiter.claimModelProbe(ctx, id)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if removed, err := limiter.removeRouteModel(ctx, route.ID, id); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, err := limiter.finishModelProbe(ctx, claim, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.ObserveModelResult(ctx, old, true, false, route, spec); err != nil {
		t.Fatal(err)
	}
	if claim, err := limiter.claimModelProbe(ctx, id); err != nil || claim != nil {
		t.Fatalf("removed probe reclaimed: %v %v", claim, err)
	}
	if _, err := client.ZScore(ctx, modelProbeDueKey(), id).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("removed model still queued: %v", err)
	}
	if _, _, err := limiter.AcquireForModel(ctx, route, "removed", spec); !errors.Is(err, errModelPaused) {
		t.Fatalf("removed model scheduled: %v", err)
	}
	lease, ok, err := limiter.AcquireForModel(ctx, route, "other-protocol", routeProbeSpec{ModelID: "bad", Path: "/v1/chat/completions"})
	if err != nil || !ok {
		t.Fatalf("other protocol blocked: %v %v", ok, err)
	}
	lease.Release(ctx)
	rows := []Route{route}
	if err := limiter.PopulateRouteRuntime(ctx, rows); err != nil || len(rows[0].ModelHealth) != 1 || !rows[0].ModelHealth[0].ProbeDisabled || rows[0].ModelHealth[0].NextProbeAt != nil {
		t.Fatalf("removed UI state: %+v %v", rows, err)
	}
}

func TestDeletedRouteRemovesAllProbesAndRejectsStaleTraffic(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ctx := context.Background()
	route := healthTestRoute(t, client)
	modelSpec := routeProbeSpec{ModelID: "one", Path: "/v1/responses"}
	oldModel := failTestModel(t, limiter, route, modelSpec)
	otherSpec := routeProbeSpec{ModelID: "two", Path: "/v1/chat/completions"}
	_ = failTestModel(t, limiter, route, otherSpec)
	oldRoute := acquireTestLease(t, limiter, route, "route-failure")
	failTestRoute(t, limiter, oldRoute)
	forceProbeDue(t, client, route.ID)
	routeClaim, _ := limiter.claimRouteProbe(ctx, route.ID)
	forceModelProbeDue(t, client, modelHealthID(route, modelSpec))
	modelClaim, _ := limiter.claimModelProbe(ctx, modelHealthID(route, modelSpec))
	if routeClaim == nil || modelClaim == nil {
		t.Fatal("pending probes not claimed")
	}
	if err := limiter.removeRouteRuntime(ctx, route.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = limiter.finishRouteProbe(ctx, routeClaim, false)
	_, _ = limiter.finishModelProbe(ctx, modelClaim, false, true)
	_, _ = limiter.ObserveRouteResult(ctx, oldRoute, false, routeProbeSpec{Path: "/v1/responses"})
	_, _ = limiter.ObserveModelResult(ctx, oldModel, false, true, route, modelSpec)
	for _, spec := range []routeProbeSpec{modelSpec, otherSpec} {
		id := modelHealthID(route, spec)
		if exists := client.Exists(ctx, modelHealthKey(id)).Val(); exists != 0 {
			t.Fatal("deleted model health recreated")
		}
		if _, err := client.ZScore(ctx, modelProbeDueKey(), id).Result(); !errors.Is(err, redis.Nil) {
			t.Fatal("deleted model probe recreated")
		}
	}
	if _, err := client.ZScore(ctx, probeDueKey(), route.ID).Result(); !errors.Is(err, redis.Nil) {
		t.Fatal("deleted route probe recreated")
	}
	if n := client.Exists(ctx, leaseKey(route.ID), routeModelIndexKey(route.ID)).Val(); n != 0 {
		t.Fatal("deleted route retains runtime data")
	}
	if _, _, err := limiter.Acquire(ctx, route, "stale-route"); !errors.Is(err, errRoutePaused) {
		t.Fatalf("stale route scheduled: %v", err)
	}
	if _, _, err := limiter.AcquireBest(ctx, []Route{route}, "stale-group", modelSpec); !errors.Is(err, errRoutePaused) {
		t.Fatalf("stale group scheduled: %v", err)
	}
	if _, err := limiter.acquireManualTest(ctx, route, "deleted-proof", modelSpec); err == nil {
		t.Fatal("deleted route manually tested")
	}
}

func TestRemovedModelRequiresFreshSuccessfulTestToRestore(t *testing.T) {
	service, route, client := manualProbeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"output":[{"type":"message"}]}`)
	})
	ctx := context.Background()
	spec := routeProbeSpec{ModelID: "model", Path: "/v1/responses"}
	id := modelHealthID(route, spec)
	if err := client.HSet(ctx, modelHealthKey(id), "isolated", 1, "epoch", 1, "model_id", "model", "protocol", spec.Path).Err(); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveRouteModel(ctx, "other-user", route.ID, RemoveRouteModelRequest{ModelID: "model", Protocol: spec.Path}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unauthorized model removal: %v", err)
	}
	if err := service.RemoveRouteModel(ctx, "user", route.ID, RemoveRouteModelRequest{ModelID: "model", Protocol: spec.Path}); err != nil {
		t.Fatal(err)
	}
	// Exclusion survives losing the Redis model state and catalog refreshes.
	if err := client.Del(ctx, modelHealthKey(id)).Err(); err != nil {
		t.Fatal(err)
	}
	// Remove the fixture's separate connection pause to test model exclusion.
	if err := client.HDel(ctx, healthKey(route.ID), "paused_until", "probe_token", "probe_spec").Err(); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.repository.GetRoute(ctx, route.ID)
	if err != nil || fresh == nil || len(fresh.RemovedModels) != 1 {
		t.Fatalf("removal not persisted: %+v %v", fresh, err)
	}
	if _, _, err := service.limiter.AcquireForModel(ctx, *fresh, "after-cache-reset", spec); !errors.Is(err, errModelPaused) {
		t.Fatalf("removed model scheduled after Redis reset: %v", err)
	}
	if _, _, err := service.limiter.AcquireBest(ctx, []Route{*fresh}, "group-after-cache-reset", spec); !errors.Is(err, errModelPaused) {
		t.Fatalf("group scheduled removed model after Redis reset: %v", err)
	}
	rows := []Route{*fresh}
	if err := service.limiter.PopulateRouteRuntime(ctx, rows); err != nil || len(rows[0].ModelHealth) != 1 || !rows[0].ModelHealth[0].ProbeDisabled {
		t.Fatalf("persisted removed UI state: %+v %v", rows, err)
	}
	result, err := service.TestRouteModel(ctx, "user", route.ID, RouteTestRequest{GroupID: "a", ModelID: "alias", Protocol: "responses"})
	if err != nil || !result.Success || result.RecoveryToken == "" {
		t.Fatalf("removed test: %+v %v", result, err)
	}
	fresh, err = service.repository.GetRoute(ctx, route.ID)
	if err != nil || len(fresh.RemovedModels) != 1 {
		t.Fatal("test restored automatically")
	}
	if err := service.RestoreRouteScheduling(ctx, "user", route.ID, RouteRecoveryRequest{GroupID: "a", Token: result.RecoveryToken}); err != nil {
		t.Fatal(err)
	}
	if client.HExists(ctx, modelHealthKey(id), "probe_disabled").Val() || client.HExists(ctx, modelHealthKey(id), "isolated").Val() {
		t.Fatal("manual recovery did not clear removal")
	}
	fresh, err = service.repository.GetRoute(ctx, route.ID)
	if err != nil || len(fresh.RemovedModels) != 0 {
		t.Fatal("manual recovery did not clear persistent removal")
	}
}

type testProbeSettings struct {
	first, subsequent int
	user, account     string
}

func (p *testProbeSettings) GetRecoveryProbeTimeouts(_ context.Context, user, account string) (int, int, error) {
	p.user, p.account = user, account
	return p.first, p.subsequent, nil
}

func TestAutomaticProbeUsesScopedFirstAndSubsequentDeadlines(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter = repo, limiter
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route)
	settings := &testProbeSettings{first: 60, subsequent: 15}
	service.SetRecoveryProbeSettings(settings)
	var deadline time.Duration
	service.dataClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		end, ok := r.Context().Deadline()
		if !ok {
			t.Error("probe has no total deadline")
		}
		deadline = time.Until(end)
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}, nil
	})
	spec := routeProbeSpec{ModelID: "bad", Path: "/v1/responses"}
	_ = failTestModel(t, limiter, route, spec)
	id := modelHealthID(route, spec)
	for _, want := range []time.Duration{60 * time.Second, 15 * time.Second, 15 * time.Second} {
		forceModelProbeDue(t, client, id)
		service.probeIsolatedModel(context.Background(), id)
		if deadline > want || deadline < want-time.Second {
			t.Fatalf("model deadline=%s want=%s", deadline, want)
		}
	}
	if settings.user != "user" || settings.account != "workspace" {
		t.Fatalf("wrong settings workspace: %+v", settings)
	}
	lease := acquireTestLease(t, limiter, route, "route-probe-failure")
	failTestRoute(t, limiter, lease)
	// The helper's group binding is intentionally absent; use a direct route.
	if err := client.HSet(context.Background(), healthKey(route.ID), "probe_spec", `{"modelId":"model","path":"/v1/responses"}`).Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{60 * time.Second, 15 * time.Second} {
		forceProbeDue(t, client, route.ID)
		service.probePausedRoute(context.Background(), route.ID)
		if deadline > want || deadline < want-time.Second {
			t.Fatalf("route deadline=%s want=%s", deadline, want)
		}
	}
}

func TestRecoveryProbeTimeoutCoversHeadersAndFullStream(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "stream"}[headers], func(t *testing.T) {
			stop := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if headers {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-stop:
				}
			}))
			defer server.Close()
			defer close(stop)
			service, route := preparedProbeService(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			started := time.Now()
			if service.performRouteProbe(ctx, route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"}, 150*time.Millisecond) {
				t.Fatal("hanging probe succeeded")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("timeout ignored: %s", elapsed)
			}
		})
	}
}
