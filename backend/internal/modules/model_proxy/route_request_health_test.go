package model_proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSmartGroupRedisCountsRequestsAfterOneRetry(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter = repo, limiter
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "group")
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello"}`)), SmartGroup{ID: "group"})
		return w
	}
	for index := 1; index <= routeFailureThreshold; index++ {
		if w := request(); w.Code != http.StatusBadGateway {
			t.Fatalf("request %d status=%d body=%s", index, w.Code, w.Body.String())
		}
		if calls.Load() != int64(index*smartGroupAttempts) {
			t.Fatalf("request %d upstream calls=%d", index, calls.Load())
		}
		if failures, err := client.HGet(context.Background(), modelHealthKey(modelHealthID(route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"})), "failures").Int(); err != nil || failures != index {
			t.Fatalf("request %d failures=%d err=%v", index, failures, err)
		}
	}
	if w := request(); w.Code != http.StatusServiceUnavailable || calls.Load() != routeFailureThreshold*smartGroupAttempts {
		t.Fatalf("paused request status=%d upstream calls=%d", w.Code, calls.Load())
	}
}

func TestProxyRequestRedisSuccessfulRetryClearsFailures(t *testing.T) {
	for _, smart := range []bool{false, true} {
		name := "direct"
		if smart {
			name = "smart-group"
		}
		t.Run(name, func(t *testing.T) {
			limiter, client := redisTestLimiter(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = io.WriteString(w, `{"output":[]}`)
			}))
			defer server.Close()
			service, route := preparedProbeService(t, server)
			service.limiter = limiter
			route.ID = healthTestRoute(t, client).ID
			lease := acquireTestLease(t, limiter, route, "previous-requests")
			for index := 0; index < routeFailureThreshold-1; index++ {
				if paused, err := limiter.ObserveRouteResult(context.Background(), lease, false, routeProbeSpec{ModelID: "model", Path: "/v1/responses"}); err != nil || paused {
					t.Fatalf("prepare failures: %v %v", paused, err)
				}
			}
			w := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hello"}`))
			if smart {
				repo := memberPauseTestRepository(t)
				service.repository = repo
				seedPauseMember(t, repo, route, "group")
				service.proxySmartGroup(w, request, SmartGroup{ID: "group"})
			} else {
				service.proxyDirect(w, request, route)
			}
			if w.Code != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("successful retry status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			if failures, err := client.HGet(context.Background(), healthKey(route.ID), "failures").Int(); err != nil || failures != 0 {
				t.Fatalf("successful retry failures=%d err=%v", failures, err)
			}
			if exists, err := client.HExists(context.Background(), healthKey(route.ID), "paused_until").Result(); err != nil || exists {
				t.Fatalf("successful retry paused route: %v %v", exists, err)
			}
		})
	}
}
