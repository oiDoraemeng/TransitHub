package model_proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

func TestRouteRequestResultIgnoresClientErrorsAndCancellation(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 422, 429, 500, 503} {
		success, observe := routeRequestResult(context.Background(), &http.Response{StatusCode: status}, nil)
		if success != (status == 200) || observe != (status != 400 && status != 422) {
			t.Fatalf("status=%d success=%v observe=%v", status, success, observe)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, observe := routeRequestResult(ctx, nil, errors.New("transport canceled")); observe {
		t.Fatal("client cancellation counted as a route failure")
	}
	if success, observe := routeRequestResult(context.Background(), &http.Response{StatusCode: 200}, io.ErrUnexpectedEOF); success || !observe {
		t.Fatal("truncated upstream body was not a failure")
	}
}

func preparedProbeService(t *testing.T, server *httptest.Server) (*Service, Route) {
	t.Helper()
	cipher, err := NewKeyCipher(base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt("sk-probe-test")
	if err != nil {
		t.Fatal(err)
	}
	return &Service{cipher: cipher, dataClient: server.Client(), maxRequestBytes: 64 << 20,
			sites: proxyTestSites{site: upstream.Site{BaseURL: server.URL}}},
		Route{ID: "probe", SiteID: "site", Enabled: true, ConcurrencyLimit: 50, UseProvidedKey: true, UpstreamKeyCiphertext: ciphertext}
}

func TestRouteProbeUsesRealInferenceCredentialAndMapping(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer sk-probe-test" {
			t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "upstream-model" || payload["input"] != "Reply OK." || payload["stream"] != true || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("probe payload=%v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	route.ModelMappingEnabled, route.ModelMapping = true, map[string]string{"alias-model": "upstream-model"}
	if !service.performRouteProbe(context.Background(), route, routeProbeSpec{ModelID: "alias-model", Path: "/v1/responses"}) {
		t.Fatal("valid inference probe failed")
	}
	if calls.Load() != 1 {
		t.Fatalf("probe calls=%d want one", calls.Load())
	}
}

func TestRouteProbeRejectsErrorBodiesAndUnavailableUpstream(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"quota", 429, `{"error":{"message":"quota"}}`},
		{"hidden-error", 200, `{"error":{"message":"unavailable"}}`},
		{"invalid-json", 200, "<html>error</html>"},
		{"empty", 200, "{}"},
		{"trailing-error", 200, `{"id":"ok"} broken`},
		{"unrelated-json", 200, `{"message":"gateway unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			service, route := preparedProbeService(t, server)
			if service.performRouteProbe(context.Background(), route, routeProbeSpec{ModelID: "model", Path: "/v1/chat/completions"}) {
				t.Fatal("failed probe reported recovery")
			}
		})
	}
}

func TestNewRouteProbeRequestProtocols(t *testing.T) {
	for _, path := range []string{"/v1/models", "/v1beta/models", "/v1/embeddings", "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/responses/compact", "/v1beta/models/gemini-test:streamGenerateContent"} {
		request, body, err := newRouteProbeRequest(context.Background(), routeProbeSpec{ModelID: "model", Path: path})
		if err != nil {
			t.Fatalf("path=%s %v", path, err)
		}
		if request.Method == http.MethodPost && !json.Valid(body) {
			t.Fatalf("path=%s invalid JSON", path)
		}
		if strings.Contains(request.URL.Path, "streamGenerateContent") {
			t.Fatal("Gemini probe must request a finite response")
		}
	}
	if _, _, err := newRouteProbeRequest(context.Background(), routeProbeSpec{Path: "/v1/responses"}); err == nil {
		t.Fatal("missing probe model accepted")
	}
	if _, _, err := newRouteProbeRequest(context.Background(), routeProbeSpec{Path: "/unsupported"}); err == nil {
		t.Fatal("unsupported probe accepted")
	}
}

func TestProxyDirectRedisStopsRequestsDuringPause(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.limiter = limiter
	route.ID = healthTestRoute(t, client).ID
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		service.proxyDirect(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"sensitive user content"}`)), route)
		return w
	}
	for index := 1; index <= routeFailureThreshold; index++ {
		if w := request(); w.Code != http.StatusBadGateway {
			t.Fatalf("request %d status=%d", index, w.Code)
		}
		if calls.Load() != int64(index*directMaxAttempts) {
			t.Fatalf("request %d upstream calls=%d", index, calls.Load())
		}
		if failures, err := client.HGet(context.Background(), modelHealthKey(modelHealthID(route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"})), "failures").Int(); err != nil || failures != index {
			t.Fatalf("request %d failures=%d err=%v", index, failures, err)
		}
	}
	if w := request(); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "model_temporarily_paused") {
		t.Fatalf("paused status=%d headers=%v", w.Code, w.Header())
	}
	if calls.Load() != routeFailureThreshold*directMaxAttempts {
		t.Fatal("paused route received real traffic")
	}
	state, err := client.HGetAll(context.Background(), modelHealthKey(modelHealthID(route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"}))).Result()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state["probe_spec"], "sensitive") {
		t.Fatal("recovery probe retained user content")
	}
}

func TestSmartGroupStopsRetryWhenRouteBecomesPaused(t *testing.T) {
	calls := 0
	_, err := attemptSmartGroupMember(context.Background(), func(_ time.Duration) (*http.Response, error) { calls++; return nil, errRoutePaused })
	if calls != 1 || !errors.Is(err, errRoutePaused) {
		t.Fatalf("attempts=%d err=%v", calls, err)
	}
}
