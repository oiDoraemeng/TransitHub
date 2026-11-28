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

const mappedChatPDF = `{"model":"alias","messages":[{"role":"system","content":"Read the attachment."},{"role":"user","content":[{"type":"text","text":"Report the circle color."},{"type":"file","file":{"filename":"sample.pdf","file_data":"data:application/pdf;base64,JVBERg=="}}]}]}`
const mappedChatCode = `{"model":"alias","messages":[{"role":"user","content":"Calculate 7*9"}],"tools":[{"type":"code_interpreter","container":{"type":"auto","memory_limit":"1g"}}],"tool_choice":{"type":"code_interpreter"}}`
const mappedChatExecuted = `{"id":"executed","model":"gpt-6-luna","status":"completed","output":[{"type":"code_interpreter_call","id":"ci_real","status":"completed","code":"print(7*9)","outputs":[{"type":"logs","logs":"63\n"}]},{"type":"message","content":[{"type":"output_text","text":"63"}]}],"usage":{"input_tokens":100,"output_tokens":6,"input_tokens_details":{"cached_tokens":30}}}`

func TestMappedChatCapabilitiesOnWireAndRestoredProtocol(t *testing.T) {
	for _, tc := range []struct{ name, request, response, answer string }{
		{"PDF", mappedChatPDF, offloadTextResponse, "Blue."},
		{"execution", mappedChatCode, mappedChatExecuted, "63"},
		{"schema", `{"model":"alias","messages":[{"role":"user","content":"Return a number"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"integer","enum":[323]}},"required":["answer"],"additionalProperties":false}}}}`, strings.Replace(offloadTextResponse, "Blue.", `{\"answer\":323}`, 1), `{"answer":323}`},
		{"search", `{"model":"alias","messages":[{"role":"user","content":"Find a source"}],"tools":[{"type":"web_search","filters":{"allowed_domains":["example.org"]}}],"tool_choice":{"type":"web_search"}}`, offloadTextResponse, "Blue."},
	} {
		for _, clientStream := range []bool{false, true} {
			for _, upstreamStream := range []bool{false, true} {
				name := tc.name + map[bool]string{false: "/json", true: "/stream"}[clientStream] + map[bool]string{false: "/upstream-json", true: "/upstream-stream"}[upstreamStream]
				t.Run(name, func(t *testing.T) {
					server := httptest.NewServer(http.NotFoundHandler())
					defer server.Close()
					svc, route := preparedProbeService(t, server)
					route.ModelMappingEnabled, route.ModelMapping = true, map[string]string{"alias": "gpt-6-luna"}
					route.RequestMatchMode = RequestMatchModeMatchOrFallback
					svc.dataClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if req.URL.Path != "/v1/responses" || req.Header.Get("Authorization") != "Bearer sk-probe-test" {
							t.Fatalf("unexpected mapped request: %s", req.URL.Path)
						}
						var sent map[string]any
						if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
							t.Fatal(err)
						}
						if sent["model"] != "gpt-6-luna" || sent["stream"] != true || sent["store"] != false {
							t.Fatalf("upstream envelope: %v", sent)
						}
						switch tc.name {
						case "PDF":
							parts := sent["input"].([]any)[0].(map[string]any)["content"].([]any)
							pdf := parts[1].(map[string]any)
							if pdf["type"] != "input_file" || pdf["file_data"] != "data:application/pdf;base64,JVBERg==" || sent["instructions"] != "Read the attachment." {
								t.Fatalf("PDF or system instruction lost: %v", sent)
							}
						case "execution":
							tool := sent["tools"].([]any)[0].(map[string]any)
							if tool["type"] != "code_interpreter" || tool["container"].(map[string]any)["memory_limit"] != "1g" || sent["tool_choice"].(map[string]any)["type"] != "code_interpreter" {
								t.Fatalf("server execution lost: %v", sent)
							}
						case "schema":
							if sent["text"].(map[string]any)["format"].(map[string]any)["strict"] != true {
								t.Fatalf("schema not enforced: %v", sent)
							}
						case "search":
							tool := sent["tools"].([]any)[0].(map[string]any)
							if tool["type"] != "web_search" || tool["filters"].(map[string]any)["allowed_domains"].([]any)[0] != "example.org" {
								t.Fatalf("search filter lost: %v", sent)
							}
						}
						body, mime := tc.response, "application/json"
						if upstreamStream {
							body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
							mime = "text/event-stream"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})
					raw := strings.TrimSuffix(tc.request, "}") + `,"stream":` + map[bool]string{false: "false", true: "true"}[clientStream] + `}`
					replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
					if err != nil {
						t.Fatal(err)
					}
					defer replay.Close()
					incoming := httptest.NewRequest("POST", "/v1/chat/completions", nil)
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
					if err != nil || isEventStream(response.Header) != clientStream || !bytes.Contains(encoded, []byte(`"model":"alias"`)) {
						t.Fatalf("response=%s err=%v", encoded, err)
					}
					var message map[string]any
					if clientStream {
						if !bytes.Contains(encoded, []byte("data: [DONE]\n\n")) || !bytes.Contains(encoded, []byte(`"finish_reason":"stop"`)) || bytes.Contains(encoded, []byte("event: message_")) {
							t.Fatalf("invalid Chat stream: %s", encoded)
						}
						for _, line := range strings.Split(string(encoded), "\n") {
							if !strings.HasPrefix(line, "data: {") {
								continue
							}
							var chunk map[string]any
							if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
								t.Fatal(err)
							}
							delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
							if _, ok := delta["content"]; ok {
								message = delta
							}
						}
					} else {
						var chat map[string]any
						if err := json.Unmarshal(encoded, &chat); err != nil {
							t.Fatal(err)
						}
						message = chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
						usage := chat["usage"].(map[string]any)
						if usage["prompt_tokens"] != float64(100) || usage["total_tokens"] != float64(106) {
							t.Fatalf("cache counted incorrectly: %v", usage)
						}
					}
					if !strings.Contains(firstString(message, "content"), tc.answer) || tc.name == "execution" && !bytes.Contains(encoded, []byte("ci_real")) {
						t.Fatalf("answer or real execution record lost: %s", encoded)
					}
				})
			}
		}
	}
}

func TestMappedChatToolHistoryPreservesArgumentsAndFunctionChoice(t *testing.T) {
	var source map[string]any
	if err := json.Unmarshal([]byte(`{"model":"gpt-6-luna","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"code_execution","arguments":"{\"id\":9007199254740993}"}}]},{"role":"tool","tool_call_id":"call1","content":"ok"}],"tools":[{"type":"function","function":{"name":"code_execution","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"code_execution"}}}`), &source); err != nil {
		t.Fatal(err)
	}
	messages, err := chatToMappedMessages(source)
	if err != nil {
		t.Fatal(err)
	}
	converted, execution, err := messagesToResponses(messages)
	if err != nil || execution || converted["tool_choice"].(map[string]any)["type"] != "function" {
		t.Fatalf("client function confused with server execution: %v %v", converted, err)
	}
	input := converted["input"].([]any)
	if input[0].(map[string]any)["arguments"] != `{"id":9007199254740993}` || input[1].(map[string]any)["call_id"] != "call1" || input[1].(map[string]any)["output"] != "ok" {
		t.Fatalf("tool history corrupted: %v", input)
	}
}

func TestMappedChatRejectsNontransferableStateAndUnsupportedOptions(t *testing.T) {
	for _, raw := range []string{
		`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_private"}}]}]}`,
		`{"n":2}`, `{"modalities":["audio"]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"arguments":"{} {}"}}]}]}`,
	} {
		var source map[string]any
		json.Unmarshal([]byte(raw), &source)
		if _, err := chatToMappedMessages(source); err == nil {
			t.Fatalf("unsupported request accepted: %s", raw)
		}
	}
	for _, container := range []string{`"cntr_private"`, `{"type":"auto","file_ids":["file_private"]}`} {
		var source map[string]any
		json.Unmarshal([]byte(`{"model":"gpt-6-luna","tools":[{"type":"code_interpreter","container":`+container+`}]}`), &source)
		messages, err := chatToMappedMessages(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := messagesToResponses(messages); err == nil {
			t.Fatal("private provider state was silently replaced")
		}
	}
}

func TestOrdinaryMappedChatBypassesCapabilityBufferLimit(t *testing.T) {
	raw := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"Hello"}]}`
	var payload map[string]any
	json.Unmarshal([]byte(raw), &payload)
	incoming, _ := withRequestMedia(httptest.NewRequest("POST", "/v1/chat/completions", nil), payload, "alias")
	route := portableFallbackRoute()
	route.ModelMapping = map[string]string{"alias": "gpt-6-luna"}
	count := 0
	for len(mappedResponseSlots) < cap(mappedResponseSlots) {
		mappedResponseSlots <- struct{}{}
		count++
	}
	defer func() {
		for i := 0; i < count; i++ {
			<-mappedResponseSlots
		}
	}()
	converted, err := adaptMemberUpstreamProtocol(incoming, io.NopCloser(strings.NewReader(raw)), route)
	if err != nil {
		t.Fatalf("ordinary Chat consumed a capability slot: %v", err)
	}
	defer converted.body.Close()
	if converted.responseAdapter != nil || converted.bufferResponse {
		t.Fatal("ordinary Chat unexpectedly uses capability response buffering")
	}
}
