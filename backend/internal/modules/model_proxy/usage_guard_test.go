package model_proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func usageTestGuard(t *testing.T, path, raw string) *claudeUsageGuard {
	t.Helper()
	body, guard, err := prepareClaudeUsageGuard(path, io.NopCloser(strings.NewReader(raw)), "route-test", "request-test")
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(preserved) != raw {
		t.Fatalf("request changed: %v", err)
	}
	return guard
}

func TestClaudeUsageGuardIncidentAndProtocolTotals(t *testing.T) {
	// A 151K-token ASCII context using four bytes/token still cannot explain
	// 1,695,788 input tokens, even under the deliberately generous byte bound.
	raw := `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"` + strings.Repeat("word", 151000) + `"}]}`
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			g := usageTestGuard(t, path, raw)
			usage := `{"usage":{"prompt_tokens":1695788,"prompt_tokens_details":{"cached_tokens":1246381,"cache_creation_tokens":401715}}}`
			if path == "/v1/messages" {
				usage = `{"usage":{"input_tokens":47692,"cache_read_input_tokens":1246381,"cache_creation_input_tokens":401715}}`
			}
			if !errors.Is(g.inspect([]byte(usage)), errUpstreamUsageAnomaly) {
				t.Fatal("incident usage was accepted")
			}
		})
	}
	// Cache details are subsets of Chat prompt_tokens, not additional tokens.
	g := usageTestGuard(t, "/v1/chat/completions", raw)
	if err := g.inspect([]byte(`{"usage":{"prompt_tokens":1000000,"prompt_tokens_details":{"cached_tokens":900000,"cache_creation_tokens":100000}}}`)); err != nil {
		t.Fatalf("double-counted Chat cache: %v", err)
	}
}

func TestClaudeUsageGuardSkipsUnboundedInputs(t *testing.T) {
	for _, extra := range []string{
		`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]`,
		`"messages":[{"role":"user","content":[{"type":"document","source":{"file_id":"file-1"}}]}]`,
		`"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]`,
		`"messages":[],"mcp_servers":[{"url":"https://example.com"}]`,
		`"messages":[],"container":"session-1"`,
		`"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image_url","image_url":{"url":"https://example.com"}}]}]}]`,
	} {
		if g := usageTestGuard(t, "/v1/messages", `{"model":"claude-sonnet-5-5",`+extra+`}`); g != nil {
			t.Fatalf("bounded remote/stateful input: %s", extra)
		}
	}
}

func TestClaudeUsageGuardKeepsSnapshotsAndBytes(t *testing.T) {
	g := usageTestGuard(t, "/v1/messages", `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}]}`)
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000,\"cache_creation_input_tokens\":40000,\"cache_read_input_tokens\":20000}}}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"partial_json\":\"{\\\"id\\\":9007199254740993}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\ndata: \"usage\":{\"input_tokens\":1000,\"cache_creation_input_tokens\":40000,\"cache_read_input_tokens\":20000}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"cache_creation_input_tokens\":0}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}" // Complete final event at EOF.
	r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := g.attach(r); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	buffer := make([]byte, 7)
	_, err := io.CopyBuffer(&got, r.Body, buffer)
	r.Body.Close()
	if err != nil || got.String() != stream || g.cacheWrite != 0 || g.cacheRead != 20000 || g.input != 1000 {
		t.Fatalf("stream/snapshots changed: err=%v write=%d read=%d input=%d", err, g.cacheWrite, g.cacheRead, g.input)
	}
}

func TestClaudeUsageGuardLateStreamFailsExplicitly(t *testing.T) {
	g := usageTestGuard(t, "/v1/chat/completions", `{"model":"claude-sonnet-5-5","messages":[]}`)
	prefix := "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"
	stream := prefix + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1695788}}\n\ndata: [DONE]\n\n"
	r := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := g.attach(r); err != nil {
		t.Fatal(err)
	}
	trackResponseHealth(r)
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	if !errors.Is(err, errUpstreamUsageAnomaly) || !responseHealth(r).failed || !strings.HasPrefix(string(raw), prefix) || !strings.Contains(string(raw), `"code":"upstream_usage_anomaly"`) || strings.Contains(string(raw), "1695788") || strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("anomalous stream completed normally: %s %v", raw, err)
	}
}

func TestClaudeUsageGuardRejectsBeforeHeadersAndNoRepayment(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse-start"}[stream], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1695788}}}\n\n")
				} else {
					io.WriteString(w, `{"type":"message","usage":{"input_tokens":1695788}}`)
				}
			}))
			defer server.Close()
			service, route := preparedProbeService(t, server)
			raw := `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hello"}]}`
			incoming := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			_, err := attemptSmartGroupMember(context.Background(), func(timeout time.Duration) (*http.Response, error) {
				response, _, err := service.performAttemptWithResponseHeaderTimeout(incoming, route, func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(raw)), nil }, nil, mappedResponseContext{}, false, timeout)
				return response, err
			})
			if !errors.Is(err, errUpstreamUsageAnomaly) || calls != 1 {
				t.Fatalf("repaid anomalous route: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestClaudeUsageGuardPreservesJSONAndRejectsOverflow(t *testing.T) {
	g := usageTestGuard(t, "/v1/messages", `{"model":"claude-sonnet-5-5","messages":[]}`)
	raw := `{"content":[{"type":"tool_use","input":{"id":9007199254740993}}],"usage":{"input_tokens":100,"cache_read_input_tokens":10000,"cache_creation_input_tokens":15000,"cache_creation":{"ephemeral_5m_input_tokens":15000}}}`
	r := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
	if err := g.attach(r); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || string(got) != raw {
		t.Fatalf("JSON changed: %s %v", got, err)
	}
	g = usageTestGuard(t, "/v1/messages", `{"model":"claude-sonnet-5-5","messages":[]}`)
	if err := g.inspect([]byte(`{"usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":9223372036854775807}}`)); !errors.Is(err, errUpstreamUsageAnomaly) {
		t.Fatalf("overflow accepted: %v", err)
	}
}
