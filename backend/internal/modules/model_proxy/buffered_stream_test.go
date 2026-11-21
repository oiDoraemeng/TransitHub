package model_proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

func doraBufferedTestService(t *testing.T, handler http.HandlerFunc) (*Service, Route) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service, route := preparedProbeService(t, server)
	service.sites = proxyTestSites{site: upstream.Site{BaseURL: "https://www.dora2api.xyz"}}
	target, _ := url.Parse(server.URL)
	transport := server.Client().Transport
	service.dataClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		forward := r.Clone(r.Context())
		forward.URL.Scheme, forward.URL.Host = target.Scheme, target.Host
		return transport.RoundTrip(forward)
	})}
	route.ModelMappingEnabled = true
	route.ModelMapping = map[string]string{"alias": "gpt-6-luna"}
	return service, route
}

func TestDoraBufferedGenerationOutlivesStartupAndPreservesProtocol(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1beta/models/alias:generateContent"} {
		t.Run(path, func(t *testing.T) {
			service, route := doraBufferedTestService(t, func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				if payload["stream"] != true || payload["model"] != "gpt-6-luna" || r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("wire payload=%v accept=%s", payload, r.Header.Get("Accept"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if r.URL.Path == "/v1/responses" {
					io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"O\"}\n\n")
				} else {
					io.WriteString(w, "data: {\"id\":\"chat_test\",\"model\":\"gpt-6-luna\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"O\"}}]}\n\n")
				}
				w.(http.Flusher).Flush()
				select {
				case <-time.After(180 * time.Millisecond):
				case <-r.Context().Done():
					t.Error("active generation canceled by startup timer")
					return
				}
				if r.URL.Path == "/v1/responses" {
					io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"gpt-6-luna\",\"status\":\"completed\",\"reasoning\":{\"effort\":\"max\"},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n")
				} else {
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"K\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
				}
			})
			raw := `{"model":"alias","messages":[{"role":"user","content":"hello"}],"input":"hello","stream":false,"reasoning_effort":"low"}`
			if strings.HasPrefix(path, "/v1beta") {
				raw = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
			}
			incoming := httptest.NewRequest(http.MethodPost, path, nil)
			replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			mapping, err := newMappedResponseContext(incoming, replay, "alias", route.ModelMapping)
			if err != nil {
				t.Fatal(err)
			}
			response, _, err := service.performAttemptWithResponseHeaderTimeout(incoming, route, replay.Open, route.ModelMapping, mapping, true, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || !json.Valid(data) || isEventStream(response.Header) || !bytes.Contains(data, []byte("OK")) {
				t.Fatalf("response=%s err=%v", data, err)
			}
			if startup := responseHealth(response).startupLatency; startup <= 0 || startup >= 100*time.Millisecond {
				t.Fatalf("startup includes generation: %s", startup)
			}
			if !strings.HasPrefix(path, "/v1beta") {
				var result map[string]any
				json.Unmarshal(data, &result)
				if result["model"] != "alias" || result["usage"].(map[string]any)["total_tokens"] != float64(12) {
					t.Fatalf("lost metadata: %s", data)
				}
			}
			if path == "/v1/responses" && bytes.Contains(data, []byte(`"max"`)) {
				t.Fatalf("injected max leaked: %s", data)
			}
		})
	}
}

func TestDoraBufferedStartupTimeoutAndClientCancellation(t *testing.T) {
	for _, cancelAfterStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_output", true: "cancel_generation"}[cancelAfterStart], func(t *testing.T) {
			started := make(chan struct{})
			service, route := doraBufferedTestService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if cancelAfterStart {
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
				} else {
					io.WriteString(w, ": heartbeat\n\n")
				}
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelAfterStart {
				go func() { <-started; time.Sleep(20 * time.Millisecond); cancel() }()
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			response, _, err := service.performAttemptWithResponseHeaderTimeout(request, route, func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(`{"model":"gpt-6-luna","stream":false}`)), nil
			}, nil, mappedResponseContext{}, true, 60*time.Millisecond)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("stalled/truncated response accepted")
			}
			if !cancelAfterStart && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout=%v", err)
			}
		})
	}
}

func TestBufferedChatToolsUsageAndMultipleChoices(t *testing.T) {
	stream := `data: {"id":"chat_1","model":"gpt-6-luna","created":9007199254740993,"choices":[{"index":1,"delta":{"role":"assistant","content":"B"}},{"index":0,"delta":{"reasoning_content":"think","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"ing","tool_calls":[{"index":0,"function":{"arguments":"\"Tokyo\"}"}}]},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\ndata: [DONE]\n\n"
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	trackResponseHealth(response)
	if err := bufferDoraStream(response, false); err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	var result map[string]any
	json.Unmarshal(data, &result)
	choices := result["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	tool := message["tool_calls"].([]any)[0].(map[string]any)
	if result["object"] != "chat.completion" || message["reasoning_content"] != "thinking" || tool["function"].(map[string]any)["arguments"] != `{"city":"Tokyo"}` || bytes.Contains(data, []byte("9007199254740992")) || !bytes.Contains(data, []byte("cached_tokens")) {
		t.Fatalf("aggregation lost content/precision: %s", data)
	}
}

func TestBufferedStreamsRejectPartialAndFailedSuccessBodies(t *testing.T) {
	for _, tc := range []struct {
		stream    string
		responses bool
	}{
		{"data: [DONE]\n\n", true},
		{"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"overloaded\"}}}\n\n", true},
		{"data: {\"error\":{\"code\":\"model_not_found\"}}\n\n", false},
		{"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n", false},
		{"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n", false},
	} {
		response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.stream))}
		trackResponseHealth(response)
		if err := bufferDoraStream(response, tc.responses); err == nil {
			t.Fatalf("accepted failed/truncated stream %s", tc.stream)
		}
	}
}

func TestDoraBufferedPolicyScope(t *testing.T) {
	for _, tc := range []struct {
		base, path, fields string
		buffered           bool
	}{
		{"https://dora2api.xyz", "/v1/chat/completions", ``, true},
		{"https://dora2api.xyz", "/v1/responses", `,"stream":false`, true},
		{"https://dora2api.xyz", "/v1/responses", `,"stream":true`, false},
		{"https://dora2api.xyz", "/v1/responses", `,"background":true`, false},
		{"https://dora2api.xyz", "/v1/chat/completions", `,"modalities":["audio"]`, false},
		{"https://other.example", "/v1/chat/completions", ``, false},
		{"https://dora2api.xyz", "/v1/responses/compact", ``, false},
	} {
		body, mapping, err := forceDoraLunaReasoning(tc.base, tc.path, io.NopCloser(strings.NewReader(`{"model":"gpt-6-luna"`+tc.fields+`}`)), mappedResponseContext{})
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
		if mapping.BufferedStream != tc.buffered {
			t.Fatalf("scope %+v mapping=%+v", tc, mapping)
		}
	}
}
