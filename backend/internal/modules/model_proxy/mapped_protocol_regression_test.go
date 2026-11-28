package model_proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesMappingUsesResponsesRegardlessOfSelectionPolicy(t *testing.T) {
	for _, policy := range []string{KeywordMatchModeDisabled, KeywordMatchModeSkipOnMatch, KeywordMatchModeRequireMatch} {
		for _, stream := range []bool{false, true} {
			t.Run(policy+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				server := httptest.NewServer(http.NotFoundHandler())
				defer server.Close()
				svc, route := preparedProbeService(t, server)
				route.ModelMappingEnabled = true
				route.ModelMapping = map[string]string{"claude-alias": "gpt-6-luna"}
				route.RequestMatchMode = policy
				route.RequestMatchCapabilities = []string{"web_search"}
				calls := 0
				svc.dataClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					var body map[string]any
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if req.URL.Path != "/v1/responses" || body["model"] != "gpt-6-luna" || body["stream"] != true {
						t.Fatalf("wrong upstream protocol: %s %v", req.URL.Path, body)
					}
					if req.Header.Get("Anthropic-Version") != "" || req.Header.Get("Anthropic-Beta") != "" || req.Header.Get("X-Client-Request-Id") != "keep-correlation" {
						t.Fatalf("wrong protocol headers: %v", req.Header)
					}
					result := strings.ReplaceAll(offloadTextResponse, "gpt-5.4", "gpt-6-luna")
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":" + result + "}\n\n"))}, nil
				})
				raw := `{"model":"claude-alias","max_tokens":100,"messages":[{"role":"user","content":"Name the color."}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
				if err != nil {
					t.Fatal(err)
				}
				defer replay.Close()
				incoming := httptest.NewRequest("POST", "/v1/messages", nil)
				incoming.Header.Set("Anthropic-Version", "2023-06-01")
				incoming.Header.Set("Anthropic-Beta", "example-beta")
				incoming.Header.Set("X-Client-Request-Id", "keep-correlation")
				mapping, err := newMappedResponseContext(incoming, replay, "claude-alias", route.ModelMapping)
				if err != nil {
					t.Fatal(err)
				}
				response, retry, err := svc.performAttempt(incoming, route, replay.Open, route.ModelMapping, mapping, false)
				if err != nil || retry || response == nil {
					t.Fatalf("mapped attempt failed: %v retry=%v", err, retry)
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if calls != 1 || isEventStream(response.Header) != stream || !strings.Contains(string(body), `"model":"claude-alias"`) || !strings.Contains(string(body), "Blue.") || response.Header.Get("X-TransitHub-Upstream-Model") != "gpt-6-luna" {
					t.Fatalf("incorrect mapped response: %s headers=%v calls=%d", body, response.Header, calls)
				}
			})
		}
	}
}

func TestDirectGPTMessagesUsesResponsesWithoutMemberPolicy(t *testing.T) {
	raw := `{"model":"gpt-6-luna","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	incoming, _ := withRequestMedia(httptest.NewRequest("POST", "/v1/messages", nil), nil, "gpt-6-luna")
	converted, err := adaptMemberUpstreamProtocol(incoming, io.NopCloser(strings.NewReader(raw)), Route{})
	if err != nil {
		t.Fatal(err)
	}
	defer converted.release()
	defer converted.body.Close()
	if converted.requestURI != "/v1/responses" || converted.responseAdapter == nil {
		t.Fatalf("direct GPT Messages was not converted: %s", converted.requestURI)
	}
}

func TestMappedNativeStateRejectedWithoutCapabilityPolicy(t *testing.T) {
	raw := `{"model":"gpt-6-luna","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"Hello"}]}`
	_, err := adaptMemberUpstreamProtocol(httptest.NewRequest("POST", "/v1/messages", nil), io.NopCloser(strings.NewReader(raw)), Route{ModelMappingEnabled: true})
	if err == nil || len(mappedResponseSlots) != 0 {
		t.Fatalf("native state accepted or slot leaked: %v", err)
	}
}

func TestMappedSchemaAllowsToolTurnAndPreservesArguments(t *testing.T) {
	var source map[string]any
	if err := json.Unmarshal([]byte(capabilitySchemaRequest), &source); err != nil {
		t.Fatal(err)
	}
	schema, err := compileMappedOutputSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"completed", "incomplete"} {
		raw := `{"id":"r","status":"` + status + `","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":9007199254740993}"}]}`
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
		err := offloadResponseToMessagesChecked(response, "gpt-6-luna", false, false, schema)
		if status == "incomplete" {
			if err == nil {
				t.Fatal("incomplete structured tool turn accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(body), `"stop_reason":"tool_use"`) || !strings.Contains(string(body), `"id":9007199254740993`) {
			t.Fatalf("tool turn was corrupted: %s", body)
		}
	}
}

func TestMappedSchemaKeepsContractVisibleWithoutExtraModelTurns(t *testing.T) {
	var source map[string]any
	if err := json.Unmarshal([]byte(capabilitySchemaRequest), &source); err != nil {
		t.Fatal(err)
	}
	source["system"] = "Preserve the original calculation instructions."
	converted, _, err := messagesToResponses(source)
	if err != nil {
		t.Fatal(err)
	}
	instructions, _ := converted["instructions"].(string)
	_, embedded, ok := strings.Cut(instructions, "JSON Schema:\n")
	if !ok || !strings.HasPrefix(instructions, source["system"].(string)) || !json.Valid([]byte(embedded)) {
		t.Fatalf("format contract or original instructions lost: %s", instructions)
	}
	format := converted["text"].(map[string]any)["format"].(map[string]any)
	schemaJSON, _ := json.Marshal(format["schema"])
	if embedded != string(schemaJSON) || format["strict"] != true {
		t.Fatal("instruction schema differs from strict output schema")
	}
	// This is still one request with the original single user turn. No repair
	// request or tool result with invented JSON data is introduced.
	if len(converted["input"].([]any)) != 1 {
		t.Fatal("format compatibility inserted extra conversation turns")
	}
}

func TestMappedSearchLocationAndUnsupportedBlockList(t *testing.T) {
	var source map[string]any
	raw := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"Search the docs"}],"tools":[{"type":"web_search_20250305","name":"web_search","user_location":{"city":"London","country":"GB"}}]}`
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	converted, _, err := messagesToResponses(source)
	if err != nil {
		t.Fatal(err)
	}
	tool := converted["tools"].([]any)[0].(map[string]any)
	location := tool["user_location"].(map[string]any)
	if location["type"] != "approximate" || location["city"] != "London" || location["country"] != "GB" {
		t.Fatalf("invalid Responses location: %v", location)
	}
	source["tools"].([]any)[0].(map[string]any)["blocked_domains"] = []any{"example.test"}
	if _, _, err := messagesToResponses(source); err == nil {
		t.Fatal("unsupported block list forwarded to OpenAI")
	}
}

func TestMappedPDFURLIsNotFilteredByProtocol(t *testing.T) {
	route := Route{ModelMappingEnabled: true, ModelMapping: map[string]string{"alias": "gpt-6-luna"}}
	media := requestMedia{Model: "alias", PDF: true, FileReference: true}
	if got := filterMediaRoutes([]Route{route}, media, httptest.NewRequest("POST", "/v1/messages", nil)); len(got) != 1 {
		t.Fatal("GPT PDF mapping was excluded before protocol conversion")
	}
}
