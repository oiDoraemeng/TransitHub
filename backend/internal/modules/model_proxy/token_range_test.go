package model_proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTokenRangeFortyPercentBoundaryAndHistoricalCache(t *testing.T) {
	limit := correctionRangeLimit(170000, 40)
	if limit != 238000 {
		t.Fatalf("limit=%d", limit)
	}
	for _, total := range []int64{150000, 170000, 237999, 238000, 238001, 1695788} {
		raw := inputUsageSnapshot{Input: 47692, Read: total - 47692, Output: 4972}
		got, changed := capUsage(raw, limit, true)
		if total <= limit {
			if changed || got != raw {
				t.Fatalf("within allowance changed: %d %+v", total, got)
			}
		} else if !changed || got.Input+got.Read+got.Write != limit || got.Input != raw.Input || got.Output != raw.Output {
			t.Fatalf("bad correction: %d %+v", total, got)
		}
	}
	// An inconsistent cache breakdown alone must not trigger this policy.
	raw := inputUsageSnapshot{Input: 100, Read: 200, Output: 7}
	if got, changed := capUsage(raw, 100, false); changed || got != raw {
		t.Fatal("below-limit usage was normalized")
	}
	for _, tc := range []struct {
		max     int64
		percent int
		want    int64
	}{
		{170000, 0, 170000}, {101, 40, 142}, {math.MaxInt64, 1000, math.MaxInt64}, {1, 40, 2},
	} {
		if got := correctionRangeLimit(tc.max, tc.percent); got != tc.want {
			t.Fatalf("%+v got %d", tc, got)
		}
	}
}

func TestTokenRangeSerializationMetadataAndStableMapOrder(t *testing.T) {
	text := strings.Repeat("完整历史 English 中文 🚀 ", 1200)
	payload := map[string]any{"model": "gpt-4o", "messages": []any{map[string]any{"role": "user", "content": text}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"image": map[string]any{"type": "string"}}}}}}}
	first, ok := correctionFeatures(payload)
	if !ok {
		t.Fatal("no features")
	}
	for i := 0; i < 20; i++ {
		payload["metadata"] = map[string]any{"padding": strings.Repeat("not input ", 10000)}
		payload["max_tokens"] = 1000000
		raw, _ := json.MarshalIndent(payload, "", "  ")
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		got, known := correctionFeatures(decoded)
		if !known || !reflect.DeepEqual(got, first) {
			t.Fatalf("serialization or map order changed range: %+v %+v", first, got)
		}
	}
}

func TestTokenRangeReferencesAndUnknownModelFallback(t *testing.T) {
	var fixtures []struct {
		Text   string           `json:"text"`
		Counts map[string]int64 `json:"counts"`
	}
	raw, err := os.ReadFile("testdata/token_range_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	models := map[string]string{"o200k_base": "gpt-4o", "cl100k_base": "gpt-4", "gemma3": "gemini-2.5-pro"}
	for _, fixture := range fixtures {
		f, ok := correctionFeatures(map[string]any{"input": fixture.Text})
		if !ok {
			t.Fatal("no features")
		}
		for profile, actual := range fixture.Counts {
			count, err := correctionTokenCounter(profile)
			if err != nil {
				t.Fatal(err)
			}
			got, err := count(fixture.Text)
			if err != nil || int64(got) != actual {
				t.Fatalf("Go/Python tokenizer mismatch %s: got=%d actual=%d err=%v", profile, got, actual, err)
			}
			range_ := f.tokenRange(models[profile])
			if actual > correctionRangeLimit(range_.Max, 40) {
				t.Fatalf("legitimate %s input would be capped: actual=%d range=%+v", profile, actual, range_)
			}
		}
		for _, model := range []string{"claude-sonnet-5-5", "private-alias", "gpt-6-unknown", "gemini-3.5-flash"} {
			r := f.tokenRange(model)
			if r.Method != "utf8-byte-fallback-unvalidated" || r.Min != 1 || r.Max < int64(len(fixture.Text)) {
				t.Fatalf("invented calibration for %s: %+v", model, r)
			}
		}
	}
}

func BenchmarkTokenRangeLocal(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20} {
		for _, model := range []string{"gpt-4o", "gemini-2.5-pro", "claude-sonnet-5-5"} {
			text := strings.Repeat("Complete history 中文 text and tools. ", size/40+1)
			input := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": text}}}
			f, _ := correctionFeatures(input)
			f.tokenRange(model) // initialize vocabulary before timing
			b.Run(model+"/"+strconv.Itoa(size), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					f, _ := correctionFeatures(input)
					correctionEstimateBenchmarkResult = f.tokenRange(model).Max
				}
			})
		}
	}
}

func TestTokenRangeMappingPerMemberAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1000000,"completion_tokens":7,"total_tokens":1000007}}`)
	}))
	defer server.Close()
	s, route := preparedProbeService(t, server)
	payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"` + strings.Repeat("Text input 中文 ", 1000) + `"}]}`)
	replay, err := newReplayBody(io.NopCloser(bytes.NewReader(payload)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	incoming := s.withUsageCorrection(httptest.NewRequest("POST", "/v1/chat/completions", nil), replay, SmartGroup{UsageCorrectionEnabled: true, UsageCorrectionAllowancePercent: 40})
	for _, enabled := range []bool{true, false, true} {
		route.ModelMappingEnabled = enabled
		response, _, err := s.performAttempt(incoming, route, replay.Open, map[string]string{"gpt-4o": "claude-sonnet-5-5"}, mappedResponseContext{}, false)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(response.Body)
		response.Body.Close()
		method := response.Header.Get("X-TransitHub-Input-Estimate-Method")
		if enabled && method != "utf8-byte-fallback-unvalidated" || !enabled && !strings.HasSuffix(method, "tokenizer-range-v1/o200k_base") {
			t.Fatalf("mapping=%v used %s", enabled, method)
		}
		if gjson.GetBytes(got, "usage.completion_tokens").Int() != 7 {
			t.Fatal("output changed")
		}
	}
}

func TestTokenRangeConcurrentCountersAndRequestCache(t *testing.T) {
	f, ok := correctionFeatures(map[string]any{"input": strings.Repeat("Complete history 中文 🚀 ", 2000)})
	if !ok {
		t.Fatal("missing features")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, model := range []string{"gpt-4o", "gpt-4", "gemini-2.5-pro"} {
				first := f.tokenRange(model)
				if got := f.tokenRange(model); got != first {
					t.Errorf("cache or shared tokenizer drift: %+v %+v", first, got)
				}
			}
		}()
	}
	wg.Wait()
}

func TestTokenRangeNativeGeminiAndOpaqueInputs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":7,"total_tokens":1000007}}`)
	}))
	defer server.Close()
	s, route := preparedProbeService(t, server)
	raw := `{"contents":[{"role":"user","parts":[{"text":"完整中文输入 history"}]}]}`
	replay, err := newReplayBody(io.NopCloser(strings.NewReader(raw)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	incoming := s.withUsageCorrection(httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil), replay, SmartGroup{UsageCorrectionEnabled: true, UsageCorrectionAllowancePercent: 40})
	response, _, err := s.performAttempt(incoming, route, replay.Open, nil, mappedResponseContext{}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.Header.Get("X-TransitHub-Input-Estimate-Method") != "full-text-tokenizer-range-v1/gemma3" || gjson.GetBytes(got, "usageMetadata.promptTokenCount").Int() >= 1000000 {
		t.Fatalf("native correction missing: %s", got)
	}
	for _, raw := range []string{
		`{"input":[{"type":"input_image","image_url":"https://example.com/a.png"}]}`,
		`{"input":"text","previous_response_id":"resp_opaque"}`,
		`{"contents":[{"parts":[{"inlineData":{"data":"aGVsbG8="}}]}]}`,
	} {
		var input map[string]any
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			t.Fatal(err)
		}
		if _, known := correctionFeatures(input); known {
			t.Fatalf("opaque input accepted: %s", raw)
		}
	}
}

func TestTokenRangeReviewExamples(t *testing.T) {
	text := strings.Repeat("请检查完整历史消息、系统提示词、工具定义和工具返回内容。缓存读取和写入不能重复计算。\n", 4000)
	for _, model := range []string{"gpt-4o", "gemini-2.5-pro", "claude-sonnet-5-5"} {
		f, ok := correctionFeatures(map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}})
		if !ok {
			t.Fatal("no features")
		}
		r := f.tokenRange(model)
		t.Logf("model=%s bytes=%d sampled=%d min=%d max=%d limit40=%d method=%s", model, f.Bytes, f.Sampled, r.Min, r.Max, correctionRangeLimit(r.Max, 40), r.Method)
	}
}
