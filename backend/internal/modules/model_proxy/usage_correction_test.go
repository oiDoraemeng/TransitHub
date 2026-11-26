package model_proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestUsageCorrectionHistoricalMessagesAndCacheBreakdown(t *testing.T) {
	c := &inputUsageCorrector{messages: true, policy: inputUsagePolicy{Estimate: 156000, Limit: 171600, AllowancePercent: 10}}
	original := `{"type":"message","usage":{"input_tokens":47692,"cache_creation_input_tokens":401715,"cache_read_input_tokens":1246381,"output_tokens":4972,"cache_creation":{"ephemeral_5m_input_tokens":301715,"ephemeral_1h_input_tokens":100000}},"content":[{"type":"tool_use","input":{"id":9007199254740993,"usage":{"input_tokens":99999999}},"signature":"untouched"}]}`
	got := c.rewrite([]byte(original))
	u := gjson.GetBytes(got, "usage")
	if u.Get("input_tokens").Int() != 47692 || u.Get("output_tokens").Int() != 4972 {
		t.Fatalf("ordinary input/output changed: %s", got)
	}
	if total := u.Get("input_tokens").Int() + u.Get("cache_creation_input_tokens").Int() + u.Get("cache_read_input_tokens").Int(); total != 171600 {
		t.Fatalf("total=%d", total)
	}
	if u.Get("cache_creation.ephemeral_5m_input_tokens").Int()+u.Get("cache_creation.ephemeral_1h_input_tokens").Int() != u.Get("cache_creation_input_tokens").Int() {
		t.Fatalf("TTL buckets inconsistent: %s", got)
	}
	if gjson.GetBytes(got, "content").Raw != gjson.Get(original, "content").Raw {
		t.Fatal("tool integer/signature/content mutated")
	}
	if c.raw.Input+c.raw.Read+c.raw.Write != 1695788 || c.corrected.Output != 4972 {
		t.Fatal("original accounting was lost")
	}
}

func TestUsageCorrectionChatAndResponsesNeverAddCacheTwice(t *testing.T) {
	for _, chat := range []bool{false, true} {
		input, output, details := "input_tokens", "output_tokens", "input_tokens_details"
		if chat {
			input, output, details = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
		}
		raw := fmt.Sprintf(`{"usage":{"%s":1000,"%s":7,"total_tokens":1007,"%s":{"cached_tokens":800,"cache_creation_tokens":100}}}`, input, output, details)
		under := &inputUsageCorrector{chat: chat, policy: inputUsagePolicy{Limit: 1100}}
		if got := under.rewrite([]byte(raw)); string(got) != raw {
			t.Fatalf("cache was double counted: %s", got)
		}
		over := &inputUsageCorrector{chat: chat, policy: inputUsagePolicy{Limit: 500}}
		got := over.rewrite([]byte(raw))
		if gjson.GetBytes(got, "usage."+input).Int() != 500 || gjson.GetBytes(got, "usage.total_tokens").Int() != 507 || gjson.GetBytes(got, "usage."+output).Int() != 7 {
			t.Fatalf("bad total/output: %s", got)
		}
		caches := gjson.GetBytes(got, "usage."+details+".cached_tokens").Int() + gjson.GetBytes(got, "usage."+details+".cache_creation_tokens").Int()
		if caches != 400 {
			t.Fatalf("ordinary input not preserved: %s", got)
		}
	}
}

func TestUsageCorrectionSSEPartialSnapshotsZerosAndFraming(t *testing.T) {
	stream := "id: first\r\nevent: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":1000,\"cache_read_input_tokens\":9000,\"output_tokens\":1}}}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"OK\",\"id\":9007199254740993}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\ndata: \"usage\":{\"cache_creation_input_tokens\":0,\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	if err := correctResponseUsage(response, inputUsagePolicy{Estimate: 500, Limit: 500}, "/v1/messages", "test"); err != nil {
		t.Fatal(err)
	}
	body := response.Body.(*correctedUsageBody)
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	if !bytes.Contains(got, []byte("id: first\r\nevent: message_start\r\n")) || !bytes.Contains(got, []byte(`"id":9007199254740993`)) || !bytes.Contains(got, []byte("event: message_stop\n")) {
		t.Fatalf("framing/content changed: %s", got)
	}
	c := body.corrector
	if c.raw.Input != 100 || c.raw.Read != 9000 || c.raw.Write != 0 || c.raw.Output != 7 {
		t.Fatalf("not cumulative snapshots: %+v", c.raw)
	}
	if c.corrected.Input+c.corrected.Read+c.corrected.Write != 500 || c.corrected.Write != 0 {
		t.Fatalf("zero/partial update ignored: %+v", c.corrected)
	}
	if c.peakOriginal.Write != 1000 || c.peakOriginal.Read != 9000 {
		t.Fatal("anomalous original snapshot lost")
	}
}

func TestUsageCorrectionResponsesNestedEventsAndChatPartialUsage(t *testing.T) {
	c := &inputUsageCorrector{policy: inputUsagePolicy{Limit: 100}}
	got := c.rewrite([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1000,"output_tokens":5,"total_tokens":1005,"input_tokens_details":{"cached_tokens":900}},"output":[{"arguments":"9007199254740993"}]}}`))
	if gjson.GetBytes(got, "response.usage.total_tokens").Int() != 105 || gjson.GetBytes(got, "response.output.0.arguments").String() != "9007199254740993" {
		t.Fatalf("nested response: %s", got)
	}
	c = &inputUsageCorrector{chat: true, policy: inputUsagePolicy{Limit: 500}}
	c.rewrite([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":800,"cache_creation_tokens":100}}}`))
	got = c.rewrite([]byte(`{"usage":{"completion_tokens":5,"total_tokens":1005}}`))
	if gjson.GetBytes(got, "usage.prompt_tokens").Int() != 500 || gjson.GetBytes(got, "usage.total_tokens").Int() != 505 || gjson.GetBytes(got, "usage.prompt_tokens_details.cache_creation_tokens").Int() != 44 {
		t.Fatalf("Chat partial snapshot lost cache style: %s", got)
	}
}

func TestUsageCorrectionSSETransitionBackBelowLimitRestoresAllBuckets(t *testing.T) {
	c := &inputUsageCorrector{messages: true, policy: inputUsagePolicy{Limit: 500}}
	c.rewrite([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":100,"cache_creation_input_tokens":9000}}}`))
	got := c.rewrite([]byte(`{"type":"message_delta","usage":{"cache_creation_input_tokens":0,"output_tokens":7}}`))
	if gjson.GetBytes(got, "usage.input_tokens").Int() != 100 || gjson.GetBytes(got, "usage.cache_read_input_tokens").Int() != 100 || gjson.GetBytes(got, "usage.cache_creation_input_tokens").Int() != 0 {
		t.Fatalf("unclipped snapshot left old corrected cache: %s", got)
	}
	c = &inputUsageCorrector{chat: true, policy: inputUsagePolicy{Limit: 100}}
	c.rewrite([]byte(`{"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":990}}}`))
	got = c.rewrite([]byte(`{"usage":{"prompt_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}}`))
	if gjson.GetBytes(got, "usage.prompt_tokens_details.cached_tokens").Int() != 0 || gjson.GetBytes(got, "usage.prompt_tokens").Int() != 0 {
		t.Fatalf("zero cache not cleared: %s", got)
	}
}

func TestUsageCorrectionRejectsUnsafeNumbersAndBoundsAllBuckets(t *testing.T) {
	for _, raw := range []string{`{"usage":{"input_tokens":-1}}`, `{"usage":{"input_tokens":1.5}}`, `{"usage":{"input_tokens":1e9}}`, `{"usage":{"input_tokens":9223372036854775808}}`, `{"usage":{"input_tokens":null}}`} {
		c := &inputUsageCorrector{policy: inputUsagePolicy{Limit: 10}}
		if got := c.rewrite([]byte(raw)); string(got) != raw {
			t.Fatalf("invalid usage rewritten: %s", got)
		}
	}
	for _, messages := range []bool{false, true} {
		for _, input := range []int64{0, 1, 99, 100, 101, 1000} {
			for _, read := range []int64{0, 3, 100, 2000} {
				for _, write := range []int64{0, 1, 500} {
					original := inputUsageSnapshot{Input: input, Read: read, Write: write, Output: 7}
					got, _ := capUsage(original, 100, messages)
					total := got.Input
					if messages {
						total += got.Read + got.Write
					}
					if total > 100 || got.Read+got.Write > total || got.Output != 7 || got.Input < 0 || got.Read < 0 || got.Write < 0 {
						t.Fatalf("invalid corrected buckets: %+v -> %+v", original, got)
					}
				}
			}
		}
	}
	if got := usageFraction(math.MaxInt64, 100, math.MaxInt64); got != 100 {
		t.Fatalf("overflow=%d", got)
	}
}

func TestUsageCorrectionDoraCacheWriteAliasStaysConsistent(t *testing.T) {
	c := &inputUsageCorrector{chat: true, policy: inputUsagePolicy{Limit: 500}}
	got := c.rewrite([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":800,"cache_write_tokens":100,"cache_creation_tokens":100}}}`))
	if gjson.GetBytes(got, "usage.prompt_tokens_details.cache_write_tokens").Int() != 44 || gjson.GetBytes(got, "usage.prompt_tokens_details.cache_creation_tokens").Int() != 44 || gjson.GetBytes(got, "usage.prompt_tokens_details.cached_tokens").Int() != 356 {
		t.Fatalf("alias mismatch: %s", got)
	}
	c = &inputUsageCorrector{chat: true, policy: inputUsagePolicy{Limit: 500}}
	got = c.rewrite([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":7,"cache_read_input_tokens":800,"cached_tokens":800,"cache_creation_input_tokens":100}}`))
	if gjson.GetBytes(got, "usage.cache_read_input_tokens").Int() != 356 || gjson.GetBytes(got, "usage.cached_tokens").Int() != 356 || gjson.GetBytes(got, "usage.cache_creation_input_tokens").Int() != 44 {
		t.Fatalf("top-level cache aliases not corrected: %s", got)
	}
}

func TestCorrectionEstimateFullHistoryToolsUnicodeAndOpaqueContext(t *testing.T) {
	parse := func(raw string) map[string]any {
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := parse(`{"messages":[{"role":"user","content":"hello"}]}`)
	n, known := estimateCorrectionInput(base)
	if !known || n <= 0 {
		t.Fatal("no baseline estimate")
	}
	full := parse(`{"system":"complete system","messages":[{"role":"user","content":"old history 中文中文中文"},{"role":"assistant","content":[{"type":"tool_use","name":"test","input":{"value":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","content":"tool output"}]}],"tools":[{"name":"test","input_schema":{"type":"object","properties":{"image":{"type":"string"}}}}]}`)
	total, known := estimateCorrectionInput(full)
	if !known || total <= n*2 {
		t.Fatalf("history/tools missing: %d %d %v", n, total, known)
	}
	for _, suffix := range []string{`,"previous_response_id":"r"`, `,"conversation":"c"`, `,"tools":[{"type":"web_search"}]`, `,"mcp_servers":[{}]`} {
		payload := parse(`{"input":"text"` + suffix + `}`)
		if _, known := estimateCorrectionInput(payload); known {
			t.Fatalf("opaque request accepted: %v", payload)
		}
	}
	for _, kind := range []string{"image", "document", "input_audio", "item_reference", "compaction", "server_tool_use"} {
		payload := parse(`{"messages":[{"content":[{"type":"` + kind + `"}]}]}`)
		if _, known := estimateCorrectionInput(payload); known {
			t.Fatalf("opaque %s accepted", kind)
		}
	}
	full["cache_control"] = map[string]any{"type": "ephemeral"}
	if _, known := estimateCorrectionInput(full); !known {
		t.Fatal("visible cached text incorrectly skipped")
	}
}

func TestUsageCorrectionPolicyIsOptInAndGroupScoped(t *testing.T) {
	s := &Service{}
	replay, err := newReplayBody(io.NopCloser(strings.NewReader(`{"messages":[{"content":"hello"}]}`)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	enabled := SmartGroup{ID: "a", UsageCorrectionEnabled: true}
	disabled := SmartGroup{ID: "b", UsageCorrectionAllowancePercent: 10}
	for _, percent := range []int{0, 10, 20, 37, 1000} {
		enabled.UsageCorrectionAllowancePercent = percent
		got := s.withUsageCorrection(r, replay, enabled)
		policy, ok := got.Context().Value(usageCorrectionContextKey{}).(inputUsagePolicy)
		if !ok || policy.Limit != (policy.Estimate*int64(100+percent)+99)/100 {
			t.Fatalf("percentage %d: %+v", percent, policy)
		}
		if s.withUsageCorrection(r, replay, disabled) != r {
			t.Fatal("another group was affected")
		}
	}
	enabled.UsageCorrectionEnabled = false
	if s.withUsageCorrection(r, replay, enabled) != r {
		t.Fatal("disabled policy still applied")
	}
}

func TestUsageCorrectionAllowanceValidation(t *testing.T) {
	if got, err := usageCorrectionAllowance(nil); err != nil || got != 10 {
		t.Fatalf("default %d %v", got, err)
	}
	for _, value := range []int{-1, 0, 10, 20, 37, 1000, 1001} {
		got, err := usageCorrectionAllowance(&value)
		valid := value >= 0 && value <= 1000
		if valid && (err != nil || got != value) || !valid && err == nil {
			t.Fatalf("value=%d got=%d err=%v", value, got, err)
		}
	}
}

func TestUsageCorrectionRealForwardingJSONAndSSE(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				original := `{"id":"test","model":"claude-test","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":7,"total_tokens":1000007,"prompt_tokens_details":{"cached_tokens":999900}}}`
				if path == "/v1/messages" {
					original = `{"id":"test","model":"claude-test","type":"message","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":100,"cache_creation_input_tokens":900000,"cache_read_input_tokens":99900,"output_tokens":7}}`
				}
				if path == "/v1/responses" {
					original = `{"id":"test","model":"claude-test","output":[],"usage":{"input_tokens":1000000,"output_tokens":7,"total_tokens":1000007,"input_tokens_details":{"cached_tokens":999900}}}`
				}
				payload := `{"model":"claude-test","messages":[{"role":"user","content":"hello"}],"input":"hello","stream":` + strconvBool(stream) + `}`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sent, _ := io.ReadAll(r.Body)
					if string(sent) != payload {
						t.Errorf("request changed: %s", sent)
					}
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, original)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					data := original
					if path == "/v1/messages" {
						data = `{"type":"message_start","message":` + original + `}`
					}
					if path == "/v1/responses" {
						data = `{"type":"response.completed","response":` + original + `}`
					}
					io.WriteString(w, "data: "+data+"\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				s, route := preparedProbeService(t, server)
				for _, enabled := range []bool{false, true} {
					replay, err := newReplayBody(io.NopCloser(strings.NewReader(payload)), 4096)
					if err != nil {
						t.Fatal(err)
					}
					incoming := s.withUsageCorrection(httptest.NewRequest("POST", path, nil), replay, SmartGroup{UsageCorrectionEnabled: enabled, UsageCorrectionAllowancePercent: 10})
					response, _, err := s.performAttempt(incoming, route, replay.Open, nil, mappedResponseContext{}, false)
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(response.Body)
					response.Body.Close()
					replay.Close()
					if err != nil {
						t.Fatal(err)
					}
					if enabled {
						if response.Header.Get("X-TransitHub-Usage-Policy") != "estimated-input-cap" || bytes.Contains(got, []byte("900000")) || bytes.Contains(got, []byte("1000000")) {
							t.Fatalf("wire correction missing: %s", got)
						}
					} else if !bytes.Contains(got, []byte(original)) {
						t.Fatalf("disabled policy changed bytes: %s", got)
					}
				}
			})
		}
	}
}

func strconvBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
