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
	"sync/atomic"
	"testing"
)

func TestGroupLocalReplyProtocolsBypassScheduling(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", compactPath, "/v1/messages", "/v1beta/models/alias:generateContent"} {
		t.Run(path, func(t *testing.T) {
			// No repository, limiter or HTTP client: local replies must need none.
			service := &Service{maxRequestBytes: 1 << 20}
			w := httptest.NewRecorder()
			body := `{"model":"alias","input":[{"role":"user","content":"hello"}],"messages":[{"role":"user","content":"hello"}],"contents":[{"parts":[{"text":"hello"}]}]}`
			service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100})
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if path == "/v1beta/models/alias:generateContent" {
				if response["modelVersion"] != "alias" {
					t.Fatalf("model=%v", response)
				}
				candidates := response["candidates"].([]any)
				candidate := candidates[0].(map[string]any)
				content := candidate["content"].(map[string]any)
				if candidate["finishReason"] != "STOP" || content["parts"].([]any)[0].(map[string]any)["text"] != "OK" {
					t.Fatalf("incomplete Gemini response: %v", response)
				}
				return
			}
			if response["model"] != "alias" || response["id"] == "" {
				t.Fatalf("wrong response model or ID: %v", response)
			}
			switch path {
			case "/v1/chat/completions":
				choice := response["choices"].([]any)[0].(map[string]any)
				if response["object"] != "chat.completion" || choice["finish_reason"] != "stop" || choice["message"].(map[string]any)["content"] != "OK" {
					t.Fatalf("invalid Chat response: %v", response)
				}
			case "/v1/messages":
				if response["type"] != "message" || response["stop_reason"] != "end_turn" || response["content"].([]any)[0].(map[string]any)["text"] != "OK" {
					t.Fatalf("invalid Messages response: %v", response)
				}
			default:
				output := response["output"].([]any)
				if path == compactPath {
					if response["object"] != "response.compaction" || len(output) != 2 || output[0].(map[string]any)["content"] != "hello" {
						t.Fatalf("Compact lost input: %v", response)
					}
				} else if response["object"] != "response" || response["status"] != "completed" {
					t.Fatalf("incomplete Responses response: %v", response)
				}
				message := output[len(output)-1].(map[string]any)
				if message["role"] != "assistant" || message["status"] != "completed" || message["content"].([]any)[0].(map[string]any)["text"] != "OK" {
					t.Fatalf("invalid output: %v", output)
				}
			}
		})
	}
}

func TestGroupLocalReplyStreamsCompleteAndRetainModel(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/alias:streamGenerateContent?alt=sse"} {
		t.Run(path, func(t *testing.T) {
			service := &Service{maxRequestBytes: 1 << 20}
			w := httptest.NewRecorder()
			body := `{"model":"alias","input":"hello","messages":[{"role":"user","content":"hello"}],"contents":[{"parts":[{"text":"hello"}]}],"stream":true,"stream_options":{"include_usage":true}}`
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			service.proxySmartGroup(w, r, SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100})
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" || !validateRouteProbeStream(strings.NewReader(w.Body.String()), r.URL.Path) {
				t.Fatalf("incomplete stream: status=%d body=%s", w.Code, w.Body.String())
			}
			sequence, modelCount := 0, 0
			var responseID, messageID string
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path == "/v1/responses" {
					if event["sequence_number"] != float64(sequence) {
						t.Fatalf("out-of-order event: %v", event)
					}
					sequence++
					if item, ok := event["item"].(map[string]any); ok {
						if messageID != "" && item["id"] != messageID {
							t.Fatal("message ID changed during stream")
						}
						messageID = item["id"].(string)
					}
				}
				for _, nested := range []string{"message", "response"} {
					if value, ok := event[nested].(map[string]any); ok {
						event = value
						break
					}
				}
				model, ok := event["model"]
				if !ok {
					model, ok = event["modelVersion"]
				}
				if ok {
					modelCount++
					if model != "alias" {
						t.Fatalf("wrong streamed model: %v", event)
					}
					if id, ok := event["id"].(string); ok {
						if responseID != "" && responseID != id {
							t.Fatal("response ID changed during stream")
						}
						responseID = id
					}
				}
			}
			if modelCount == 0 || (r.URL.Path == "/v1/responses" && sequence != 9) {
				t.Fatalf("missing stream metadata: %s", w.Body.String())
			}
		})
	}
}

func TestGroupLocalReplyChatStreamUsageContract(t *testing.T) {
	// OpenAI's include_usage contract requires null usage on every ordinary
	// chunk and a final chunk with empty choices and aggregate usage before DONE.
	for _, includeUsage := range []bool{false, true} {
		t.Run(fmt.Sprint(includeUsage), func(t *testing.T) {
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"alias","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":%t}}`, includeUsage)
			(&Service{maxRequestBytes: 1 << 20}).proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)), SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100})
			chunks, summaries := 0, 0
			for _, frame := range strings.Split(strings.TrimSpace(w.Body.String()), "\n\n") {
				if frame == "data: [DONE]" {
					continue
				}
				if summaries > 0 {
					t.Fatal("usage summary must be the final chunk before DONE")
				}
				var chunk map[string]json.RawMessage
				if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk); err != nil {
					t.Fatal(err)
				}
				chunks++
				usage, hasUsage := chunk["usage"]
				if hasUsage != includeUsage {
					t.Fatalf("usage field presence=%t want=%t: %s", hasUsage, includeUsage, frame)
				}
				if string(chunk["choices"]) == "[]" {
					summaries++
					var counts struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
						TotalTokens      int `json:"total_tokens"`
					}
					if !includeUsage || string(usage) == "null" || json.Unmarshal(usage, &counts) != nil || counts.PromptTokens != 0 || counts.CompletionTokens != 0 || counts.TotalTokens != 0 {
						t.Fatalf("invalid usage summary: %s", frame)
					}
				} else if includeUsage && string(usage) != "null" {
					t.Fatalf("ordinary chunk must have usage:null: %s", frame)
				}
			}
			wantChunks, wantSummaries := 3, 0
			if includeUsage {
				wantChunks, wantSummaries = 4, 1
			}
			if chunks != wantChunks || summaries != wantSummaries || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("wrong termination or usage order: %s", w.Body.String())
			}
		})
	}
}

func TestGroupLocalReplyThresholdAndOpaqueInputs(t *testing.T) {
	for _, test := range []struct {
		name, body     string
		threshold      int
		enabled, local bool
	}{
		{"disabled", `{"input":"x"}`, 100, false, false},
		{"below", `{"input":"123456789"}`, 4, true, true},
		{"equal", `{"input":"1234567890123"}`, 4, true, false},
		{"above", `{"input":"12345678901234567"}`, 4, true, false},
		{"adjustable", `{"input":"1234567890123"}`, 5, true, true},
		{"unicode", `{"input":"你好世界"}`, 2, true, true},
		{"instructions", `{"input":"x","instructions":"123456789012"}`, 4, true, false},
		{"system", `{"messages":[{"role":"system","content":"1234567890123"},{"role":"user","content":"x"}]}`, 4, true, false},
		{"history", `{"input":"x","previous_response_id":"resp_history"}`, 100, true, false},
		{"conversation", `{"input":"x","conversation":{"id":"conv_history"}}`, 100, true, false},
		{"image", `{"input":[{"type":"input_image","image_url":"https://example.test/img.png"}]}`, 100, true, false},
		{"gemini-file", `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}`, 100, true, false},
		{"token-array", `{"prompt":[1,2,3,4]}`, 4, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay, err := newReplayBody(io.NopCloser(strings.NewReader(test.body)), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			w := httptest.NewRecorder()
			local := (&Service{}).serveGroupLocalReply(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), SmartGroup{LocalReplyEnabled: test.enabled, LocalReplyMinTokens: test.threshold}, replay, "alias")
			if local != test.local || (!local && w.Body.Len() != 0) {
				t.Fatalf("local=%t want=%t response=%s", local, test.local, w.Body.String())
			}
		})
	}
}

func TestGroupLocalReplyRejectsInvalidSettings(t *testing.T) {
	for _, threshold := range []int{0, -1, 1000001} {
		for _, create := range []bool{false, true} {
			var err error
			if create {
				_, _, err = (&Service{}).CreateSmartGroup(context.Background(), "user", CreateSmartGroupRequest{LocalReplyMinTokens: &threshold})
			} else {
				_, err = (&Service{}).UpdateSmartGroup(context.Background(), "user", "group", UpdateSmartGroupRequest{LocalReplyMinTokens: &threshold})
			}
			var request *requestError
			if !errors.As(err, &request) || request.Status != http.StatusBadRequest {
				t.Fatalf("threshold=%d create=%t error=%v", threshold, create, err)
			}
		}
	}
}

func TestGroupLocalReplyPersistsPerGroupAndForwardsBoundary(t *testing.T) {
	ctx := context.Background()
	repo := memberPauseTestRepository(t)
	limiter, client := redisTestLimiter(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["model"] != "model" {
			t.Errorf("upstream mapping=%v err=%v", input, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"upstream","object":"response","model":"model","status":"completed","output":[]}`)
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
	route.ID = healthTestRoute(t, client).ID
	seedPauseMember(t, repo, route, "a", "b")
	for _, groupID := range []string{"a", "b"} {
		enabled, mapping := true, map[string]string{"alias": "model"}
		if err := service.UpdateMemberPolicy(ctx, "user", groupID, route.ID, UpdateMemberPolicyRequest{UseProvidedKey: &enabled, ModelMappingEnabled: &enabled, ModelMapping: &mapping}); err != nil {
			t.Fatal(err)
		}
		group, err := repo.GetSmartGroup(ctx, groupID)
		if err != nil || group.LocalReplyEnabled || group.LocalReplyMinTokens != 100 {
			t.Fatalf("default group=%+v err=%v", group, err)
		}
	}
	enabled, threshold := true, 4
	if _, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{LocalReplyEnabled: &enabled, LocalReplyMinTokens: &threshold}); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if _, err := service.UpdateSmartGroup(ctx, "user", "a", UpdateSmartGroupRequest{Name: &name}); err != nil {
		t.Fatal(err)
	}
	groups, err := NewRepository(repo.db).ListSmartGroups(ctx, "user", "workspace")
	if err != nil || len(groups) != 2 {
		t.Fatalf("list=%+v %v", groups, err)
	}
	for _, group := range groups {
		if (group.ID == "a") != group.LocalReplyEnabled || (group.ID == "a" && group.LocalReplyMinTokens != 4) || (group.ID == "b" && group.LocalReplyMinTokens != 100) {
			t.Fatalf("settings changed across update or groups: %+v", group)
		}
	}
	if _, err := service.UpdateSmartGroup(ctx, "other-user", "a", UpdateSmartGroupRequest{LocalReplyEnabled: &enabled}); err == nil {
		t.Fatal("group settings bypassed workspace ownership")
	}
	for _, test := range []struct {
		group, input string
		wantCalls    int64
	}{
		{"a", "123456789", 0},
		{"a", "1234567890123", 1},
		{"b", "x", 2},
	} {
		group, err := repo.GetSmartGroup(ctx, test.group)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		service.proxySmartGroup(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"alias","input":%q}`, test.input))), *group)
		if w.Code != http.StatusOK || calls.Load() != test.wantCalls || !strings.Contains(w.Body.String(), `"model":"alias"`) {
			t.Fatalf("group=%s status=%d calls=%d body=%s", test.group, w.Code, calls.Load(), w.Body.String())
		}
	}
	runtime := []Route{route}
	if err := limiter.PopulateRouteRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if runtime[0].TodayRequests != 2 {
		t.Fatalf("local reply counted as upstream traffic: %d", runtime[0].TodayRequests)
	}
}
