package model_proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func assertReservedThinkingSignature(t *testing.T, value any) string {
	t.Helper()
	signature, ok := value.(string)
	if !ok || signature == "" || !slices.Contains(reservedThinkingSignatures[:], signature) {
		t.Fatalf("signature %v is not one of the reserved labels", value)
	}
	return signature
}

func assertMappedThinkingSignatures(t *testing.T, body []byte, stream bool) {
	t.Helper()
	var message map[string]any
	count := 0
	if stream {
		starts := 0
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event["type"] == "message_start" {
				message, _ = event["message"].(map[string]any)
			}
			if block, ok := event["content_block"].(map[string]any); ok && block["type"] == "thinking" {
				starts++
				if block["signature"] != "" {
					t.Fatal("SSE signature must be emitted in signature_delta")
				}
			}
			if delta, ok := event["delta"].(map[string]any); ok && delta["type"] == "signature_delta" {
				assertReservedThinkingSignature(t, delta["signature"])
				count++
			}
		}
		if starts != count {
			t.Fatalf("thinking starts=%d signature deltas=%d", starts, count)
		}
	} else {
		if err := json.Unmarshal(body, &message); err != nil {
			t.Fatal(err)
		}
		blocks, _ := message["content"].([]any)
		for _, entry := range blocks {
			block, _ := entry.(map[string]any)
			if block["type"] == "thinking" {
				assertReservedThinkingSignature(t, block["signature"])
				count++
			}
		}
	}
	if count == 0 {
		t.Fatal("mapped response is missing reserved thinking signatures")
	}
	metadata, _ := message["transithub"].(map[string]any)
	thinking, _ := metadata["thinking"].(map[string]any)
	if thinking["signature_status"] != "reserved" {
		t.Fatalf("incorrect signature metadata: %v", thinking)
	}
}

func TestReservedThinkingSignaturesAreUniqueAndRandomlySelected(t *testing.T) {
	if len(reservedThinkingSignatures) != 30 {
		t.Fatalf("reserved label count=%d want 30", len(reservedThinkingSignatures))
	}
	labels := map[string]bool{}
	for _, label := range reservedThinkingSignatures {
		if strings.TrimSpace(label) == "" || labels[label] {
			t.Fatalf("empty or duplicate reserved label %q", label)
		}
		labels[label] = true
	}
	seen := map[string]bool{}
	for range 256 {
		signature := assertReservedThinkingSignature(t, pickMappedThinkingSignature())
		seen[signature] = true
	}
	if len(seen) < 2 {
		t.Fatal("random selection always returned the same label")
	}
}

func TestMappedThinkingUsesReservedSignaturesWithAndWithoutSummary(t *testing.T) {
	for _, raw := range []string{mappedThinkingResponse, offloadTextResponse, `{"id":"empty","status":"completed","output":[{"type":"reasoning","summary":[]}]}`} {
		for _, stream := range []bool{false, true} {
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
			if err := offloadResponseToMessagesChecked(response, "gpt-6-luna", stream, false, nil, true); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			assertMappedThinkingSignatures(t, body, stream)
		}
	}
}

func TestNativeThinkingResponsesAreForwardedUnchanged(t *testing.T) {
	service := &Service{repository: &Repository{}} // No database connection is needed.
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		body := `{"content":[{"type":"thinking","thinking":"native summary","signature":"native-signature"},{"type":"tool_use","input":{"signature":"business-value"}}]}`
		if contentType == "text/event-stream" {
			body = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"native-signature\"}}\n\n"
		}
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
		recorder := httptest.NewRecorder()
		completed, err := service.writeUpstreamResponse(recorder, response)
		if err != nil || !completed || recorder.Body.String() != body || recorder.Header().Get("Content-Type") != contentType {
			t.Fatalf("native response changed: completed=%t err=%v body=%s", completed, err, recorder.Body.String())
		}
	}
}
