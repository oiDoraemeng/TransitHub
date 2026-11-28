package model_proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func portableFallbackRoute() Route {
	return Route{ID: "mapping", ModelMappingEnabled: true, ModelMapping: map[string]string{"model": "gpt-6-luna"}, RequestMatchMode: RequestMatchModeMatchOrFallback, RequestMatchCapabilities: []string{"pdf", "code_execution", "json_schema", "web_search"}, SimulateThinking: true, Priority: 1}
}

func TestCapabilityFallbackEligibilityKeywordsAndNativeState(t *testing.T) {
	route := portableFallbackRoute()
	for _, tc := range []struct {
		name          string
		facts         requestCapabilities
		keyword, want bool
	}{
		{"plain backup", requestCapabilities{}, false, true},
		{"keyword plain excluded", requestCapabilities{}, true, false},
		{"PDF", requestCapabilities{PDF: true}, false, true},
		{"code", requestCapabilities{CodeExecution: true}, false, true},
		{"mixed portable", requestCapabilities{PDF: true, CodeExecution: true, JSONSchema: true, WebSearch: true}, false, true},
		{"capability primary", requestCapabilities{JSONSchema: true}, true, true},
		{"simulated thinking", requestCapabilities{Thinking: true, NativeClaude: true}, false, true},
		{"opaque state", requestCapabilities{NativeState: true, NativeClaude: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "ordinary"
			if tc.keyword {
				input = "reserved"
			}
			got := filterRoutesByRequest(tc.facts, []Route{route}, "model")
			got = filterRoutesByKeywordsAndRequest(input, []string{"reserved"}, got, tc.facts, "model")
			if (len(got) == 1) != tc.want {
				t.Fatalf("got=%v", got)
			}
		})
	}
	route.RequestMatchCapabilities = []string{"json_schema"}
	if got := filterRoutesByRequest(requestCapabilities{JSONSchema: true, CodeExecution: true}, []Route{route}, "model"); len(got) != 0 {
		t.Fatal("backup bypassed mixed capability coverage")
	}
	route.ModelMappingEnabled = false
	if requestPolicyAllows(route, requestCapabilities{}, "model") {
		t.Fatal("native member accepted mapped backup policy")
	}
}

func TestCapabilityFallbackSchedulerOrderAndKeywordBranch(t *testing.T) {
	limiter, client := redisTestLimiter(t)
	ordinary := healthTestRoute(t, client)
	ordinary.Priority = 2
	mapped := healthTestRoute(t, client)
	policy := portableFallbackRoute()
	mapped.Priority, mapped.ModelMappingEnabled, mapped.ModelMapping, mapped.RequestMatchMode, mapped.RequestMatchCapabilities = 1, true, policy.ModelMapping, policy.RequestMatchMode, policy.RequestMatchCapabilities
	enterprise := healthTestRoute(t, client)
	enterprise.Priority = 0
	routes := []Route{ordinary, mapped, enterprise}
	cleanupRedisTestRoutes(t, client, routes)
	spec := routeProbeSpec{ModelID: "model", Path: "/v1/messages"}
	for _, tc := range []struct {
		name            string
		routes          []Route
		keyword, failed bool
		want            string
	}{
		{"ordinary first", routes, false, false, ordinary.ID},
		{"mapping after ordinary failure", routes[1:], false, true, mapped.ID},
		{"enterprise after mapping failure", routes[2:], false, true, enterprise.ID},
		{"keyword skips mapping even on capability match", routes[1:], true, true, enterprise.ID},
		{"mapping before lower priority native when ordinary absent", routes[1:], false, false, mapped.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease, route, _, err := limiter.acquireWithMappedFallback(context.Background(), tc.routes, "fallback-order", spec, requestCapabilities{JSONSchema: tc.keyword}, tc.keyword, tc.failed)
			if err != nil || route == nil || route.ID != tc.want {
				t.Fatalf("route=%v err=%v", route, err)
			}
			lease.Release(context.Background())
		})
	}
	// A failed ordinary member must yield to mapping even if the remaining
	// enterprise member was configured with a higher priority than the mapping.
	enterprise.Priority = 9
	lease, route, _, err := limiter.acquireWithMappedFallback(context.Background(), []Route{enterprise, mapped}, "fallback-force", spec, requestCapabilities{}, false, true)
	if err != nil || route == nil || route.ID != mapped.ID {
		t.Fatalf("mapping fallback was displaced: %v %v", route, err)
	}
	lease.Release(context.Background())
	legacy := mapped
	legacy.RequestMatchMode = KeywordMatchModeRequireMatch
	lease, route, _, err = limiter.acquireWithMappedFallback(context.Background(), []Route{legacy, enterprise}, "legacy-priority", spec, requestCapabilities{JSONSchema: true}, false, true)
	if err != nil || route == nil || route.ID != enterprise.ID {
		t.Fatalf("unconfigured groups lost priority behavior: %v %v", route, err)
	}
	lease.Release(context.Background())
	// A manually paused or full mapping still yields to the real native backup.
	mapped.SchedulingPaused = true
	lease, route, _, err = limiter.acquireWithMappedFallback(context.Background(), []Route{mapped, enterprise}, "paused-fallback", spec, requestCapabilities{}, false, true)
	if err != nil || route == nil || route.ID != enterprise.ID {
		t.Fatalf("paused backup blocked native: %v %v", route, err)
	}
	lease.Release(context.Background())
}

func TestCapabilityFallbackPolicyPersistsAndRequiresMapping(t *testing.T) {
	repo := memberPauseTestRepository(t)
	seedPauseMember(t, repo, Route{ID: "fallback-policy"}, "a", "b")
	svc := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	mode, caps := RequestMatchModeMatchOrFallback, []string{"pdf", "code_execution", "json_schema", "web_search"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "fallback-policy", UpdateMemberPolicyRequest{RequestMatchMode: &mode, RequestMatchCapabilities: &caps}); err == nil {
		t.Fatal("fallback enabled without mapping")
	}
	enabled, disabled := true, false
	mapping := map[string]string{"model": "gpt-6-luna"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "fallback-policy", UpdateMemberPolicyRequest{RequestMatchMode: &mode, RequestMatchCapabilities: &caps, ModelMappingEnabled: &enabled, ModelMapping: &mapping}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "fallback-policy", UpdateMemberPolicyRequest{ModelMappingEnabled: &disabled}); err == nil {
		t.Fatal("partial update left active fallback without mapping")
	}
	for _, group := range []string{"a", "b"} {
		members, err := repo.ListGroupRoutes(ctx, group, false, "")
		if err != nil || len(members) != 1 {
			t.Fatalf("members=%v err=%v", members, err)
		}
		if group == "a" && (members[0].RequestMatchMode != mode || !reflect.DeepEqual(members[0].RequestMatchCapabilities, caps)) {
			t.Fatal("fallback policy did not persist")
		}
		if group == "b" && members[0].RequestMatchMode == mode {
			t.Fatal("policy leaked to another group")
		}
	}
}

func TestTerminalRequestErrorsStopSmartGroupRetries(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		status           int
		terminal         bool
	}{
		{"blocked", `{"error":{"message":"This conversation has been permanently blocked by the gateway."}}`, "conversation_blocked", 403, true},
		{"parameter conflict", `{"error":{"message":"temperature and top_p cannot both be specified"}}`, "invalid_request_parameters", 400, true},
		{"larger context may help", `{"error":{"code":"context_length_exceeded","message":"Context window is full."}}`, "context_length_exceeded", 400, false},
		{"capability error may use another provider", `{"error":{"type":"invalid_request_error","message":"Unsupported server tool"}}`, "", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			trackResponseHealth(response)
			if responseHealth(response).requestErrorKind != tc.kind {
				t.Fatalf("classification=%s", responseHealth(response).requestErrorKind)
			}
			calls := 0
			_, err := attemptSmartGroupMemberWithPolicy(context.Background(), smartGroupRetryPolicy{deadline: time.Now().Add(time.Second), hasAlternatives: true}, func(time.Duration) (*http.Response, error) { calls++; return response, nil })
			var terminal *terminalUpstreamRequestError
			if errors.As(err, &terminal) != tc.terminal || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
	// An error marker inside normal successful answer text is never classified.
	b := &healthResponseBody{}
	b.inspectJSON([]byte(`{"content":[{"type":"text","text":"conversation_blocked invalid_json"}]}`))
	if b.terminalRequestError() != nil || b.failed {
		t.Fatal("normal text was classified as a terminal error")
	}
	// Preserve the error classification before streaming starts as well.
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"conversation has been blocked\"}}\n\n"))}
	trackResponseHealth(response)
	if err := prepareStreamStart(context.Background(), response, time.Second); err != nil {
		t.Fatal(err)
	}
	if responseHealth(response).terminalRequestError() == nil {
		t.Fatal("streaming block lost classification")
	}
	response.Body.Close()
}

func TestSelectionLogsContainReasonsAndKeywordIndexesOnly(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(previous)
	logMemberSelection("req_test", "group", "model", "/v1/messages", requestCapabilities{PDF: true}, []int{2}, []memberSelectionDecision{{"ordinary", "keyword_matched"}, {"mapping", "eligible_mapped_fallback"}})
	line := buffer.String()
	index := strings.Index(line, "selection ")
	if index < 0 {
		t.Fatal("selection log absent")
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line[index+len("selection "):])), &event); err != nil {
		t.Fatal(err)
	}
	if event["request_id"] != "req_test" || len(event["keyword_indexes"].([]any)) != 1 || len(event["members"].([]any)) != 2 {
		t.Fatal("selection metadata lost")
	}
	for _, field := range []string{"prompt", "content", "signature", "authorization", "upstream_key"} {
		if _, ok := event[field]; ok {
			t.Fatalf("sensitive log field %s", field)
		}
	}
}

func TestCapabilityFallbackSmartGroupRoutesWithoutEnterpriseUntilNeeded(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var mu sync.Mutex
	var calls []string
	ordinaryError, mappedFailure := "context", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer sk-")
		mu.Lock()
		calls = append(calls, who)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if who == "ordinary" && ordinaryError != "" {
			if ordinaryError == "transient" {
				w.WriteHeader(503)
			} else {
				w.WriteHeader(400)
			}
			switch ordinaryError {
			case "transient":
				io.WriteString(w, `{"error":{"message":"Service temporarily unavailable"}}`)
			case "blocked":
				io.WriteString(w, `{"error":{"message":"This conversation has been permanently blocked by the gateway."}}`)
			case "invalid":
				io.WriteString(w, `{"error":{"message":"temperature and top_p cannot both be specified"}}`)
			default:
				io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"Context window is full."}}`)
			}
			return
		}
		if who == "mapped" {
			if mappedFailure {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"Provider does not support this request"}}`)
				return
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "gpt-6-luna" {
				t.Errorf("mapping did not use target model: %v", payload["model"])
			}
			if r.URL.Path == "/v1/responses" {
				tools, _ := payload["tools"].([]any)
				for _, entry := range tools {
					tool, _ := entry.(map[string]any)
					if firstString(tool, "type") == "code_interpreter" {
						io.WriteString(w, `{"id":"executed","status":"completed","output":[{"type":"code_interpreter_call","id":"ci_real","status":"completed","code":"print(7*9)","outputs":[{"type":"logs","logs":"63\n"}]},{"type":"message","content":[{"type":"output_text","text":"63"}]}]}`)
						return
					}
				}
				io.WriteString(w, strings.Replace(offloadTextResponse, "Blue.", `{\"answer\":323}`, 1))
				return
			}
		}
		if r.URL.Path == "/v1/chat/completions" {
			io.WriteString(w, `{"id":"chat","object":"chat.completion","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		} else {
			io.WriteString(w, `{"id":"native","type":"message","model":"model","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
		}
	}))
	defer server.Close()
	svc, base := preparedProbeService(t, server)
	svc.repository, svc.limiter, svc.accounts = repo, limiter, proxyTestAccount{}
	ctx := context.Background()
	routes := make([]Route, 3)
	for i, name := range []string{"ordinary", "mapped", "enterprise"} {
		route := base
		route.ID = healthTestRoute(t, client).ID
		route.UpstreamKeyCiphertext, _ = svc.cipher.Encrypt("sk-" + name)
		routes[i] = route
		if i == 0 {
			seedPauseMember(t, repo, route, "fallback-group")
		} else {
			seedPauseMember(t, repo, route)
			if err := repo.AddMember(ctx, "user", "workspace", "fallback-group", route.ID); err != nil {
				t.Fatal(err)
			}
		}
		enabled, priority := true, 2-i
		if err := svc.UpdateMemberPolicy(ctx, "user", "fallback-group", route.ID, UpdateMemberPolicyRequest{UseProvidedKey: &enabled, Priority: &priority, NeverAutoPause: &enabled}); err != nil {
			t.Fatal(err)
		}
	}
	cleanupRedisTestRoutes(t, client, routes)
	if err := repo.ReplaceRouteModels(ctx, routes[1].ID, []Model{{ID: "gpt-6-luna", Object: "model"}}); err != nil {
		t.Fatal(err)
	}
	policy := portableFallbackRoute()
	enabled := true
	if err := svc.UpdateMemberPolicy(ctx, "user", "fallback-group", routes[1].ID, UpdateMemberPolicyRequest{ModelMappingEnabled: &enabled, ModelMapping: &policy.ModelMapping, RequestMatchMode: &policy.RequestMatchMode, RequestMatchCapabilities: &policy.RequestMatchCapabilities}); err != nil {
		t.Fatal(err)
	}
	group, err := repo.GetSmartGroup(ctx, "fallback-group")
	if err != nil {
		t.Fatal(err)
	}
	group.Keywords = []string{"reserved"}
	for _, tc := range []struct {
		name, path, body, ordinaryError string
		mappedFailure                   bool
		want                            []string
		status                          int
	}{
		{"Messages no keyword maps first", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "context", false, []string{"ordinary", "mapped"}, 200},
		{"Chat no keyword maps first", "/v1/chat/completions", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "context", false, []string{"ordinary", "mapped"}, 200},
		{"retryable failure maps before enterprise", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "transient", false, []string{"ordinary", "ordinary", "mapped"}, 200},
		{"keyword plain skips mapping after retryable failure", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"reserved"}]}`, "transient", false, []string{"ordinary", "ordinary", "enterprise"}, 200},
		{"keyword skips eligible mapping after failure", "/v1/messages", "", "context", false, nil, 200},
		{"mapping failure reaches enterprise", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "context", true, []string{"ordinary", "mapped", "enterprise"}, 200},
		{"blocked conversation stops before backup", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "blocked", false, []string{"ordinary"}, 403},
		{"invalid parameters stop before backup", "/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Hello"}]}`, "invalid", false, []string{"ordinary"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == "" {
				body = strings.Replace(strings.Replace(capabilitySchemaRequest, `"model":"gpt-6-luna"`, `"model":"model"`, 1), "Return a number", "reserved", 1)
				tc.want = []string{"ordinary", "enterprise"}
			}
			ordinaryError, mappedFailure = tc.ordinaryError, tc.mappedFailure
			mu.Lock()
			calls = nil
			mu.Unlock()
			w := httptest.NewRecorder()
			svc.proxySmartGroup(w, httptest.NewRequest("POST", tc.path, strings.NewReader(body)), *group)
			mu.Lock()
			got := append([]string{}, calls...)
			mu.Unlock()
			if w.Code != tc.status || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("status=%d calls=%v want=%v body=%s", w.Code, got, tc.want, w.Body.String())
			}
		})
	}
	// Portable server capabilities bypass the ordinary member and are actually
	// converted to Responses tools, with no enterprise call.
	skip := KeywordMatchModeSkipOnMatch
	if err := svc.UpdateMemberPolicy(ctx, "user", "fallback-group", routes[0].ID, UpdateMemberPolicyRequest{RequestMatchMode: &skip, RequestMatchCapabilities: &policy.RequestMatchCapabilities}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", strings.Replace(offloadPDFRequest, `"model":"alias"`, `"model":"model"`, 1)},
		{"/v1/messages", `{"model":"model","messages":[{"role":"user","content":"Calculate 7*9"}],"tools":[{"type":"code_execution_20250825","name":"code_execution"}],"tool_choice":{"type":"tool","name":"code_execution"}}`},
		{"/v1/chat/completions", strings.Replace(mappedChatPDF, `"model":"alias"`, `"model":"model"`, 1)},
		{"/v1/chat/completions", strings.Replace(mappedChatCode, `"model":"alias"`, `"model":"model"`, 1)},
	} {
		ordinaryError, mappedFailure = "", false
		mu.Lock()
		calls = nil
		mu.Unlock()
		w := httptest.NewRecorder()
		svc.proxySmartGroup(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)), *group)
		mu.Lock()
		got := append([]string{}, calls...)
		mu.Unlock()
		if w.Code != 200 || !reflect.DeepEqual(got, []string{"mapped"}) {
			t.Fatalf("portable capability calls=%v status=%d body=%s", got, w.Code, w.Body.String())
		}
		if (strings.Contains(tc.body, "code_execution_") || strings.Contains(tc.body, "code_interpreter")) && !strings.Contains(w.Body.String(), "ci_real") {
			t.Fatal("actual code execution record lost")
		}
	}
}
