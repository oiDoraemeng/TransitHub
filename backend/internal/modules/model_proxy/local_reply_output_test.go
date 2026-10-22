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
	"testing"
)

func TestGroupLocalReplyExplicitOutputLimits(t *testing.T) {
	for _, test := range []struct {
		name, path, settings string
		threshold            int
		want                 bool
	}{
		{"chat-legacy", "/v1/chat/completions", `"max_tokens":20`, 20, true},
		{"chat-completion", "/v1/chat/completions", `"max_completion_tokens":20`, 20, true},
		{"chat-effective-limit", "/v1/chat/completions", `"max_completion_tokens":21,"max_tokens":20`, 20, false},
		{"responses", "/v1/responses", `"max_output_tokens":20`, 20, true},
		{"compact", compactPath, `"max_output_tokens":20`, 20, true},
		{"messages", "/v1/messages", `"max_tokens":20`, 20, true},
		{"gemini", "/v1beta/models/alias:generateContent", `"generationConfig":{"maxOutputTokens":20}`, 20, true},
		{"gemini-stream", "/v1beta/models/alias:streamGenerateContent", `"generation_config":{"max_output_tokens":20}`, 20, true},
		{"above", "/v1/responses", `"max_output_tokens":21`, 20, false},
		{"adjustable", "/v1/responses", `"max_output_tokens":30`, 30, true},
		{"disabled", "/v1/responses", `"max_output_tokens":20`, 0, false},
		{"missing", "/v1/responses", `"temperature":0`, 20, false},
		{"wrong-protocol-field", "/v1/responses", `"max_tokens":20`, 20, false},
		{"zero", "/v1/responses", `"max_output_tokens":0`, 20, false},
		{"negative", "/v1/responses", `"max_output_tokens":-1`, 20, false},
		{"fraction", "/v1/responses", `"max_output_tokens":1.5`, 20, false},
		{"string", "/v1/responses", `"max_output_tokens":"20"`, 20, false},
		{"null", "/v1/responses", `"max_output_tokens":null`, 20, false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				// The output rule is independent of long input, keyword, cache and
				// history exemptions that protect the small-input rule.
				body := fmt.Sprintf(`{"model":"alias","input":%q,"previous_response_id":"resp_history","prompt_cache_key":"cached","stream":%t,%s}`, strings.Repeat("premium long input ", 100), stream, test.settings)
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(body)), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				defer replay.Close()
				group := SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100, LocalReplyMaxOutputTokens: test.threshold, Keywords: []string{"premium"}}
				w := httptest.NewRecorder()
				local := (&Service{}).serveGroupLocalReply(w, httptest.NewRequest(http.MethodPost, test.path, nil), group, replay, "alias")
				if local != test.want || (!local && w.Body.Len() != 0) {
					t.Fatalf("local=%t want=%t body=%s", local, test.want, w.Body.String())
				}
				group.LocalReplyEnabled = false
				if (&Service{}).serveGroupLocalReply(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, test.path, nil), group, replay, "alias") {
					t.Fatal("disabled master rule still intercepted request")
				}
			})
		}
	}
}

func TestGroupLocalReplyCustomTextAcrossProtocols(t *testing.T) {
	text := "已收到 \"OK\"\n下一行 <测试>"
	encoded, _ := json.Marshal(text)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", compactPath, "/v1/messages", "/v1beta/models/alias:generateContent", "/v1beta/models/alias:streamGenerateContent"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", path, stream), func(t *testing.T) {
				w := httptest.NewRecorder()
				body := fmt.Sprintf(`{"model":"alias","input":"hello","stream":%t}`, stream)
				(&Service{maxRequestBytes: 1 << 20}).proxySmartGroup(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), SmartGroup{LocalReplyEnabled: true, LocalReplyMinTokens: 100, LocalReplyText: text})
				if w.Code != 200 || !strings.Contains(w.Body.String(), string(encoded)) || strings.Contains(w.Body.String(), `:"OK"`) {
					t.Fatalf("custom text corrupted: %d %s", w.Code, w.Body.String())
				}
				if w.Header().Get("Content-Type") == "text/event-stream" {
					if !validateRouteProbeStream(strings.NewReader(w.Body.String()), path) {
						t.Fatalf("incomplete custom reply stream: %s", w.Body.String())
					}
				} else if !json.Valid(w.Body.Bytes()) {
					t.Fatal("invalid JSON reply")
				}
			})
		}
	}
}

func TestLocalReplyOutputSettingsValidation(t *testing.T) {
	limit, text, err := localReplyOutputSettings(nil, nil)
	if err != nil || limit != 20 || text != "OK" {
		t.Fatalf("defaults=%d %q %v", limit, text, err)
	}
	for _, test := range []struct {
		limit int
		text  string
	}{{-1, "OK"}, {1000001, "OK"}, {20, ""}, {20, "   "}, {20, strings.Repeat("字", 4001)}} {
		for _, create := range []bool{false, true} {
			if create {
				_, _, err = (&Service{}).CreateSmartGroup(context.Background(), "user", CreateSmartGroupRequest{LocalReplyMaxOutputTokens: &test.limit, LocalReplyText: &test.text})
			} else {
				_, err = (&Service{}).UpdateSmartGroup(context.Background(), "user", "group", UpdateSmartGroupRequest{LocalReplyMaxOutputTokens: &test.limit, LocalReplyText: &test.text})
			}
			var request *requestError
			if !errors.As(err, &request) || request.Status != 400 {
				t.Fatalf("validation failed: %v", err)
			}
		}
	}
}
