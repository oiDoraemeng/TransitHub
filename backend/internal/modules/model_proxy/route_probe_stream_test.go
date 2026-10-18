package model_proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamingRecoveryProbeProtocols(t *testing.T) {
	for _, test := range []struct {
		path, upstreamPath, stream string
	}{
		{"/v1/chat/completions", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"/v1/messages", "/v1/messages", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"/v1beta/models/model:generateContent", "/v1beta/models/model:streamGenerateContent", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}]}\n\n"},
		{"/v1beta/models/model:streamGenerateContent", "/v1beta/models/model:streamGenerateContent", "data: {\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}\n\n"},
	} {
		t.Run(test.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.upstreamPath || r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("probe request=%s accept=%s", r.URL, r.Header.Get("Accept"))
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if strings.HasPrefix(test.path, "/v1beta/") {
					if r.URL.Query().Get("alt") != "sse" {
						t.Error("Gemini probe did not request SSE")
					}
				} else if payload["stream"] != true {
					t.Errorf("streaming probe payload=%v", payload)
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				// Split every event across transport chunks to exercise incremental reads.
				for _, line := range strings.SplitAfter(test.stream, "\n") {
					_, _ = io.WriteString(w, line)
					w.(http.Flusher).Flush()
				}
			}))
			defer server.Close()
			service, route := preparedProbeService(t, server)
			if !service.performRouteProbe(context.Background(), route, routeProbeSpec{ModelID: "model", Path: test.path}) {
				t.Fatal("valid streaming probe rejected")
			}
		})
	}
}

func TestValidateRouteProbeStream(t *testing.T) {
	chat := "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"
	completed := "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, test := range []struct {
		name, path, stream string
		success            bool
	}{
		{"chat-complete", "/v1/chat/completions", chat + "data: [DONE]\n\n", true},
		{"chat-crlf", "/v1/chat/completions", strings.ReplaceAll(chat+"data: [DONE]\n\n", "\n", "\r\n"), true},
		{"chat-truncated", "/v1/chat/completions", chat, false},
		{"chat-done-only", "/v1/chat/completions", "data: [DONE]\n\n", false},
		{"chat-role-only", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n", false},
		{"chat-late-error", "/v1/chat/completions", chat + "data: {\"error\":{\"message\":\"quota\"}}\n\ndata: [DONE]\n\n", false},
		{"responses-complete", "/v1/responses", completed, true},
		{"responses-heartbeat", "/v1/responses", ": heartbeat\n\ndata: {\"type\":\"response.created\"}\n\n" + completed, true},
		{"responses-metadata-only", "/v1/responses", "data: {\"type\":\"response.created\"}\n\n", false},
		{"responses-completion-missing-output", "/v1/responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", false},
		{"responses-nested-error", "/v1/responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"error\":{\"message\":\"failed\"}}}\n\n", false},
		{"responses-token-cap", "/v1/responses", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", true},
		{"responses-content-filter", "/v1/responses", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"output\":[],\"incomplete_details\":{\"reason\":\"content_filter\"}}}\n\n", false},
		{"responses-failed", "/v1/responses", "data: {\"type\":\"response.failed\"}\n\n", false},
		{"messages-stop-only", "/v1/messages", "data: {\"type\":\"message_stop\"}\n\n", false},
		{"gemini-truncated", "/v1beta/models/model:generateContent", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]}}]}\n\n", false},
		{"gemini-blocked", "/v1beta/models/model:generateContent", "data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n\n", false},
		{"invalid-json", "/v1/responses", "data: not-json\n\n" + completed, false},
		{"unterminated-event", "/v1/responses", strings.TrimSuffix(completed, "\n\n"), false},
		{"oversized-event", "/v1/responses", "data: " + strings.Repeat("x", (1<<20)+1) + "\n\n" + completed, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if success := validateRouteProbeStream(strings.NewReader(test.stream), test.path); success != test.success {
				t.Fatalf("success=%v want %v", success, test.success)
			}
		})
	}
}

func TestStreamingRecoveryProbeCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": heartbeat\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if service.performRouteProbe(ctx, route, routeProbeSpec{ModelID: "model", Path: "/v1/responses"}) {
		t.Fatal("stalled stream restored scheduling")
	}
}

func TestNonStreamingProbeProtocolsRemainSupported(t *testing.T) {
	for _, path := range []string{"/v1/models", "/v1beta/models", "/v1/responses/compact"} {
		request, _, streaming, err := newStreamingRouteProbeRequest(context.Background(), routeProbeSpec{ModelID: "model", Path: path})
		if err != nil || streaming || request.Header.Get("Accept") == "text/event-stream" {
			t.Fatalf("non-streaming endpoint %s: streaming=%v err=%v", path, streaming, err)
		}
	}
}
