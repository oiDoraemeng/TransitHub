package model_proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const offloadPDFRequest = `{"model":"alias","max_tokens":300,"system":"Read the attachment.","messages":[{"role":"user","content":[{"type":"text","text":"Report the circle color."},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}]}]}`
const offloadTextResponse = `{"id":"resp_test","model":"gpt-5.4","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Blue."}]}],"usage":{"input_tokens":100,"output_tokens":6,"input_tokens_details":{"cached_tokens":30}}}`

func TestMappedPDFOnWireAndRestoredMessages(t *testing.T) {
	for _, clientStream := range []bool{false, true} {
		for _, upstreamStream := range []bool{false, true} {
			t.Run(map[bool]string{false: "json", true: "stream"}[clientStream]+"/"+map[bool]string{false: "json", true: "stream"}[upstreamStream], func(t *testing.T) {
				server := httptest.NewServer(http.NotFoundHandler())
				defer server.Close()
				svc, route := preparedProbeService(t, server)
				route.ModelMappingEnabled, route.ModelMapping = true, map[string]string{"alias": "gpt-5.4"}
				route.RequestMatchMode, route.RequestMatchCapabilities = KeywordMatchModeRequireMatch, []string{"pdf"}
				svc.dataClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path != "/v1/responses" || req.Header.Get("Authorization") != "Bearer sk-probe-test" || req.Header.Get("Accept") != "text/event-stream" {
						t.Fatalf("request=%s headers=%v", req.URL.Path, req.Header)
					}
					var sent map[string]any
					if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
						t.Fatal(err)
					}
					if sent["model"] != "gpt-5.4" || sent["stream"] != true || sent["store"] != false || sent["instructions"] != "Read the attachment." {
						t.Fatalf("sent=%v", sent)
					}
					parts := sent["input"].([]any)[0].(map[string]any)["content"].([]any)
					pdf := parts[1].(map[string]any)
					if pdf["type"] != "input_file" || pdf["file_data"] != "data:application/pdf;base64,JVBERg==" {
						t.Fatalf("PDF lost: %v", pdf)
					}
					body, mime := offloadTextResponse, "application/json"
					if upstreamStream {
						body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
						mime = "text/event-stream"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				raw := strings.TrimSuffix(offloadPDFRequest, "}") + `,"stream":` + map[bool]string{false: "false", true: "true"}[clientStream] + `}`
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
				if err != nil {
					t.Fatal(err)
				}
				defer replay.Close()
				incoming := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				mapping, err := newMappedResponseContext(incoming, replay, "alias", route.ModelMapping)
				if err != nil {
					t.Fatal(err)
				}
				response, retry, err := svc.performAttemptWithResponseHeaderTimeout(incoming, route, replay.Open, route.ModelMapping, mapping, !clientStream, time.Second)
				if err != nil || retry {
					t.Fatalf("attempt err=%v retry=%v", err, retry)
				}
				encoded, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || !bytes.Contains(encoded, []byte(`"model":"alias"`)) || !bytes.Contains(encoded, []byte("Blue.")) || bytes.Contains(encoded, []byte(`"model":"gpt-5.4"`)) {
					t.Fatalf("response=%s err=%v", encoded, err)
				}
				if isEventStream(response.Header) != clientStream {
					t.Fatalf("client streaming changed: %v", response.Header)
				}
				if clientStream {
					if !bytes.Contains(encoded, []byte("event: message_stop")) || !bytes.Contains(encoded, []byte("event: content_block_delta")) {
						t.Fatalf("incomplete Claude stream: %s", encoded)
					}
				} else {
					var message map[string]any
					if err := json.Unmarshal(encoded, &message); err != nil {
						t.Fatal(err)
					}
					usage := message["usage"].(map[string]any)
					if usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(30) || usage["output_tokens"] != float64(6) {
						t.Fatalf("usage=%v", usage)
					}
				}
				original, _ := replay.Open()
				defer original.Close()
				unchanged, _ := io.ReadAll(original)
				if string(unchanged) != raw {
					t.Fatal("retry replay was modified")
				}
			})
		}
	}
}

func TestMappedExecutionRequiresRealUpstreamLogs(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		rejected   bool
	}{
		{"text alone", offloadTextResponse, true},
		{"no outputs", `{"id":"r","status":"completed","output":[{"type":"code_interpreter_call","status":"completed","outputs":null}]}`, true},
		{"failed call", `{"id":"r","status":"completed","output":[{"type":"code_interpreter_call","status":"failed","outputs":[{"type":"logs","logs":"error"}]}]}`, true},
		{"actual logs", `{"id":"r","status":"completed","output":[{"type":"code_interpreter_call","id":"ci_1","code":"print(84*72)","status":"completed","outputs":[{"type":"logs","logs":"6048\n"}]},{"type":"message","content":[{"type":"output_text","text":"6048"}]}]}`, false},
		{"unsupported files", `{"id":"r","status":"completed","output":[{"type":"code_interpreter_call","status":"completed","outputs":[{"type":"image","url":"https://example.test/result.png"}]}]}`, true},
		{"truncated SSE", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"6048\"}\n\n", true},
		{"in-band error", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"failed\"}}}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mime := "application/json"
			if strings.HasPrefix(tc.body, "data:") {
				mime = "text/event-stream"
			}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			err := offloadResponseToMessages(response, "alias", false, true)
			if (err != nil) != tc.rejected {
				t.Fatalf("error=%v rejected=%v", err, tc.rejected)
			}
			if err == nil {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if !bytes.Contains(body, []byte("6048")) || bytes.Contains(body, []byte("return_code")) || bytes.Contains(body, []byte("code_execution_result")) {
					t.Fatalf("lost logs or invented native process result: %s", body)
				}
			}
		})
	}
}

func TestMappedToolConversationAndPDFURL(t *testing.T) {
	var source map[string]any
	raw := `{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"key":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"value"}]}],"tools":[{"type":"code_execution_20250522","name":"code_execution"},{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"code_execution"}}`
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	converted, execution, err := messagesToResponses(source)
	if err != nil || !execution {
		t.Fatalf("conversion %v execution=%v", err, execution)
	}
	encoded, _ := json.Marshal(converted)
	for _, part := range []string{`"type":"input_file"`, `"file_url":"https://example.test/a.pdf"`, `"type":"function_call"`, `"call_id":"call_1"`, `"type":"function_call_output"`, `"type":"code_interpreter"`, `"code_interpreter_call.outputs"`} {
		if !bytes.Contains(encoded, []byte(part)) {
			t.Fatalf("missing %s in %s", part, encoded)
		}
	}
	for _, content := range []any{float64(1), map[string]any{"text": "invalid"}, []any{map[string]any{"type": "document", "source": map[string]any{"type": "file", "file_id": "file-remote"}}}} {
		source["messages"] = []any{map[string]any{"role": "user", "content": content}}
		if _, _, err := messagesToResponses(source); err == nil {
			t.Fatal("silently dropped unsupported content")
		}
	}
}

func TestOffloadAdapterPreservesNonGPTTargets(t *testing.T) {
	for _, route := range []Route{
		{},
		{ModelMappingEnabled: true},
		{ModelMappingEnabled: true, RequestMatchMode: KeywordMatchModeSkipOnMatch, RequestMatchCapabilities: []string{"pdf"}},
		{ModelMappingEnabled: true, RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: []string{"code_execution"}},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		converted, err := adaptMemberUpstreamProtocol(request, io.NopCloser(strings.NewReader(offloadPDFRequest)), route)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(converted.body)
		converted.body.Close()
		if converted.requestURI != "/v1/messages" || string(body) != offloadPDFRequest || converted.responseAdapter != nil {
			t.Fatal("adapted a request without a GPT target")
		}
	}
}
