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

func TestDoraLunaForceMaxAndRestoreClientMetadata(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, fields := range []string{``, `,"reasoning":{"effort":"low","summary":"auto"}`, `,"reasoning_effort":"high"`, `,"reasoning":{"effort":null}`, `,"reasoning":{"effort":"none"}`, `,"reasoning":{"effort":"low"},"reasoning_effort":"high"`} {
			t.Run(path+fields, func(t *testing.T) {
				raw := `{"model":"gpt-6-sol","input":"hello"` + fields + `}`
				incoming := httptest.NewRequest(http.MethodPost, path, nil)
				replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
				if err != nil {
					t.Fatal(err)
				}
				defer replay.Close()
				for attempt := 0; attempt < 2; attempt++ {
					mapping, err := newMappedResponseContext(incoming, replay, "gpt-6-sol", map[string]string{"gpt-6-sol": "gpt-6-luna"})
					if err != nil {
						t.Fatal(err)
					}
					body, _ := replay.Open()
					_, body, err = applyModelMapping(incoming, body, true, map[string]string{"gpt-6-sol": "gpt-6-luna"})
					if err != nil {
						t.Fatal(err)
					}
					body, mapping, err = forceDoraLunaReasoning("https://www.dora2api.xyz", path, body, mapping)
					if err != nil {
						t.Fatal(err)
					}
					var sent map[string]any
					if err := json.NewDecoder(body).Decode(&sent); err != nil {
						t.Fatal(err)
					}
					body.Close()
					if path == "/v1/responses" {
						if sent["reasoning"].(map[string]any)["effort"] != "max" || sent["reasoning_effort"] != nil {
							t.Fatalf("sent=%v", sent)
						}
					} else if sent["reasoning_effort"] != "max" {
						t.Fatalf("sent=%v", sent)
					}
					if sent["model"] != "gpt-6-luna" {
						t.Fatalf("mapping lost: %v", sent)
					}
					for _, stream := range []bool{false, true} {
						payload := `{"model":"gpt-6-luna","reasoning":{"effort":"max","summary":"auto"},"reasoning_effort":"max"}`
						contentType := "application/json"
						if stream {
							contentType = "text/event-stream"
							payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\ndata: [DONE]\n\n"
						}
						response := &http.Response{Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}
						if err := rewriteMappedResponse(response, mapping); err != nil {
							t.Fatal(err)
						}
						encoded, err := io.ReadAll(response.Body)
						response.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						var got map[string]any
						if stream {
							line := bytes.Split(encoded, []byte("\n"))[0]
							if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &got); err != nil {
								t.Fatal(err)
							}
							got = got["response"].(map[string]any)
						} else if err := json.Unmarshal(encoded, &got); err != nil {
							t.Fatal(err)
						}
						if got["model"] != "gpt-6-sol" {
							t.Fatalf("client model=%v", got)
						}
						reasoning := got["reasoning"].(map[string]any)
						if reasoning["summary"] != "auto" {
							t.Fatalf("summary lost: %v", got)
						}
						if mapping.ClientEffort.Present {
							if reasoning["effort"] != mapping.ClientEffort.Value || got["reasoning_effort"] != mapping.ClientEffort.Value {
								t.Fatalf("client effort not restored: %v", got)
							}
						} else if _, exists := reasoning["effort"]; exists || got["reasoning_effort"] != nil {
							t.Fatalf("injected max leaked: %v", got)
						}
					}
					// Streaming deltas remain free of response metadata.
					line := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n")
					if got := rewriteMappedSSELine(line, mapping); !bytes.Equal(got, line) { // JSON field order may change.
						if bytes.Contains(got, []byte("effort")) || bytes.Contains(got, []byte("reasoning")) {
							t.Fatalf("delta polluted: %s", got)
						}
					}
				}
				body, _ := replay.Open()
				unchanged, _ := io.ReadAll(body)
				body.Close()
				if string(unchanged) != raw {
					t.Fatal("override mutated replay for retry/other routes")
				}
			})
		}
	}
}

func TestDoraLunaOverrideScopeAndDirectModel(t *testing.T) {
	for _, test := range []struct {
		base, path, model string
		force             bool
	}{
		{"https://dora2api.xyz", "/v1/responses?x=1", "gpt-6-luna", true},
		{"https://www.dora2api.xyz", "/v1/chat/completions", "gpt-5.6-luna", true},
		{"https://other.example", "/v1/responses", "gpt-6-luna", false},
		{"https://dora2api.xyz.evil.example", "/v1/responses", "gpt-6-luna", false},
		{"https://dora2api.xyz", "/v1/responses", "gpt-6-sol", false},
		{"https://dora2api.xyz", "/v1/responses/compact", "gpt-6-luna", false},
	} {
		raw := `{"model":"` + test.model + `","reasoning":{"effort":"low"}}`
		body, mapping, err := forceDoraLunaReasoning(test.base, test.path, io.NopCloser(strings.NewReader(raw)), mappedResponseContext{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := io.ReadAll(body)
		body.Close()
		if mapping.Enabled != test.force || test.force && !bytes.Contains(encoded, []byte("max")) || !test.force && string(encoded) != raw {
			t.Fatalf("scope=%+v body=%s mapping=%+v", test, encoded, mapping)
		}
	}
}

func TestDoraLunaOverridePreservesLargeToolArgumentIntegers(t *testing.T) {
	raw := `{"model":"gpt-6-luna","input":[{"type":"function_call_output","call_id":"x","output":{"id":9007199254740993}}]}`
	body, _, err := forceDoraLunaReasoning("https://dora2api.xyz", "/v1/responses", io.NopCloser(strings.NewReader(raw)), mappedResponseContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Contains(got, []byte("9007199254740993")) {
		t.Fatalf("tool argument changed: %s %v", got, err)
	}
}

func TestDoraLunaGeminiAliasForcesConvertedChat(t *testing.T) {
	incoming := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-alias:generateContent", nil)
	body := io.NopCloser(strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	incoming, body, err := applyModelMapping(incoming, body, true, map[string]string{"gemini-alias": "gpt-6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := adaptUpstreamProtocol(incoming, body)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err = forceDoraLunaReasoning("https://dora2api.xyz", request.requestURI, request.body, mappedResponseContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	var payload map[string]any
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "gpt-6-luna" || payload["reasoning_effort"] != "max" {
		t.Fatalf("converted=%v", payload)
	}
}

func TestDoraLunaPreparedAttemptAppliesPolicyToWireAndRestoresStream(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, mapped := range []bool{false, true} {
		service, route := preparedProbeService(t, server)
		service.sites = proxyTestSites{site: upstream.Site{BaseURL: "https://www.dora2api.xyz"}}
		route.ModelMappingEnabled, route.ModelMapping = mapped, map[string]string{"alias": "gpt-6-luna"}
		model := "gpt-6-luna"
		if mapped {
			model = "alias"
		}
		service.dataClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var sent map[string]any
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			if sent["model"] != "gpt-6-luna" || sent["reasoning"].(map[string]any)["effort"] != "max" {
				t.Fatalf("wire payload=%v", sent)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-luna\",\"reasoning\":{\"effort\":\"max\"}}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-luna\",\"status\":\"completed\",\"reasoning\":{\"effort\":\"max\"}}}\n\n"))}, nil
		})
		incoming := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		replay, err := newReplayBody(io.NopCloser(strings.NewReader(`{"model":"`+model+`","input":"hello","reasoning":{"effort":"low"},"stream":true}`)), 4096)
		if err != nil {
			t.Fatal(err)
		}
		mapping, err := newMappedResponseContext(incoming, replay, model, route.ModelMapping)
		if err != nil {
			t.Fatal(err)
		}
		response, retryable, err := service.performPreparedAttempt(incoming, route, replay.Open, route.ModelMapping, mapping, false, time.Second, nil)
		if err != nil || retryable {
			t.Fatalf("attempt: %v %v", retryable, err)
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		replay.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"max"`)) || bytes.Count(raw, []byte(`"effort":"low"`)) != 2 || bytes.Count(raw, []byte(`"model":"`+model+`"`)) != 2 {
			t.Fatalf("client stream=%s", raw)
		}
	}
}
