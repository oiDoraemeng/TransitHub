package model_proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func correctionEstimateFixture(t testing.TB, text string, spool bool) (*replayBody, map[string]any) {
	t.Helper()
	raw := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"` + text + `"}]}`)
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	body := &replayBody{memory: raw, size: int64(len(raw)), object: input, objectRead: true}
	if spool {
		file, err := os.CreateTemp(t.TempDir(), "request-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(raw); err != nil {
			t.Fatal(err)
		}
		body.memory, body.file = nil, file
	}
	t.Cleanup(func() { body.Close() })
	return body, input
}

func TestCorrectionEstimateBoundedSamplingAcrossLanguagesAndStorage(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("plain english text ", 20000),
		strings.Repeat("完整历史中文内容", 20000),
		strings.Repeat("English 中文交替 ", 20000),
		strings.Repeat("emoji 🌍🚀 ", 20000),
	} {
		memory, input := correctionEstimateFixture(t, text, false)
		spool, _ := correctionEstimateFixture(t, text, true)
		first, known := estimateCorrectionInput(input, memory)
		second, spoolKnown := estimateCorrectionInput(input, spool)
		if !known || !spoolKnown || first != second {
			t.Fatalf("storage changed estimate: %d %d %v %v", first, second, known, spoolKnown)
		}
		for _, body := range []*replayBody{memory, spool} {
			sampled, _, ok := sampleCorrectionBytes(body)
			if !ok || sampled > correctionSampleBytes {
				t.Fatalf("unbounded sampling: %d %v", sampled, ok)
			}
			reader, err := body.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(got, memory.memory) {
				t.Fatal("sampling changed request replay")
			}
		}
		// Check language calibration against a complete byte census in this
		// test only. Production reads a fixed sample, irrespective of length.
		nonASCII := int64(0)
		for _, b := range memory.memory {
			if b >= 128 {
				nonASCII++
			}
		}
		full := (memory.size+nonASCII+2)/3 + 16
		if delta := first - full; delta < -full/20 || delta > full/20 {
			t.Fatalf("language sampling drift >5%%: estimate=%d full=%d", first, full)
		}
	}
}

func TestCorrectionEstimateSmallRequestAndFailedSpoolRead(t *testing.T) {
	body, input := correctionEstimateFixture(t, "short 中文", false)
	sampled, _, ok := sampleCorrectionBytes(body)
	if !ok || sampled != body.size {
		t.Fatalf("small sample=%d body=%d ok=%v", sampled, body.size, ok)
	}
	spool, _ := correctionEstimateFixture(t, "short 中文", true)
	if err := spool.file.Truncate(1); err != nil {
		t.Fatal(err)
	}
	if _, known := estimateCorrectionInput(input, spool); known {
		t.Fatal("unreadable spool must preserve upstream usage")
	}
}

// Keep the previous implementation only as a benchmark baseline. Both paths
// receive the same parsed JSON; request receipt and JSON parsing are excluded.
func legacyCorrectionEstimate(input map[string]any) (int64, bool) {
	for _, key := range []string{"previous_response_id", "conversation", "cachedContent", "cached_content", "container", "context_management", "mcp_servers"} {
		if v := input[key]; v != nil && v != "" {
			return 0, false
		}
	}
	selected := map[string]any{}
	for _, key := range []string{"messages", "input", "prompt", "system", "instructions", "tools", "functions", "tool_choice", "function_call"} {
		if v := input[key]; v != nil {
			selected[key] = v
		}
	}
	if len(selected) == 0 || opaqueCorrectionContent(selected) {
		return 0, false
	}
	if tools, ok := input["tools"].([]any); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				return 0, false
			}
			kind, _ := tool["type"].(string)
			if kind != "" && kind != "function" && kind != "custom" {
				return 0, false
			}
		}
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return 0, false
	}
	var ascii, other int64
	for _, r := range string(raw) {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	framing := int64(8)
	if messages, ok := input["messages"].([]any); ok {
		framing += int64(len(messages)) * 8
	}
	if items, ok := input["input"].([]any); ok {
		framing += int64(len(items)) * 8
	}
	return (ascii+2)/3 + other*2 + framing, true
}

var correctionEstimateBenchmarkResult int64

func BenchmarkCorrectionEstimate(b *testing.B) {
	for _, size := range []int{1024, 1 << 20, 16 << 20} {
		for _, language := range []string{"ASCII", "CJK"} {
			text := strings.Repeat("a", size)
			if language == "CJK" {
				text = strings.Repeat("中", size/3)
			}
			body, input := correctionEstimateFixture(b, text, false)
			b.Run(fmt.Sprintf("%s/%d/legacy", language, size), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					correctionEstimateBenchmarkResult, _ = legacyCorrectionEstimate(input)
				}
			})
			b.Run(fmt.Sprintf("%s/%d/fast-memory", language, size), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					correctionEstimateBenchmarkResult, _ = estimateCorrectionInput(input, body)
				}
			})
			if size >= 1<<20 {
				spool, _ := correctionEstimateFixture(b, text, true)
				b.Run(fmt.Sprintf("%s/%d/fast-spool", language, size), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						correctionEstimateBenchmarkResult, _ = estimateCorrectionInput(input, spool)
					}
				})
			}
		}
	}
}
