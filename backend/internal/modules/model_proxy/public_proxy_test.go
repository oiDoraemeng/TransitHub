package model_proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSupportedPublicEndpoint(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/v1/models", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/v1/messages", true},
		{http.MethodGet, "/v1beta/models", true},
		{http.MethodGet, "/v1beta/models/gemini-2.5-pro", true},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", true},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent", true},
		{http.MethodPost, "/v1beta/models", false},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro", false},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:delete", false},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro/other:generateContent", false},
		{http.MethodPost, "/v1beta/files", false},
		{http.MethodGet, "/v1/chat/completions", false},
		{http.MethodPost, "/v1/embeddings", false},
	}
	for _, test := range tests {
		if got := supportedPublicEndpoint(test.method, test.path); got != test.want {
			t.Errorf("supportedPublicEndpoint(%q, %q)=%v want %v", test.method, test.path, got, test.want)
		}
	}
}

func TestAdaptUpstreamProtocolConvertsGeminiGenerateContentToChat(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	adapted, err := adaptUpstreamProtocol(request, io.NopCloser(strings.NewReader(`{
		"systemInstruction":{"parts":[{"text":"Be concise"}]},
		"contents":[{"role":"user","parts":[{"text":"Hello"}]}],
		"generationConfig":{"temperature":0.2,"maxOutputTokens":128}
	}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.body.Close()
	if adapted.requestURI != "/v1/chat/completions" {
		t.Fatalf("request URI=%q", adapted.requestURI)
	}
	var payload map[string]any
	if err := json.NewDecoder(adapted.body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "gemini-2.5-pro" || payload["stream"] != false {
		t.Fatalf("converted payload=%#v", payload)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("converted messages=%#v", payload["messages"])
	}
	if adapted.responseAdapter == nil {
		t.Fatal("Gemini request must install response adapter")
	}
}

func TestAdaptUpstreamProtocolKeepsGeminiStreamingNative(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", nil)
	adapted, err := adaptUpstreamProtocol(request, io.NopCloser(strings.NewReader(`{"contents":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.body.Close()
	want := "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"
	if adapted.requestURI != want || adapted.responseAdapter != nil {
		t.Fatalf("adapted request URI=%q adapter=%v", adapted.requestURI, adapted.responseAdapter != nil)
	}
}

func TestAdaptUpstreamProtocolKeepsChatCompletionsForSub2API(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=true", nil)
	adapted, err := adaptUpstreamProtocol(request, io.NopCloser(strings.NewReader(`{"model":"gemini-2.5-pro","messages":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.body.Close()
	if adapted.requestURI != "/v1/chat/completions?trace=true" || adapted.responseAdapter != nil {
		t.Fatalf("adapted request URI=%q adapter=%v", adapted.requestURI, adapted.responseAdapter != nil)
	}
}

func TestChatCompletionsToGeminiResponse(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","model":"gemini-2.5-pro",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
		}`)),
	}
	if err := chatCompletionsToGemini(response, "fallback-model"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Usage struct {
			Total int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Candidates) != 1 || payload.Candidates[0].FinishReason != "STOP" || payload.Candidates[0].Content.Parts[0].Text != "Hello" {
		t.Fatalf("Gemini response=%#v", payload)
	}
	if payload.Usage.Total != 6 {
		t.Fatalf("total tokens=%d", payload.Usage.Total)
	}
}

func TestRequestModelIDSupportsGeminiPath(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	replay, err := newReplayBody(io.NopCloser(strings.NewReader(`{"contents":[]}`)), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	modelID, err := requestModelID(request, replay)
	if err != nil || modelID != "gemini-2.5-pro" {
		t.Fatalf("requestModelID()=%q err=%v", modelID, err)
	}
}

func TestPublicAPIKeySupportsNativeProtocolHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{name: "openai", header: http.Header{"Authorization": []string{"Bearer openai-key"}}, want: "openai-key"},
		{name: "anthropic", header: http.Header{"X-Api-Key": []string{"claude-key"}}, want: "claude-key"},
		{name: "gemini", header: http.Header{"X-Goog-Api-Key": []string{"gemini-key"}}, want: "gemini-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publicAPIKey(test.header); got != test.want {
				t.Fatalf("publicAPIKey()=%q want %q", got, test.want)
			}
		})
	}
}

func TestCopyRequestHeadersRemovesClientCredentials(t *testing.T) {
	source := http.Header{
		"Authorization":  []string{"Bearer client-key"},
		"X-Api-Key":      []string{"claude-key"},
		"X-Goog-Api-Key": []string{"gemini-key"},
		"Anthropic-Beta": []string{"feature"},
	}
	destination := make(http.Header)
	copyRequestHeaders(destination, source)
	if destination.Get("Authorization") != "" || destination.Get("X-Api-Key") != "" || destination.Get("X-Goog-Api-Key") != "" {
		t.Fatal("client credentials must not be forwarded upstream")
	}
	if got := destination.Get("Anthropic-Beta"); got != "feature" {
		t.Fatalf("protocol header=%q want feature", got)
	}
}

func TestWaitForProxyRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if waitForProxyRetry(ctx, 0) {
		t.Fatal("canceled request must not retry")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("canceled retry wait should return immediately")
	}
}

func TestReplayBodyMemoryAndModel(t *testing.T) {
	body, err := newReplayBody(io.NopCloser(strings.NewReader(`{"model":"gpt-test","input":"hello"}`)), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if body.file != nil {
		t.Fatal("small body should remain in memory")
	}
	model, err := body.ModelID()
	if err != nil || model != "gpt-test" {
		t.Fatalf("ModelID=%q err=%v", model, err)
	}
	for range 2 {
		reader, err := body.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(reader)
		reader.Close()
		if !strings.Contains(string(data), "hello") {
			t.Fatalf("replayed body = %q", data)
		}
	}
}

func TestReplayBodySpoolsAndLimits(t *testing.T) {
	large := `{"model":"gpt-test","input":"` + strings.Repeat("x", memoryReplayLimit+128) + `"}`
	body, err := newReplayBody(io.NopCloser(strings.NewReader(large)), int64(len(large)+1))
	if err != nil {
		t.Fatal(err)
	}
	if body.file == nil {
		t.Fatal("large body should use a temporary file")
	}
	name := body.file.Name()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newReplayBody(io.NopCloser(strings.NewReader("too large")), 3); err != errBodyTooLarge {
		t.Fatalf("size error=%v", err)
	}
	_ = name
}

func TestCleanupGateBodyDeletesBeforeFirstEventIsReleased(t *testing.T) {
	var cleanupCalls int
	body := newCleanupGateBody(
		io.NopCloser(strings.NewReader("data: first\n\n"+"data: second\n\n")),
		func() error {
			cleanupCalls++
			return nil
		},
	)

	first := make([]byte, len("data: first\n\n"))
	n, err := body.Read(first)
	if err != nil {
		t.Fatalf("first Read error: %v", err)
	}
	if string(first[:n]) != "data: first\n\n" {
		t.Fatalf("first event = %q", first[:n])
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls after first event = %d, want 1", cleanupCalls)
	}

	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("remaining Read error: %v", err)
	}
	if string(rest) != "data: second\n\n" {
		t.Fatalf("remaining events = %q", rest)
	}
}

func TestReadFirstSSEEventPreservesBufferedBytes(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("event: message\r\ndata: {}\r\n\r\nrest"))
	event, err := readFirstSSEEvent(reader)
	if err != nil {
		t.Fatalf("readFirstSSEEvent error: %v", err)
	}
	if string(event) != "event: message\r\ndata: {}\r\n\r\n" {
		t.Fatalf("event = %q", event)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("remaining reader error: %v", err)
	}
	if string(rest) != "rest" {
		t.Fatalf("remaining reader = %q", rest)
	}
}
