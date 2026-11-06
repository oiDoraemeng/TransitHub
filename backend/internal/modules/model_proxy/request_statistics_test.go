package model_proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type disconnectedStatisticsWriter struct{ *httptest.ResponseRecorder }

func (w disconnectedStatisticsWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestProxyRequestStatisticsOnlyCountSuccessfulResponses(t *testing.T) {
	const delta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	for _, smart := range []bool{false, true} {
		name := "direct"
		if smart {
			name = "smart-group"
		}
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				name       string
				status     int
				body       string
				stream     bool
				retry      bool
				truncated  bool
				disconnect bool
				wantCount  int64
			}{
				{name: "success", status: 200, body: `{"output":[]}`, wantCount: 1},
				{name: "successful-retry", status: 200, body: `{"output":[]}`, retry: true, wantCount: 1},
				{name: "failed-retries", status: 503, body: `{"error":"unavailable"}`},
				{name: "invalid-request", status: 400, body: `{"error":"invalid request"}`},
				{name: "http-200-json-error", status: 200, body: `{"error":{"code":"overloaded"}}`},
				{name: "stream-success", status: 200, body: delta + "data: [DONE]\n\n", stream: true, wantCount: 1},
				{name: "stream-error-after-output", status: 200, body: delta + "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"overloaded\"}}}\n\n", stream: true},
				{name: "truncated-stream", status: 200, body: delta, stream: true, truncated: true},
				{name: "downstream-disconnect", status: 200, body: `{"output":[]}`, disconnect: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					limiter, client := redisTestLimiter(t)
					var calls atomic.Int64
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						if calls.Add(1) == 1 && test.retry {
							w.WriteHeader(503)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if test.stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						if test.truncated {
							w.Header().Set("Content-Length", "4096")
						}
						w.WriteHeader(test.status)
						_, _ = io.WriteString(w, test.body)
					}))
					defer server.Close()
					service, route := preparedProbeService(t, server)
					service.limiter = limiter
					route.ID = healthTestRoute(t, client).ID
					group := SmartGroup{ID: "statistics", Keywords: []string{"premium"}}
					if smart {
						service.repository = memberPauseTestRepository(t)
						seedPauseMember(t, service.repository, route, group.ID)
						service.statistics = newRequestStatistics(limiter)
					}
					ctx := context.Background()
					routeKey, _ := routeRequestCountKey(time.Now())
					keywordKey, _ := keywordMatchCountKey(group.ID, time.Now())
					t.Cleanup(func() { client.HDel(ctx, routeKey, route.ID); client.Del(ctx, keywordKey) })
					recorder := httptest.NewRecorder()
					var writer http.ResponseWriter = recorder
					if test.disconnect {
						writer = disconnectedStatisticsWriter{recorder}
					}
					request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"premium premium","stream":true}`))
					if smart {
						service.proxySmartGroup(writer, request, group)
						service.statistics.flush(ctx)
					} else {
						service.proxyDirect(writer, request, route)
					}
					if test.wantCount > 0 && recorder.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					if test.retry && calls.Load() != 2 {
						t.Fatalf("retry attempts=%d want 2", calls.Load())
					}
					if count := client.HGet(ctx, routeKey, route.ID).Val(); (test.wantCount == 0 && count != "") || (test.wantCount == 1 && count != "1") {
						t.Fatalf("route count=%q want %d", count, test.wantCount)
					}
					if smart {
						if count := client.HGet(ctx, keywordKey, "premium").Val(); (test.wantCount == 0 && count != "") || (test.wantCount == 1 && count != "1") {
							t.Fatalf("keyword count=%q want %d", count, test.wantCount)
						}
					}
				})
			}
		})
	}
}

func TestSmartGroupRequestStatisticsCountOnlySuccessfulFallbackRoute(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer sk-probe-test" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()
	service, bad := preparedProbeService(t, server)
	service.repository, service.limiter = repo, limiter
	service.statistics = newRequestStatistics(limiter)
	bad.ID = healthTestRoute(t, client).ID
	group := SmartGroup{ID: "fallback-statistics", Keywords: []string{"premium"}}
	seedPauseMember(t, repo, bad, group.ID)
	ctx := context.Background()
	goodID := healthTestRoute(t, client).ID
	goodKey, err := service.cipher.Encrypt("sk-good")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(ctx, `INSERT INTO proxy_routes(id,user_id,admin_account_id,name,site_id,group_id,group_name,upstream_key_ciphertext) VALUES($1,'user','workspace','fallback','site','1','group',$2)`, goodID, goodKey); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddMember(ctx, "user", "workspace", group.ID, goodID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceRouteModels(ctx, goodID, []Model{{ID: "model", Object: "model"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(ctx, `UPDATE proxy_smart_group_members SET priority=10 WHERE route_id=$1`, bad.ID); err != nil {
		t.Fatal(err)
	}
	routeKey, _ := routeRequestCountKey(time.Now())
	keywordKey, _ := keywordMatchCountKey(group.ID, time.Now())
	t.Cleanup(func() { client.HDel(ctx, routeKey, bad.ID, goodID); client.Del(ctx, keywordKey) })
	w := httptest.NewRecorder()
	service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"premium"}`)), group)
	service.statistics.flush(ctx)
	if w.Code != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	for key, fields := range map[string]map[string]string{routeKey: {bad.ID: "", goodID: "1"}, keywordKey: {"premium": "1"}} {
		for field, want := range fields {
			if got := client.HGet(ctx, key, field).Val(); got != want {
				t.Fatal(fmt.Sprintf("counter %s/%s=%q want %q", key, field, got, want))
			}
		}
	}
}
