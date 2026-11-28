package model_proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

const mappedThinkingResponse = `{"id":"resp_summary","model":"gpt-6-luna","status":"completed","output":[{"type":"reasoning","encrypted_content":"GPT_ENCRYPTED_PRIVATE","content":[{"type":"reasoning_text","text":"RAW_PRIVATE_REASONING"}],"summary":[{"type":"summary_text","text":"Checked the supplied values."}]},{"type":"message","content":[{"type":"output_text","text":"Example answer."}]}],"usage":{"input_tokens":20,"output_tokens":10}}`

func thinkingMappedRoute() Route {
	return Route{ID: "mapped", ModelMappingEnabled: true, ModelMapping: map[string]string{"alias": "gpt-6-luna"}, SimulateThinking: true, RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: []string{"json_schema", "web_search"}, KeywordMatchMode: KeywordMatchModeRequireMatch}
}

func TestThinkingPolicyRequiresExplicitOptInAndPreservesOtherCapabilities(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name, body string
		change     func(*Route)
		mapped     bool
	}{
		{"enabled", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, nil, true},
		{"adaptive", `{"thinking":{"type":"adaptive"}}`, nil, true},
		{"signed continuation", `{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":"old"},{"type":"text","text":"A"}]}]}`, nil, true},
		{"thinking and schema", `{"thinking":{"type":"enabled"},"output_config":{"format":{"type":"json_schema"}}}`, nil, true},
		{"thinking and search", `{"thinking":{"type":"enabled"},"tools":[{"type":"web_search_20250305"}]}`, nil, true},
		{"simulation off", `{"thinking":{"type":"enabled"}}`, func(r *Route) { r.SimulateThinking = false }, false},
		{"thinking off", `{"thinking":{"type":"enabled"}}`, func(r *Route) { r.ThinkingEnabled = &disabled }, false},
		{"mapping off", `{"thinking":{"type":"enabled"}}`, func(r *Route) { r.ModelMappingEnabled = false }, false},
		{"prompt mention", `{"messages":[{"role":"user","content":"thinking signature tests"}]}`, nil, false},
		{"redacted state", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, nil, false},
		{"container state", `{"thinking":{"type":"enabled"},"container":"container_1"}`, nil, false},
		{"native tool history", `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"server_tool_use"}]}]}`, nil, false},
		{"unsupported mixed PDF", `{"thinking":{"type":"enabled"},"messages":[{"role":"user","content":[{"type":"document","source":{"media_type":"application/pdf"}}]}]}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := thinkingMappedRoute()
			if tc.change != nil {
				tc.change(&route)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.body), &payload); err != nil {
				t.Fatal(err)
			}
			facts := inspectRequestCapabilities(payload)
			routes := filterRoutesByRequest(facts, []Route{route}, "alias")
			routes = filterRoutesByKeywordsAndRequest("no keyword", []string{"special"}, routes, facts, "alias")
			if (len(routes) == 1) != tc.mapped {
				t.Fatalf("routes=%v facts=%+v", routes, facts)
			}
		})
	}
	// An ambiguous/native aggregate must not become transferable merely because
	// the switch is on. There must be actual supported thinking in the request.
	if got := filterRoutesByRequest(requestCapabilities{NativeClaude: true}, []Route{thinkingMappedRoute()}, "alias"); len(got) != 0 {
		t.Fatal("unknown native state became transferable")
	}
}

func TestMappedThinkingFormatMatchesSavedEnterpriseBlockAndSSE(t *testing.T) {
	raw, err := os.ReadFile("testdata/enterprise_thinking_format.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	format := fixture["content"].([]any)[0].(map[string]any)
	savedStream, err := os.ReadFile("testdata/enterprise_thinking_format.sse")
	if err != nil {
		t.Fatal(err)
	}
	savedDeltas := map[string]map[string]any{}
	for _, line := range strings.Split(string(savedStream), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if delta, ok := event["delta"].(map[string]any); ok {
			savedDeltas[firstString(delta, "type")] = delta
		}
	}
	for _, stream := range []bool{false, true} {
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(mappedThinkingResponse))}
		if err := offloadResponseToMessagesChecked(response, "gpt-6-luna", stream, false, nil, true); err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(response.Body)
		response.Body.Close()
		for _, private := range []string{"GPT_ENCRYPTED_PRIVATE", "RAW_PRIVATE_REASONING"} {
			if bytes.Contains(out, []byte(private)) {
				t.Fatal("private reasoning crossed the adapter")
			}
		}
		if response.Header.Get("X-TransitHub-Thinking-Mode") != "mapped-summary" {
			t.Fatal("missing mode header")
		}
		if stream {
			var kinds []string
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if delta, ok := event["delta"].(map[string]any); ok {
					kind := firstString(delta, "type")
					if kind == "thinking_delta" || kind == "signature_delta" {
						kinds = append(kinds, kind)
						saved, ok := savedDeltas[kind]
						if !ok {
							t.Fatalf("event %s absent from enterprise format", kind)
						}
						for key := range delta {
							if _, ok := saved[key]; !ok {
								t.Fatalf("field %s absent from saved %s", key, kind)
							}
						}
					}
					if kind == "signature_delta" {
						assertReservedThinkingSignature(t, delta["signature"])
					}
				}
			}
			if !reflect.DeepEqual(kinds, []string{"thinking_delta", "signature_delta"}) || !bytes.Contains(out, []byte("event: message_stop")) {
				t.Fatalf("incomplete thinking stream: %s", out)
			}
		} else {
			var message map[string]any
			if err := json.Unmarshal(out, &message); err != nil {
				t.Fatal(err)
			}
			block := message["content"].([]any)[0].(map[string]any)
			if len(block) != len(format) {
				t.Fatal("thinking fields differ from saved format")
			}
			for key := range format {
				if _, ok := block[key]; !ok {
					t.Fatalf("missing saved field %s", key)
				}
			}
			if block["thinking"] != "Checked the supplied values." {
				t.Fatalf("bad thinking block: %v", block)
			}
			assertReservedThinkingSignature(t, block["signature"])
			metadata := message["transithub"].(map[string]any)
			if metadata["upstream_model"] != "gpt-6-luna" || metadata["thinking"].(map[string]any)["summary_available"] != true {
				t.Fatal("lost origin/summary status")
			}
		}
	}
}

func TestMappedThinkingDoesNotInventMissingSummaryOrAffectDisabledMode(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(offloadTextResponse))}
		if err := offloadResponseToMessagesChecked(response, "gpt-6-luna", false, false, nil, enabled); err != nil {
			t.Fatal(err)
		}
		var message map[string]any
		if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		blocks := message["content"].([]any)
		if enabled {
			block := blocks[0].(map[string]any)
			if block["thinking"] != "" {
				t.Fatal("invented summary")
			}
			assertReservedThinkingSignature(t, block["signature"])
			if message["transithub"].(map[string]any)["thinking"].(map[string]any)["summary_available"] != false {
				t.Fatal("missing summary reported as available")
			}
		} else if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" || response.Header.Get("X-TransitHub-Thinking-Mode") != "" {
			t.Fatal("disabled simulation changed existing format")
		}
	}
}

func TestMappedThinkingContinuationStripsOnlyPrivateHistory(t *testing.T) {
	raw := `{"model":"gpt-6-luna","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"Calculate"},{"role":"assistant","content":[{"type":"thinking","thinking":"CLAUDE_PRIVATE_REASONING","signature":"CLAUDE_REAL_SIGNATURE"},{"type":"text","text":"Visible answer"},{"type":"tool_use","id":"call_1","name":"sum","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"42"}]}]}`
	var source map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&source); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(source)
	visible, err := mappedThinkingSource(source)
	if err != nil {
		t.Fatal(err)
	}
	converted, _, err := messagesToResponses(visible)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(converted)
	for _, private := range []string{"CLAUDE_PRIVATE_REASONING", "CLAUDE_REAL_SIGNATURE", `"thinking"`} {
		if bytes.Contains(out, []byte(private)) {
			t.Fatal("private history forwarded")
		}
	}
	for _, required := range []string{"Visible answer", "function_call", "function_call_output", "9007199254740993", "42"} {
		if !bytes.Contains(out, []byte(required)) {
			t.Fatalf("lost visible/tool history: %s", required)
		}
	}
	after, _ := json.Marshal(source)
	if !bytes.Equal(before, after) {
		t.Fatal("native retry payload mutated")
	}
	for _, body := range []string{`{"container":"opaque"}`, `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`} {
		var native map[string]any
		json.Unmarshal([]byte(body), &native)
		if _, err := mappedThinkingSource(native); err == nil {
			t.Fatal("opaque state accepted")
		}
	}
}

func TestMappedThinkingPreservesSchemaAndExplicitDisabledRequest(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		var source map[string]any
		if err := json.Unmarshal([]byte(capabilitySchemaRequest), &source); err != nil {
			t.Fatal(err)
		}
		source["thinking"] = map[string]any{"type": "enabled"}
		if disabled {
			source["thinking"] = map[string]any{"type": "disabled"}
			source["messages"] = []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "private", "signature": "native"}, map[string]any{"type": "text", "text": "Previous answer"}}}, map[string]any{"role": "user", "content": "Return a number"}}
		}
		raw, _ := json.Marshal(source)
		adapted, err := adaptMemberUpstreamProtocol(httptest.NewRequest("POST", "/v1/messages", nil), io.NopCloser(bytes.NewReader(raw)), thinkingMappedRoute())
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer adapted.release()
			var sent map[string]any
			if err := json.NewDecoder(adapted.body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			adapted.body.Close()
			if sent["thinking"] != nil || (sent["reasoning"] == nil) != disabled || sent["text"] == nil {
				t.Fatal("thinking/schema settings lost")
			}
			body := strings.Replace(mappedThinkingResponse, "Example answer.", `{\"answer\":323}`, 1)
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			if err := adapted.responseAdapter(response); err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if bytes.Contains(out, []byte(`"type":"thinking"`)) == disabled {
				t.Fatalf("explicit disabled setting ignored: %s", out)
			}
		}()
	}
}

func TestThinkingPolicyPersistenceDefaultsPartialUpdatesAndIsolation(t *testing.T) {
	repo := memberPauseTestRepository(t)
	seedPauseMember(t, repo, Route{ID: "thinking-route"}, "a", "b")
	svc := &Service{repository: repo, accounts: proxyTestAccount{}}
	ctx := context.Background()
	enabled, disabled := true, false
	mapping := map[string]string{"model": "gpt-6-luna"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "thinking-route", UpdateMemberPolicyRequest{SimulateThinking: &enabled}); err == nil {
		t.Fatal("simulation enabled without mapping")
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "thinking-route", UpdateMemberPolicyRequest{ModelMappingEnabled: &enabled, ModelMapping: &mapping, SimulateThinking: &enabled}); err != nil {
		t.Fatal(err)
	}
	priority := 4
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "thinking-route", UpdateMemberPolicyRequest{Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"a", "b"} {
		members, err := NewRepository(repo.db).ListGroupRoutes(ctx, group, false, "")
		if err != nil || len(members) != 1 {
			t.Fatalf("members=%v err=%v", members, err)
		}
		member := members[0]
		if member.ThinkingEnabled == nil || !*member.ThinkingEnabled || member.SimulateThinking != (group == "a") {
			t.Fatalf("bad persisted policy in %s: %+v", group, member)
		}
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "thinking-route", UpdateMemberPolicyRequest{ThinkingEnabled: &disabled}); err == nil {
		t.Fatal("partial update left simulation enabled without thinking")
	}
	if err := svc.UpdateMemberPolicy(ctx, "other-user", "a", "thinking-route", UpdateMemberPolicyRequest{SimulateThinking: &disabled}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unauthorized update: %v", err)
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "a", "thinking-route", UpdateMemberPolicyRequest{ThinkingEnabled: &disabled, SimulateThinking: &disabled}); err != nil {
		t.Fatal(err)
	}
	members, err := repo.ListGroupRoutes(ctx, "a", false, "")
	if err != nil || members[0].thinkingAllowed() || members[0].SimulateThinking {
		t.Fatalf("toggle off did not persist: %v", err)
	}
}

func TestMappedThinkingWireRequestsKeepVisibleHistoryAndNativeReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		server := httptest.NewServer(http.NotFoundHandler())
		svc, route := preparedProbeService(t, server)
		policy := thinkingMappedRoute()
		route.ModelMappingEnabled, route.ModelMapping, route.SimulateThinking = true, policy.ModelMapping, true
		calls := 0
		svc.dataClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body, _ := io.ReadAll(req.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if req.URL.Path != "/v1/responses" || payload["model"] != "gpt-6-luna" || payload["thinking"] != nil {
				t.Fatalf("bad mapped request: %s %s", req.URL.Path, body)
			}
			reasoning := payload["reasoning"].(map[string]any)
			if reasoning["summary"] != "auto" {
				t.Fatal("public summary was not requested")
			}
			for _, private := range []string{"native-signature", "private-thinking"} {
				if bytes.Contains(body, []byte(private)) {
					t.Fatal("native private state sent upstream")
				}
			}
			if !bytes.Contains(body, []byte("Visible previous answer")) {
				t.Fatal("visible history lost")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":" + mappedThinkingResponse + "}\n\n"))}, nil
		})
		raw := `{"model":"alias","thinking":{"type":"enabled","budget_tokens":1024},"max_tokens":2048,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private-thinking","signature":"native-signature"},{"type":"text","text":"Visible previous answer"}]},{"role":"user","content":"Continue"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
		replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
		if err != nil {
			t.Fatal(err)
		}
		incoming := httptest.NewRequest("POST", "/v1/messages", nil)
		mapping, err := newMappedResponseContext(incoming, replay, "alias", route.ModelMapping)
		if err != nil {
			t.Fatal(err)
		}
		response, retry, err := svc.performAttempt(incoming, route, replay.Open, route.ModelMapping, mapping, false)
		if err != nil || retry || response == nil {
			t.Fatalf("attempt failed: %v retry=%v", err, retry)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if calls != 1 || isEventStream(response.Header) != stream || !bytes.Contains(body, []byte(`"model":"alias"`)) || !bytes.Contains(body, []byte("Checked the supplied values.")) {
			t.Fatalf("bad mapped thinking response: %s", body)
		}
		assertMappedThinkingSignatures(t, body, stream)
		original, _ := replay.Open()
		unchanged, _ := io.ReadAll(original)
		original.Close()
		if string(unchanged) != raw {
			t.Fatal("native fallback replay changed")
		}
		replay.Close()
		server.Close()
	}
}

func TestThinkingSmartGroupRepeatedRequestsUseMappingAndNativeModePreservesSignature(t *testing.T) {
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var mappedCalls, nativeCalls atomic.Int64
	nativeResponse := `{"id":"native","type":"message","role":"assistant","model":"model","content":[{"type":"thinking","thinking":"","signature":"native-test-signature"},{"type":"text","text":"Native answer."}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":10}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			mappedCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("old-native-signature")) || bytes.Contains(body, []byte("old-private-thinking")) {
				t.Error("private native state reached mapping")
			}
			io.WriteString(w, mappedThinkingResponse)
		case "/v1/messages":
			nativeCalls.Add(1)
			io.WriteString(w, nativeResponse)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	svc, native := preparedProbeService(t, server)
	svc.repository, svc.limiter, svc.accounts = repo, limiter, proxyTestAccount{}
	native.ID = healthTestRoute(t, client).ID
	mapped := native
	mapped.ID = healthTestRoute(t, client).ID
	cleanupRedisTestRoutes(t, client, []Route{native, mapped})
	seedPauseMember(t, repo, native, "thinking-group")
	seedPauseMember(t, repo, mapped)
	ctx := context.Background()
	if err := repo.AddMember(ctx, "user", "workspace", "thinking-group", mapped.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceRouteModels(ctx, mapped.ID, []Model{{ID: "gpt-6-luna", Object: "model"}}); err != nil {
		t.Fatal(err)
	}
	enabled, disabled, priority := true, false, 5
	mapping := map[string]string{"model": "gpt-6-luna"}
	required, caps := KeywordMatchModeRequireMatch, []string{"json_schema", "web_search"}
	if err := svc.UpdateMemberPolicy(ctx, "user", "thinking-group", mapped.ID, UpdateMemberPolicyRequest{ModelMappingEnabled: &enabled, ModelMapping: &mapping, SimulateThinking: &enabled, Priority: &priority, RequestMatchMode: &required, RequestMatchCapabilities: &caps, KeywordMatchMode: &required}); err != nil {
		t.Fatal(err)
	}
	group, err := repo.GetSmartGroup(ctx, "thinking-group")
	if err != nil {
		t.Fatal(err)
	}
	group.Keywords = []string{"absent keyword"}
	request := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.proxySmartGroup(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), *group)
		return w
	}
	fresh := `{"model":"model","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"Answer"}]}`
	history := `{"model":"model","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"old-private-thinking","signature":"old-native-signature"},{"type":"text","text":"Previous answer"}]},{"role":"user","content":"Continue"}]}`
	for i := 0; i < 10; i++ {
		for _, body := range []string{fresh, history} {
			w := request(body)
			if w.Code != 200 || w.Header().Get("X-TransitHub-Thinking-Mode") != "mapped-summary" {
				t.Fatalf("round=%d status=%d body=%s", i, w.Code, w.Body.String())
			}
			assertMappedThinkingSignatures(t, w.Body.Bytes(), false)
		}
	}
	if mappedCalls.Load() != 20 || nativeCalls.Load() != 0 {
		t.Fatalf("mapped=%d native=%d", mappedCalls.Load(), nativeCalls.Load())
	}
	if err := svc.UpdateMemberPolicy(ctx, "user", "thinking-group", mapped.ID, UpdateMemberPolicyRequest{SimulateThinking: &disabled}); err != nil {
		t.Fatal(err)
	}
	w := request(fresh)
	if w.Code != 200 || w.Body.String() != nativeResponse || mappedCalls.Load() != 20 || nativeCalls.Load() != 1 {
		t.Fatalf("native mode altered signature: status=%d mapped=%d native=%d body=%s", w.Code, mappedCalls.Load(), nativeCalls.Load(), w.Body.String())
	}
}
