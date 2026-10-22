package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func memberPauseTestRepository(t testing.TB) *Repository {
	t.Helper()
	url := os.Getenv("MODEL_PROXY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MODEL_PROXY_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := randomID("model_proxy_test_")
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE upstream_sites (id text PRIMARY KEY, name text NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../database/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if filepath.Base(file) < "000019" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s: %v", file, err)
		}
	}
	return NewRepository(pool)
}

func seedPauseMember(t testing.TB, repo *Repository, route Route, groups ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := repo.db.Exec(ctx, `INSERT INTO proxy_routes (id,user_id,admin_account_id,name,site_id,group_id,group_name,upstream_key_ciphertext,upstream_key_preview)
VALUES ($1,'user','workspace','test route','site','1','test group',$2,'sk-test')`, route.ID, route.UpstreamKeyCiphertext); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if _, err := repo.db.Exec(ctx, `INSERT INTO proxy_smart_groups (id,user_id,admin_account_id,name) VALUES ($1,'user','workspace',$1)`, group); err != nil {
			t.Fatal(err)
		}
		if err := repo.AddMember(ctx, "user", "workspace", group, route.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceRouteModels(ctx, route.ID, []Model{{ID: "model", Object: "model"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMemberPausePostgresPersistsAndIsolatesGroups(t *testing.T) {
	repo := memberPauseTestRepository(t)
	route := Route{ID: "member-route"}
	seedPauseMember(t, repo, route, "a", "b")
	service := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	paused := true
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	// A fresh repository observes the persisted setting without any local cache.
	reloaded := NewRepository(repo.db)
	members, err := reloaded.ListGroupRoutes(ctx, "a", false, "")
	if err != nil || len(members) != 1 || !members[0].SchedulingPaused {
		t.Fatalf("admin list=%+v %v", members, err)
	}
	eligible, err := reloaded.ListGroupRoutes(ctx, "a", true, "model")
	if err != nil || len(eligible) != 0 {
		t.Fatalf("paused member still eligible: %+v %v", eligible, err)
	}
	other, err := reloaded.ListGroupRoutes(ctx, "b", true, "model")
	if err != nil || len(other) != 1 || other[0].SchedulingPaused {
		t.Fatalf("other group affected: %+v %v", other, err)
	}
	direct, err := reloaded.GetRoute(ctx, route.ID)
	if err != nil || direct == nil || !direct.Enabled || direct.SchedulingPaused {
		t.Fatalf("direct route affected: %+v %v", direct, err)
	}
	models, err := reloaded.GroupModels(ctx, "a")
	if err != nil || len(models) != 0 {
		t.Fatalf("paused member contributes models: %+v %v", models, err)
	}
	group, err := reloaded.GetSmartGroup(ctx, "a")
	if err != nil || group.TotalConcurrency != 0 {
		t.Fatalf("paused group capacity=%+v %v", group, err)
	}
	if err := service.UpdateMemberPolicy(ctx, "other-user", "a", route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("workspace authorization bypass: %v", err)
	}
	paused = false
	if err := service.UpdateMemberPolicy(ctx, "user", "a", route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	eligible, err = reloaded.ListGroupRoutes(ctx, "a", true, "model")
	if err != nil || len(eligible) != 1 || eligible[0].SchedulingPaused {
		t.Fatalf("resume didn't restore member: %+v %v", eligible, err)
	}
	group, err = reloaded.GetSmartGroup(ctx, "a")
	if err != nil || group.TotalConcurrency != 50 {
		t.Fatalf("resumed capacity=%+v %v", group, err)
	}
}

func TestMemberPausePostgresRedisStopsTrafficAndRecoveryProbes(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, redisClient := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") == "text/event-stream" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["stream"] != true {
				t.Errorf("automatic recovery didn't request streaming: %v %v", payload, err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","output":[]}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
	route.ID = healthTestRoute(t, redisClient).ID
	groupID := "probe-group"
	seedPauseMember(t, repo, route, groupID)
	lease := acquireTestLease(t, limiter, route, "before-pause")
	for index := 0; index < 3; index++ {
		if _, err := limiter.ObserveRouteResult(context.Background(), lease, false, routeProbeSpec{GroupID: groupID, ModelID: "model", Path: "/v1/responses"}); err != nil {
			t.Fatal(err)
		}
	}
	paused := true
	if err := service.UpdateMemberPolicy(context.Background(), "user", groupID, route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	forceProbeDue(t, redisClient, route.ID)
	service.probePausedRoute(context.Background(), route.ID)
	if calls.Load() != 0 {
		t.Fatal("manually paused member received recovery probe")
	}
	group, err := repo.GetSmartGroup(context.Background(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello"}`)), *group)
	if calls.Load() != 0 {
		t.Fatal("manually paused member received live traffic")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("no eligible member status=%d", w.Code)
	}
	paused = false
	if err := service.UpdateMemberPolicy(context.Background(), "user", groupID, route.ID, UpdateMemberPolicyRequest{SchedulingPaused: &paused}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := limiter.AcquireBest(context.Background(), []Route{route}, "auto-still-paused"); !errors.Is(err, errRoutePaused) {
		t.Fatalf("manual resume bypassed automatic pause: %v", err)
	}
	forceProbeDue(t, redisClient, route.ID)
	service.probePausedRoute(context.Background(), route.ID)
	if calls.Load() != 1 {
		t.Fatalf("probe calls=%d want 1", calls.Load())
	}
	group, err = repo.GetSmartGroup(context.Background(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello"}`)), *group)
	if w.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("resumed traffic status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if active, err := limiter.Active(context.Background(), route.ID); err != nil || active != 1 {
		t.Fatalf("leaked lease: %d %v", active, err)
	}
}

func TestAtomicSchedulerRedisRejectsManuallyPausedMember(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	route := healthTestRoute(t, client)
	route.SchedulingPaused = true
	if _, _, err := limiter.AcquireBest(context.Background(), []Route{route}, fmt.Sprintf("manual-%d", time.Now().UnixNano())); !errors.Is(err, errRoutePaused) {
		t.Fatalf("manual pause bypassed scheduler: %v", err)
	}
}
