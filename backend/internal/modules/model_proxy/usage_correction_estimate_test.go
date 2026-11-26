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
		features, ok := correctionFeatures(input)
		if !ok || features.Sampled > correctionMaxSampleWork || features.Sampled == 0 {
			t.Fatalf("unbounded sampling: %+v %v", features, ok)
		}
		for _, body := range []*replayBody{memory, spool} {
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
		for _, model := range []string{"gpt-4o", "gpt-4", "gemini-2.5-pro", "claude-sonnet-5-5"} {
			range_ := features.tokenRange(model)
			if range_.Min <= 0 || range_.Max < range_.Min || range_.Method == "" {
				t.Fatalf("bad range: %+v", range_)
			}
		}
	}
}

func TestCorrectionEstimateUsesDecodedInputAndPreservesSpool(t *testing.T) {
	body, input := correctionEstimateFixture(t, "short 中文", false)
	features, ok := correctionFeatures(input)
	if !ok || features.Sampled != features.Bytes || features.Bytes >= body.size {
		t.Fatalf("small sample=%+v body=%d ok=%v", features, body.size, ok)
	}
	spool, _ := correctionEstimateFixture(t, "short 中文", true)
	position, _ := spool.file.Seek(0, io.SeekCurrent)
	if _, known := estimateCorrectionInput(input, spool); !known {
		t.Fatal("decoded input must not require spool I/O")
	}
	after, _ := spool.file.Seek(0, io.SeekCurrent)
	if position != after {
		t.Fatal("estimation moved the replay file position")
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
