package model_proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const capabilitySchemaRequest = `{"model":"gpt-6-luna","messages":[{"role":"user","content":"Return a number"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer","enum":[323]}},"required":["answer"],"additionalProperties":false}}}}`

func TestMappedStandaloneSchemaIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		rejected   bool
	}{
		{"valid", `{"answer":323}`, false},
		{"wrong value", `{"answer":324}`, true},
		{"extra field", `{"answer":323,"fake":true}`, true},
		{"prose", `The answer is 323.`, true},
		{"trailing JSON", `{"answer":323} {}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/messages", nil)
			route := Route{ModelMappingEnabled: true, RequestMatchMode: KeywordMatchModeRequireMatch, RequestMatchCapabilities: []string{"json_schema"}}
			adapted, err := adaptMemberUpstreamProtocol(request, io.NopCloser(strings.NewReader(capabilitySchemaRequest)), route)
			if err != nil {
				t.Fatal(err)
			}
			defer adapted.release()
			if adapted.requestURI != "/v1/responses" {
				t.Fatal("standalone schema was not converted")
			}
			adapted.body.Close()
			raw, _ := json.Marshal(map[string]any{"id": "r", "model": "gpt-6-luna", "status": "completed", "output": []any{map[string]any{"type": "message", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": tc.text}}}}})
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}
			err = adapted.responseAdapter(response)
			if (err != nil) != tc.rejected {
				t.Fatalf("err=%v rejected=%v", err, tc.rejected)
			}
			if err == nil {
				defer response.Body.Close()
				if response.Header.Get("X-TransitHub-Upstream-Model") != "gpt-6-luna" {
					t.Fatal("actual mapped model is not disclosed")
				}
			}
		})
	}
}

func TestMappedSchemaCannotLoadExternalResources(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(capabilitySchemaRequest), &payload); err != nil {
		t.Fatal(err)
	}
	format := payload["output_config"].(map[string]any)["format"].(map[string]any)
	for _, ref := range []string{"file:///etc/passwd", "http://127.0.0.1/schema", "https://example.test/schema"} {
		format["schema"] = map[string]any{"$ref": ref}
		if _, err := compileMappedOutputSchema(payload); err == nil {
			t.Fatalf("external ref accepted: %s", ref)
		}
	}
}

func TestMappedSearchPreservesRecordsWithoutNativeTokens(t *testing.T) {
	source := map[string]any{"model": "gpt-6-luna", "messages": []any{map[string]any{"role": "user", "content": "Find the official docs"}}, "tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 2, "allowed_domains": []string{"openai.com"}}}, "tool_choice": map[string]any{"type": "tool", "name": "web_search"}}
	converted, execution, err := messagesToResponses(source)
	if err != nil || execution {
		t.Fatalf("err=%v execution=%v", err, execution)
	}
	raw, _ := json.Marshal(converted)
	for _, part := range []string{`"type":"web_search"`, `"allowed_domains":["openai.com"]`, `"max_tool_calls":2`, `web_search_call.action.sources`} {
		if !strings.Contains(string(raw), part) {
			t.Fatalf("missing %s: %s", part, raw)
		}
	}
	for _, status := range []string{"completed", "failed"} {
		body := `{"id":"r","status":"completed","output":[{"type":"web_search_call","id":"ws_real","status":"` + status + `","action":{"type":"search","sources":[{"type":"url","url":"https://openai.com/docs"}]}},{"type":"message","content":[{"type":"output_text","text":"Official documentation: https://openai.com/docs"}]}]}`
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		err := offloadResponseToMessages(response, "gpt-6-luna", false, false)
		if status == "failed" {
			if err == nil {
				t.Fatal("failed search accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(out), "ws_real") || !strings.Contains(string(out), "https://openai.com/docs") {
			t.Fatalf("lost real search evidence: %s", out)
		}
		for _, invented := range []string{"encrypted_content", "encrypted_index", "server_tool_use"} {
			if strings.Contains(string(out), invented) {
				t.Fatalf("invented native data: %s", out)
			}
		}
	}
}

func TestMappedNativeStateIsRejectedAndSlotsAreReleased(t *testing.T) {
	var source map[string]any
	json.Unmarshal([]byte(capabilitySchemaRequest), &source)
	for _, history := range []any{
		map[string]any{"type": "thinking", "thinking": "reason", "signature": "native-signature"},
		map[string]any{"type": "web_search_tool_result", "content": []any{}},
	} {
		source["messages"] = []any{map[string]any{"role": "assistant", "content": []any{history}}}
		if _, _, err := messagesToResponses(source); err == nil {
			t.Fatal("opaque native history was dropped")
		}
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	svc, route := preparedProbeService(t, server)
	route.ModelMappingEnabled = true
	route.RequestMatchMode = KeywordMatchModeRequireMatch
	route.RequestMatchCapabilities = []string{"json_schema"}
	svc.dataClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("upstream disconnected") })
	for i := 0; i < 10; i++ {
		_, _, err := svc.performAttempt(httptest.NewRequest("POST", "/v1/messages", nil), route, func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(capabilitySchemaRequest)), nil }, nil, mappedResponseContext{}, false)
		if err == nil || strings.Contains(err.Error(), "adapter is busy") {
			t.Fatalf("slot leaked: %v", err)
		}
	}
	if len(mappedResponseSlots) != 0 {
		t.Fatal("mapped adapter slot leaked")
	}
}
