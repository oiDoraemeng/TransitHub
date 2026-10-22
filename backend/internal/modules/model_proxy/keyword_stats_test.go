package model_proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeywordStatsCountRequestOnceAcrossProtocols(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	group := SmartGroup{ID: fmt.Sprintf("keyword-stats-%d", time.Now().UnixNano()), Keywords: []string{"Premium", "认真", "unused"}, LocalReplyEnabled: true, LocalReplyMinTokens: 100, LocalReplyMaxOutputTokens: 20}
	key, _ := keywordMatchCountKey(group.ID, time.Now())
	t.Cleanup(func() { client.Del(context.Background(), key) })
	service := &Service{limiter: limiter, maxRequestBytes: 1 << 20}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", compactPath, "/v1/messages", "/v1beta/models/alias:generateContent"} {
		body := `{"model":"alias","input":"PREMIUM premium 认真认真","max_tokens":20,"max_output_tokens":20,"generationConfig":{"maxOutputTokens":20}}`
		w := httptest.NewRecorder()
		service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), group)
		if w.Code != 200 {
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
	}
	groups := []SmartGroup{group, {ID: "other-group", Keywords: group.Keywords}}
	if err := limiter.PopulateKeywordMatchCounts(context.Background(), groups); err != nil {
		t.Fatal(err)
	}
	if groups[0].KeywordMatchCounts["Premium"] != 5 || groups[0].KeywordMatchCounts["认真"] != 5 || groups[0].KeywordMatchCounts["unused"] != 0 || groups[1].KeywordMatchCounts["Premium"] != 0 {
		t.Fatalf("counts=%v other=%v", groups[0].KeywordMatchCounts, groups[1].KeywordMatchCounts)
	}
	if ttl := client.TTL(context.Background(), key).Val(); ttl <= 0 || ttl > 24*time.Hour {
		t.Fatalf("unexpected TTL: %s", ttl)
	}
}

func TestKeywordStatsRetriesCountOneLogicalRequest(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"retry"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"upstream","object":"response","model":"model","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	service, route := preparedProbeService(t, upstream)
	route.ID = healthTestRoute(t, client).ID
	groupID := fmt.Sprintf("keyword-retry-%d", time.Now().UnixNano())
	seedPauseMember(t, repo, route, groupID)
	service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
	service.statistics = newRequestStatistics(limiter)
	enabled := true
	if err := service.UpdateMemberPolicy(context.Background(), "user", groupID, route.ID, UpdateMemberPolicyRequest{UseProvidedKey: &enabled}); err != nil {
		t.Fatal(err)
	}
	group, _ := repo.GetSmartGroupConfig(context.Background(), groupID)
	group.Keywords = []string{"premium"}
	keywordKey, _ := keywordMatchCountKey(groupID, time.Now())
	routeKey, _ := routeRequestCountKey(time.Now())
	t.Cleanup(func() {
		client.Del(context.Background(), keywordKey)
		client.HDel(context.Background(), routeKey, route.ID)
	})
	w := httptest.NewRecorder()
	service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"premium premium"}`)), *group)
	service.statistics.flush(context.Background())
	if w.Code != 200 || attempts.Load() != 2 || client.HGet(context.Background(), keywordKey, "premium").Val() != "1" || client.HGet(context.Background(), routeKey, route.ID).Val() != "1" {
		t.Fatalf("status=%d attempts=%d keyword=%s route=%s body=%s", w.Code, attempts.Load(), client.HGet(context.Background(), keywordKey, "premium").Val(), client.HGet(context.Background(), routeKey, route.ID).Val(), w.Body.String())
	}
}

func TestDailyStatisticsBatchConcurrentUpdates(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	stats := newRequestStatistics(limiter)
	now := time.Now()
	key, expiresAt := keywordMatchCountKey(fmt.Sprintf("keyword-batch-%d", now.UnixNano()), now)
	t.Cleanup(func() { client.Del(context.Background(), key) })
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				stats.add(key, expiresAt, []string{"keyword"})
			}
		}()
	}
	wg.Wait()
	if client.Exists(context.Background(), key).Val() != 0 {
		t.Fatal("request counter performed a synchronous write")
	}
	stats.flush(context.Background())
	if count := client.HGet(context.Background(), key, "keyword").Val(); count != "1000" {
		t.Fatalf("lost concurrent counts: %s", count)
	}
	stats.flush(context.Background())
	if count := client.HGet(context.Background(), key, "keyword").Val(); count != "1000" {
		t.Fatal("flush duplicated counts")
	}
	before := time.Date(2026, 10, 22, 23, 59, 59, 0, shanghaiLocation)
	a, expiry := keywordMatchCountKey("group", before)
	b, _ := keywordMatchCountKey("group", before.Add(time.Second))
	if a == b || expiry != before.Add(time.Second).Unix() {
		t.Fatal("counter did not rotate at Shanghai midnight")
	}
	stats.add(key+"expired", now.Add(-time.Second).Unix(), []string{"keyword"})
	stats.flush(context.Background())
	if client.Exists(context.Background(), key+"expired").Val() != 0 {
		t.Fatal("expired counter resurrected")
	}
}
