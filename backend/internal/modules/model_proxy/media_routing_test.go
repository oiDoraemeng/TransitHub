package model_proxy

import (
	"bytes"
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
	"time"
)

func mediaPayload(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestMediaDetectionIncludesHistoryWithoutReadingTextOrToolSchemas(t *testing.T) {
	for _, test := range []struct {
		name, body                   string
		audio, video, pdf, reference bool
	}{
		{"chat-audio", `{"messages":[{"content":[{"type":"input_audio","input_audio":{"format":"wav","data":"YQ=="}}]},{"content":"next turn"}]}`, true, false, false, false},
		{"mislabelled-video", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:video/mp4;base64,YQ=="}}]}]}`, false, true, false, false},
		{"responses-pdf", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:application/pdf;base64,YQ=="}]}]}`, false, false, true, false},
		{"gemini-pdf-url", `{"contents":[{"parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"https://example.test/f.pdf"}}]}]}`, false, false, true, true},
		{"chat-pdf-url", `{"messages":[{"content":[{"type":"file","file":{"filename":"document.pdf","file_url":"https://example.test/download/123"}}]}]}`, false, false, true, true},
		{"responses-pdf-url", `{"input":[{"content":[{"type":"input_file","filename":"document.pdf","file_url":"https://example.test/download/123"}]}]}`, false, false, true, true},
		{"gemini-audio-snake", `{"contents":[{"parts":[{"inline_data":{"mime_type":"audio/mpeg","data":"YQ=="}}]}]}`, true, false, false, false},
		{"url-extension", `{"messages":[{"content":[{"type":"video_url","video_url":{"url":"https://example.test/f.mp4?token=not-logged"}}]}]}`, false, true, false, false},
		{"ordinary-text", `{"messages":[{"content":"Explain data:video/mp4;base64,YQ== and input_audio"}]}`, false, false, false, false},
		{"tool-schema", `{"messages":[{"content":"hello"}],"tools":[{"type":"input_audio","inlineData":{"mimeType":"video/mp4"}}]}`, false, false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := inspectRequestMedia(mediaPayload(t, test.body))
			if m.Audio != test.audio || m.Video != test.video || m.PDF != test.pdf || m.FileReference != test.reference {
				t.Fatalf("media=%+v", m)
			}
		})
	}
}

func TestMediaRoutingNeverFallsBackToGPTOrOtherGroups(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	routes := []Route{
		{ID: "mapped", Name: "gemini 映射", Priority: 10, ModelMappingEnabled: true, ModelMapping: map[string]string{"gemini-test": "gpt-6-luna"}},
		{ID: "pro", Name: "gemini pro", Priority: 9},
		{ID: "ultra", Name: "gemini ultra", Priority: 0},
	}
	for _, m := range []requestMedia{{Audio: true, Model: "gemini-test"}, {Video: true, Model: "gemini-test"}} {
		got := filterMediaRoutes(routes, m, request)
		if len(got) != 1 || got[0].ID != "ultra" {
			t.Fatalf("media selected %+v", got)
		}
		if got := filterMediaRoutes(routes[:2], m, request); len(got) != 0 {
			t.Fatalf("missing Ultra fell through: %+v", got)
		}
	}
	if got := filterMediaRoutes(routes, requestMedia{PDF: true, Model: "gemini-test"}, request); len(got) != 3 {
		t.Fatal("inline PDF unnecessarily changed ordinary scheduling")
	}
	stream := httptest.NewRequest("POST", "/v1beta/models/gemini-test:streamGenerateContent?alt=sse", nil)
	if got := filterMediaRoutes(routes, requestMedia{PDF: true, Model: "gemini-test"}, stream); len(got) != 2 || got[0].ID == "mapped" {
		t.Fatalf("native stream PDF selected GPT: %+v", got)
	}
	spoof := routes[2]
	spoof.ModelMappingEnabled = true
	spoof.ModelMapping = map[string]string{"gemini-test": "gpt-6-luna"}
	if got := filterMediaRoutes([]Route{spoof}, requestMedia{Audio: true, Model: "gemini-test"}, request); len(got) != 0 {
		t.Fatal("Ultra name bypassed final model check")
	}
}

func TestGeminiPDFConversionPreservesBytes(t *testing.T) {
	raw := `{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`
	converted, err := geminiToChatCompletions("gpt-6-luna", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	payload := mediaPayload(t, string(converted))
	parts := payload["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts=%v", parts)
	}
	file := parts[1].(map[string]any)
	if file["type"] != "file" || file["file"].(map[string]any)["file_data"] != "data:application/pdf;base64,JVBERi0=" {
		t.Fatalf("PDF bytes lost: %v", file)
	}
}

func TestMislabelledPDFUsesFileBeforeGPTForwarding(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		raw := `{"model":"gpt-6-luna","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0="}}]}]}`
		if endpoint == "/v1/responses" {
			raw = `{"model":"gpt-6-luna","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:application/pdf;base64,JVBERi0="}]}]}`
		}
		request, _ := withRequestMedia(httptest.NewRequest("POST", endpoint, nil), mediaPayload(t, raw), "gpt-6-luna")
		adapted, err := adaptMediaUpstreamProtocol(request, io.NopCloser(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(adapted.body)
		adapted.body.Close()
		if bytes.Contains(got, []byte(`"image_url"`)) || !bytes.Contains(got, []byte(`"file_data":"data:application/pdf;base64,JVBERi0="`)) {
			t.Fatalf("PDF not normalized: %s", got)
		}
	}
}

func TestNativeGeminiMediaPassesThroughByteForByte(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent?alt=sse"} {
		raw := `{"contents":[{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"dmlkZW8="},"videoMetadata":{"fps":1}}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`
		request, _ := withRequestMedia(httptest.NewRequest("POST", "/v1beta/models/gemini-test:"+action, nil), mediaPayload(t, raw), "gemini-test")
		adapted, err := adaptMediaUpstreamProtocol(request, io.NopCloser(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(adapted.body)
		adapted.body.Close()
		if string(got) != raw || adapted.requestURI != request.URL.RequestURI() || adapted.responseAdapter != nil {
			t.Fatalf("native media altered: %s %s", adapted.requestURI, got)
		}
	}
}

func TestOpenAIMediaHTTPBridgePreservesAttachmentsAndClientProtocol(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", endpoint, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					want := "/v1beta/models/gemini-test:generateContent"
					if stream {
						want = "/v1beta/models/gemini-test:streamGenerateContent"
					}
					if r.URL.Path != want {
						t.Errorf("path=%s", r.URL.Path)
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					parts := payload["contents"].([]any)[0].(map[string]any)["parts"].([]any)
					data := parts[1].(map[string]any)["inlineData"].(map[string]any)
					if data["mimeType"] != "audio/wav" || data["data"] != "c2lsdmVyIG90dGVy" {
						t.Errorf("media=%v", data)
					}
					if stream {
						if r.URL.Query().Get("alt") != "sse" {
							t.Error("missing SSE")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"silver \"}]}}]}\n\n")
						w.(http.Flusher).Flush()
						fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"otter\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2,\"totalTokenCount\":6}}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"modelVersion":"upstream-name","candidates":[{"content":{"parts":[{"text":"private thought","thought":true},{"text":"silver otter"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`)
					}
				}))
				defer server.Close()
				service, route := preparedProbeService(t, server)
				raw := fmt.Sprintf(`{"model":"gemini-test","stream":%v,"messages":[{"role":"user","content":[{"type":"text","text":"transcribe"},{"type":"input_audio","input_audio":{"format":"wav","data":"c2lsdmVyIG90dGVy"}}]}]}`, stream)
				if endpoint == "/v1/responses" {
					raw = fmt.Sprintf(`{"model":"gemini-test","stream":%v,"input":[{"role":"user","content":[{"type":"input_text","text":"transcribe"},{"type":"input_audio","input_audio":{"format":"wav","data":"c2lsdmVyIG90dGVy"}}]}]}`, stream)
				}
				request, _ := withRequestMedia(httptest.NewRequest("POST", endpoint, nil), mediaPayload(t, raw), "client-alias")
				response, retry, err := service.performAttempt(request, route, func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(raw)), nil }, nil, mappedResponseContext{}, false)
				if err != nil || retry {
					t.Fatalf("attempt: %v retry=%v", err, retry)
				}
				defer response.Body.Close()
				got, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(got), "private thought") || !strings.Contains(string(got), "client-alias") || !strings.Contains(string(got), "otter") {
					t.Fatalf("response=%s", got)
				}
				if stream && endpoint == "/v1/chat/completions" && !bytes.Contains(got, []byte("data: [DONE]")) {
					t.Fatal("missing Chat terminator")
				}
				if stream && endpoint == "/v1/responses" && !bytes.Contains(got, []byte("response.completed")) {
					t.Fatal("missing Responses completion")
				}
				if !stream {
					var payload map[string]any
					if json.Unmarshal(got, &payload) != nil {
						t.Fatalf("non-stream response=%s", got)
					}
				}
			})
		}
	}
}

func TestMediaInvalidInputIsNotRetried(t *testing.T) {
	request, _ := withRequestMedia(httptest.NewRequest("POST", "/v1/chat/completions", nil), mediaPayload(t, `{"messages":[{"content":[{"type":"input_audio"}]}]}`), "gemini-test")
	_, err := adaptMediaUpstreamProtocol(request, io.NopCloser(strings.NewReader(`{"model":"gemini-test","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav","data":"[Image]"}}]}]}`)))
	var inputErr *mediaInputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("invalid Base64 accepted: %v", err)
	}
	calls := 0
	_, err = attemptSmartGroupMember(request.Context(), func(time.Duration) (*http.Response, error) { calls++; return nil, inputErr })
	if calls != 1 || !errors.As(err, &inputErr) {
		t.Fatalf("client input retried %d times: %v", calls, err)
	}
}

func TestMediaStreamTruncationAndCancellation(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))}
		if err := geminiMediaToOpenAI(response, endpoint, "gemini-test", true); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if !errors.Is(err, io.ErrUnexpectedEOF) || bytes.Contains(body, []byte("[DONE]")) || bytes.Contains(body, []byte("response.completed")) {
			t.Fatalf("truncated stream marked complete: %s %v", body, err)
		}
	}
	reader, writer := io.Pipe()
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}
	if err := geminiMediaToOpenAI(response, "/v1/chat/completions", "gemini-test", true); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := writer.Write([]byte("event")); err == nil {
		t.Fatal("closing client did not close source")
	}
	writer.Close()
}

func TestMediaResponsesStartupWaitsForRealOutput(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}
	if err := geminiMediaToOpenAI(response, "/v1/responses", "gemini-test", true); err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := prepareStreamStart(context.Background(), response, 30*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("synthetic metadata bypassed the upstream startup timeout: %v", err)
	}
}

func TestMediaToolHistoryPreservesArgumentsAndSettings(t *testing.T) {
	payload := mediaPayload(t, `{"model":"gemini-test","max_tokens":20,"max_completion_tokens":50,"messages":[{"role":"system","content":"Read attachments carefully."},{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{\"id\":9007199254740993}"}}]},{"role":"tool","tool_call_id":"call1","content":"found"},{"role":"user","content":[{"type":"file","file":{"filename":"video.mp4","file_data":"dmlkZW8="}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"required"}`)
	got, err := openAIMediaToGemini(payload, "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	for _, want := range []string{`9007199254740993`, `"maxOutputTokens":50`, `"mode":"ANY"`, `"functionResponse"`, `"mimeType":"video/mp4"`, `"systemInstruction"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}

func TestInlinePDFToNativeGeminiUsesNativeProtocol(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		raw := `{"model":"gemini-test","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`
		if endpoint == "/v1/responses" {
			raw = `{"model":"gemini-test","input":[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]}`
		}
		request, _ := withRequestMedia(httptest.NewRequest("POST", endpoint, nil), mediaPayload(t, raw), "gemini-test")
		converted, err := adaptMediaUpstreamProtocol(request, io.NopCloser(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(converted.body)
		converted.body.Close()
		if converted.requestURI != "/v1beta/models/gemini-test:generateContent" || !bytes.Contains(got, []byte(`"mimeType":"application/pdf"`)) || !bytes.Contains(got, []byte(`"data":"JVBERi0="`)) {
			t.Fatalf("PDF did not reach native Gemini: %s %s", converted.requestURI, got)
		}
	}
}

func TestMediaSmartGroupPostgresRedisUsesOnlyUltra(t *testing.T) {
	for _, scenario := range []string{"success", "failed", "full", "paused", "pdf-fallback"} {
		t.Run(scenario, func(t *testing.T) {
			repo := memberPauseTestRepository(t)
			limiter, client := redisTestLimiter(t)
			var ultraCalls, gptCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/models/gemini-test:") {
					gptCalls.Add(1)
					w.WriteHeader(400)
					return
				}
				ultraCalls.Add(1)
				if scenario == "pdf-fallback" {
					body, _ := io.ReadAll(r.Body)
					if !bytes.Contains(body, []byte(`"mimeType":"application/pdf"`)) || !bytes.Contains(body, []byte(`"data":"JVBERi0="`)) {
						t.Error("PDF missing after fallback")
					}
				}
				if scenario == "failed" {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"silver otter"}]},"finishReason":"STOP"}]}`)
			}))
			defer server.Close()
			service, ultra := preparedProbeService(t, server)
			service.repository, service.limiter, service.accounts = repo, limiter, proxyTestAccount{}
			ultra.ID = healthTestRoute(t, client).ID
			ultra.ConcurrencyLimit = 50
			gpt := ultra
			gpt.ID = healthTestRoute(t, client).ID
			seedPauseMember(t, repo, ultra, "media-group")
			seedPauseMember(t, repo, gpt)
			ctx := context.Background()
			if err := repo.AddMember(ctx, "user", "workspace", "media-group", gpt.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.db.Exec(ctx, `UPDATE proxy_smart_groups SET keywords=ARRAY['restricted-keyword']::text[] WHERE id='media-group'`); err != nil {
				t.Fatal(err)
			}
			for _, member := range []struct {
				route       Route
				name, model string
				priority    int
				mapped      bool
			}{
				{ultra, "gemini ultra", "gemini-test", 0, false},
				{gpt, "gemini 映射", "gpt-6-luna", 10, true},
			} {
				if _, err := repo.db.Exec(ctx, `UPDATE proxy_routes SET name=$2 WHERE id=$1`, member.route.ID, member.name); err != nil {
					t.Fatal(err)
				}
				if err := repo.ReplaceRouteModels(ctx, member.route.ID, []Model{{ID: member.model, Object: "model"}}); err != nil {
					t.Fatal(err)
				}
				provided, neverPause := true, true
				mapping := `{"gemini-test":"gpt-6-luna"}`
				keywordMode := KeywordMatchModeDisabled
				if member.route.ID == ultra.ID && scenario != "pdf-fallback" {
					keywordMode = KeywordMatchModeRequireMatch
				}
				if err := repo.UpdateMemberPolicy(ctx, "user", "workspace", "media-group", member.route.ID, nil, nil, nil, &member.priority, &member.mapped, &mapping, &provided, nil, nil, &keywordMode, nil, nil, nil, &neverPause); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "full" {
				for i := 0; i < 50; i++ {
					lease := acquireTestLease(t, limiter, ultra, fmt.Sprintf("held-%d", i))
					defer lease.Release(ctx)
				}
			}
			if scenario == "paused" {
				paused := true
				if err := repo.UpdateMemberPolicy(ctx, "user", "workspace", "media-group", ultra.ID, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, &paused, nil); err != nil {
					t.Fatal(err)
				}
			}
			group, err := repo.GetSmartGroup(ctx, "media-group")
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			body := `{"model":"gemini-test","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav","data":"c2lsdmVy"}}]}]}`
			if scenario == "pdf-fallback" {
				body = `{"model":"gemini-test","stream":true,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0="}}]}]}`
			}
			service.proxySmartGroup(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)), *group)
			if scenario != "pdf-fallback" && gptCalls.Load() != 0 {
				t.Fatal("media fell back to a GPT mapping")
			}
			if (scenario == "success" || scenario == "pdf-fallback") && (w.Code != 200 || ultraCalls.Load() != 1 || !strings.Contains(w.Body.String(), "silver otter")) {
				t.Fatalf("Ultra failed: status=%d calls=%d body=%s", w.Code, ultraCalls.Load(), w.Body.String())
			}
			if scenario == "pdf-fallback" && gptCalls.Load() == 0 {
				t.Fatal("PDF did not exercise the higher priority GPT failure")
			}
			if scenario != "success" && scenario != "pdf-fallback" && w.Code < 400 {
				t.Fatalf("unavailable Ultra returned success: %d", w.Code)
			}
			if (scenario == "full" || scenario == "paused") && ultraCalls.Load() != 0 {
				t.Fatal("unavailable Ultra received traffic")
			}
		})
	}
}
