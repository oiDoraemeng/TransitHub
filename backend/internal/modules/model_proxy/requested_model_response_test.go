package model_proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The mock upstream deliberately returns a different snapshot name.
func TestRequestedModelInvariant(t *testing.T) {
	cases := []struct {
		name, path, body, ct, upstream, field string
		identity                              bool
	}{
		{"chat_json", "/v1/chat/completions", `{"model":"client-alias","messages":[{"role":"user","content":"hello"}]}`, "application/json", `{"model":"upstream-snapshot","choices":[]}`, "model", false},
		{"chat_sse", "/v1/chat/completions", `{"model":"client-alias","stream":true,"messages":[]}`, "text/event-stream", "data: {\"model\":\"upstream-snapshot\",\"choices\":[]}\n\n", "model", false},
		{"responses_json", "/v1/responses", `{"model":"client-alias","input":"hello"}`, "application/json", `{"model":"upstream-snapshot","output":[]}`, "model", false},
		{"responses_sse", "/v1/responses", `{"model":"client-alias","stream":true,"input":"hello"}`, "text/event-stream", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"upstream-snapshot\"}}\n\n", "response.model", false},
		{"anthropic_json", "/v1/messages", `{"model":"client-alias","max_tokens":32,"messages":[]}`, "application/json", `{"type":"message","model":"upstream-snapshot","content":[]}`, "model", false},
		{"anthropic_message_start", "/v1/messages", `{"model":"client-alias","stream":true,"max_tokens":32,"messages":[]}`, "text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"model\":\"upstream-snapshot\",\"content\":[]}}\n\n", "message.model", false},
		{"gemini_generate_via_chat", "/v1beta/models/client-alias:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "application/json", `{"model":"upstream-snapshot","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`, "modelVersion", false},
		{"gemini_native_sse", "/v1beta/models/client-alias:streamGenerateContent?alt=sse", `{"contents":[]}`, "text/event-stream", "data: {\"modelVersion\":\"upstream-snapshot\",\"candidates\":[]}\n\n", "modelVersion", false},
		{"gemini_native_pdf_json", "/v1beta/models/client-alias:generateContent", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0xLjQ="}},{"text":"hello"}]}]}`, "application/json", `{"modelVersion":"upstream-snapshot","candidates":[]}`, "modelVersion", false},
		{"identity_mapping_snapshot", "/v1/chat/completions", `{"model":"client-alias","messages":[]}`, "application/json", `{"model":"upstream-snapshot","choices":[]}`, "model", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "gemini-upstream"
			if tc.identity {
				target = "client-alias"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.upstream)
			}))
			defer upstream.Close()
			service, route := preparedProbeService(t, upstream)
			route.ModelMappingEnabled = true
			route.ModelMapping = map[string]string{"client-alias": target}
			request := httptest.NewRequest("POST", tc.path, nil)
			var input map[string]any
			if err := json.Unmarshal([]byte(tc.body), &input); err != nil {
				t.Fatal(err)
			}
			request, _ = withRequestMedia(request, input, "client-alias")
			replay, err := newReplayBody(io.NopCloser(strings.NewReader(tc.body)), 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			mapping, err := newMappedResponseContext(request, replay, "client-alias", route.ModelMapping)
			if err != nil {
				t.Fatal(err)
			}
			response, retry, err := service.performAttempt(request, route, replay.Open, route.ModelMapping, mapping, false)
			if err != nil || retry {
				t.Fatalf("attempt: %v retry=%v", err, retry)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			data := string(raw)
			if tc.ct == "text/event-stream" {
				for _, line := range strings.Split(data, "\n") {
					if strings.HasPrefix(line, "data:") {
						data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
						break
					}
				}
			}
			var output map[string]any
			if err := json.Unmarshal([]byte(data), &output); err != nil {
				t.Fatalf("decode %q: %v", data, err)
			}
			var got any = output
			for _, key := range strings.Split(tc.field, ".") {
				obj, ok := got.(map[string]any)
				if !ok {
					got = nil
					break
				}
				got = obj[key]
			}
			t.Logf("requested=client-alias target=%s returned=%v field=%s", target, got, tc.field)
			if got != "client-alias" {
				t.Errorf("requested-model invariant not satisfied: returned=%v", got)
			}
		})
	}
}

func TestRequestedModelWithoutMapping(t *testing.T) {
	for _, mode := range []string{"direct", "disabled", "identity", "missing-entry"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages", "/v1beta/models/client-alias"} {
			t.Run(mode+path, func(t *testing.T) {
				method := "POST"
				upstreamBody := `{"model":"different-snapshot","content":[]}`
				field, want := "model", "client-alias"
				if strings.HasPrefix(path, "/v1beta/") {
					method = "GET"
					field = "name"
					want = "models/client-alias"
					upstreamBody = `{"name":"models/different-snapshot","baseModelId":"different","displayName":"Different","version":"123"}`
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if method == "POST" {
						raw, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(raw, "model").String() != "client-alias" {
							t.Errorf("request model changed: %s", raw)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, upstreamBody)
				}))
				defer server.Close()
				s, route := preparedProbeService(t, server)
				var configured map[string]string
				switch mode {
				case "disabled":
					configured = map[string]string{"client-alias": "should-not-be-used"}
				case "identity":
					route.ModelMappingEnabled = true
					configured = map[string]string{"client-alias": "client-alias"}
				case "missing-entry":
					route.ModelMappingEnabled = true
					configured = map[string]string{"other": "other-target"}
				}
				raw := `{"model":"client-alias","messages":[],"input":"hello"}`
				request := httptest.NewRequest(method, path, nil)
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
				if err != nil {
					t.Fatal(err)
				}
				defer replay.Close()
				model, err := requestModelID(request, replay)
				if err != nil {
					t.Fatal(err)
				}
				ctx, err := newMappedResponseContext(request, replay, model, configured)
				if err != nil {
					t.Fatal(err)
				}
				response, retry, err := s.performAttempt(request, route, replay.Open, configured, ctx, false)
				if err != nil || retry {
					t.Fatalf("%v retry=%v", err, retry)
				}
				defer response.Body.Close()
				got, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(got, field).String() != want {
					t.Fatalf("response=%s", got)
				}
				if method == "GET" && (gjson.GetBytes(got, "baseModelId").String() != model || gjson.GetBytes(got, "displayName").String() != model || gjson.GetBytes(got, "version").String() != "123") {
					t.Fatalf("detail=%s", got)
				}
			})
		}
	}
}

func TestRequestedModelPreservesPayloadBytes(t *testing.T) {
	ctx := mappedResponseContext{Enabled: true, SourceModel: "client-alias"}
	raw := []byte(`{"model":"snapshot","content":[{"type":"tool_use","name":"lookup","input":{"model":"snapshot","number":9007199254740993,"exponent":1.2300e+9}},{"type":"thinking","thinking":"snapshot","signature":"a\\b\u0041"}],"usage":{"n":9007199254740997},"metadata":{"model":"snapshot"}}`)
	got, err := rewriteMappedJSON(raw, ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(raw, []byte(`"model":"snapshot"`), []byte(`"model":"client-alias"`), 1)
	if !bytes.Equal(got, want) {
		t.Fatalf("payload changed beyond metadata:\n%s\n%s", got, want)
	}
	for _, raw := range []string{
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"model\\\":\\\"snapshot\\\"}\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"model snapshot failed\"}}\n\n",
		"data: [DONE]\n\n",
	} {
		got, err := io.ReadAll(newMappedSSEBody(io.NopCloser(strings.NewReader(raw)), ctx))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != raw {
			t.Fatalf("SSE content changed: %s", got)
		}
	}
}

func TestRequestedModelGeminiJSONArray(t *testing.T) {
	raw := []byte(`[{"modelVersion":"snapshot","candidates":[{"content":{"parts":[{"text":"snapshot","functionCall":{"args":{"model":"snapshot","n":9007199254740993}}}]}}]},{"modelVersion":"another-snapshot","candidates":[]}]`)
	ctx := mappedResponseContext{Enabled: true, SourceModel: "gemini-alias"}
	response := &http.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}
	if err := rewriteMappedResponse(response, ctx); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range gjson.ParseBytes(got).Array() {
		if item.Get("modelVersion").String() != "gemini-alias" {
			t.Fatalf("array=%s", got)
		}
	}
	if !bytes.Contains(got, []byte(`"args":{"model":"snapshot","n":9007199254740993}`)) {
		t.Fatalf("tool arguments changed: %s", got)
	}
}

type interruptedModelStream struct{ sent bool }

func (r *interruptedModelStream) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, []byte("data: {\"model\":\"snapshot\"}")), io.ErrUnexpectedEOF
}
func (*interruptedModelStream) Close() error { return nil }

func TestRequestedModelSSEFramingAndErrors(t *testing.T) {
	ctx := mappedResponseContext{Enabled: true, SourceModel: "client-alias"}
	raw := "id: 123\r\nevent: message_start\r\ndata: {\"type\":\"message_start\",\r\ndata: \"message\":{\"model\":\"snapshot\",\"content\":[]}}\r\n\r\n: heartbeat\r\n\r\n"
	got, err := io.ReadAll(newMappedSSEBody(io.NopCloser(strings.NewReader(raw)), ctx))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"model":"client-alias"`)) || !bytes.Contains(got, []byte("id: 123\r\nevent: message_start\r\n")) || !bytes.HasSuffix(got, []byte(": heartbeat\r\n\r\n")) {
		t.Fatalf("SSE framing=%s", got)
	}
	got, err = io.ReadAll(newMappedSSEBody(&interruptedModelStream{}, ctx))
	if !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Contains(got, []byte("client-alias")) {
		t.Fatalf("read=%s err=%v", got, err)
	}
}
