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

	"transithub/backend/internal/modules/upstream"
)

func TestMemberReasoningEffortOnMappedWireAndRestoredResponse(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			for _, streaming := range []bool{false, true} {
				t.Run(endpoint+"/"+effort+"/"+map[bool]string{true: "stream", false: "json"}[streaming], func(t *testing.T) {
					server := httptest.NewServer(http.NotFoundHandler())
					defer server.Close()
					service, route := preparedProbeService(t, server)
					service.sites = proxyTestSites{site: upstream.Site{BaseURL: "https://www.dora2api.xyz"}}
					route.ModelMappingEnabled, route.ModelMapping = true, map[string]string{"alias": "gpt-6-luna"}
					route.ReasoningEffort = effort
					responseJSON := `{"id":"test","model":"gpt-6-luna","status":"completed","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"reasoning":{"effort":"` + effort + `"},"reasoning_effort":"` + effort + `"}`
					service.dataClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						var sent map[string]any
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							t.Fatal(err)
						}
						if sent["model"] != "gpt-6-luna" || sent["stream"] != true {
							t.Fatalf("upstream mapping / internal streaming lost: %v", sent)
						}
						if endpoint == "/v1/responses" {
							if sent["reasoning"].(map[string]any)["effort"] != effort || sent["reasoning_effort"] != nil {
								t.Fatalf("configured Responses effort not applied: %v", sent)
							}
						} else if sent["reasoning_effort"] != effort || sent["reasoning"].(map[string]any)["effort"] != effort {
							t.Fatalf("configured Chat effort not applied: %v", sent)
						}
						stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}],\"model\":\"gpt-6-luna\",\"reasoning\":{\"effort\":\"" + effort + "\"},\"reasoning_effort\":\"" + effort + "\"}\n\ndata: [DONE]\n\n"
						if endpoint == "/v1/responses" {
							stream = "data: {\"type\":\"response.completed\",\"response\":" + responseJSON + "}\n\n"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
					})
					requestJSON := `{"model":"alias","messages":[{"role":"user","content":"hello"}],"input":"hello","reasoning":{"effort":"minimal","summary":"auto"},"reasoning_effort":"minimal","stream":` + map[bool]string{true: "true", false: "false"}[streaming] + `}`
					incoming := httptest.NewRequest(http.MethodPost, endpoint, nil)
					replay, err := newReplayBody(io.NopCloser(strings.NewReader(requestJSON)), 4096)
					if err != nil {
						t.Fatal(err)
					}
					defer replay.Close()
					mapping, err := newMappedResponseContext(incoming, replay, "alias", route.ModelMapping)
					if err != nil {
						t.Fatal(err)
					}
					for attempt := 0; attempt < 2; attempt++ {
						response, _, err := service.performAttemptWithResponseHeaderTimeout(incoming, route, replay.Open, route.ModelMapping, mapping, !streaming, time.Second)
						if err != nil {
							t.Fatal(err)
						}
						raw, err := io.ReadAll(response.Body)
						response.Body.Close()
						if err != nil || !bytes.Contains(raw, []byte(`"model":"alias"`)) || !bytes.Contains(raw, []byte(`"effort":"minimal"`)) {
							t.Fatalf("client model / effort not restored: %s %v", raw, err)
						}
						if isEventStream(response.Header) != streaming {
							t.Fatalf("client streaming protocol changed: %v", response.Header)
						}
					}
					original, _ := replay.Open()
					defer original.Close()
					raw, _ := io.ReadAll(original)
					if string(raw) != requestJSON {
						t.Fatalf("override changed replay for another member: %s", raw)
					}
				})
			}
		}
	}
}

func TestExplicitReasoningEffortOnOtherOpenAICompatibleUpstream(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", compactPath} {
		raw := `{"model":"another-model","stream":false}`
		body, mapping, err := applyUpstreamReasoning("https://other.example", path, io.NopCloser(strings.NewReader(raw)), mappedResponseContext{}, "high")
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := io.ReadAll(body)
		body.Close()
		if path == compactPath {
			if string(encoded) != raw || mapping.Enabled {
				t.Fatalf("Compact changed: %s", encoded)
			}
			continue
		}
		if !mapping.Enabled || mapping.BufferedStream || !bytes.Contains(encoded, []byte(`"high"`)) || !bytes.Contains(encoded, []byte(`"stream":false`)) {
			t.Fatalf("explicit effort or protocol incorrect: %s %+v", encoded, mapping)
		}
		payload := map[string]any{"model": "another-model", "reasoning_effort": "high", "reasoning": map[string]any{"effort": "high"}}
		rewriteMappedResponseObject(payload, mapping, false)
		if payload["reasoning_effort"] != nil || payload["reasoning"] != nil {
			t.Fatalf("unrequested override leaked: %v", payload)
		}
	}
}
