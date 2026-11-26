package model_proxy

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	sentencepiece "github.com/eliben/go-sentencepiece"
	"github.com/tiktoken-go/tokenizer"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Generated offline from independent tokenizers. These are empirical ranges,
// not mathematical bounds or evidence that an upstream injected context.
//
//go:embed token_range_profiles.json
var tokenRangeProfileJSON []byte

type tokenRangeProfile struct {
	LowerFactor float64 `json:"lower_factor"`
	UpperFactor float64 `json:"upper_factor"`
}

var tokenRangeProfiles = func() map[string]tokenRangeProfile {
	var data struct {
		Profiles map[string]tokenRangeProfile `json:"profiles"`
	}
	if err := json.Unmarshal(tokenRangeProfileJSON, &data); err != nil {
		panic(err)
	}
	return data.Profiles
}()

const correctionSampleBytes = 16384
const correctionSampleWindows = 64
const correctionSampleContext = 32
const correctionMaxSampleWork = correctionSampleBytes + 2*correctionSampleContext*correctionSampleWindows

type correctionInputFeatures struct {
	Bytes, Sampled         int64
	Messages, Tools, Nodes int64
	Samples                []string
	Exact                  bool
	Cache                  *sync.Map
}

type correctionTokenRange struct {
	Min, Max int64
	Method   string
}

// Walk JSON structure and retain string references; never marshal the request or
// scan entire long strings. Whitespace, JSON escaping, metadata and max_tokens
// do not change this representation. Traversal is O(nodes); text sampling is
// bounded at 20 KiB regardless of request size. Parsing/receiving remains O(bytes).
func correctionFeatures(input map[string]any) (correctionInputFeatures, bool) {
	f := correctionInputFeatures{Cache: new(sync.Map)}
	for _, key := range []string{"previous_response_id", "conversation", "cachedContent", "cached_content", "container", "context_management", "mcp_servers"} {
		if v := input[key]; v != nil && v != "" {
			return f, false
		}
	}
	var parts []string
	nodes := 0
	var collect func(any, bool, int) bool
	collect = func(value any, keys bool, depth int) bool {
		nodes++
		if nodes > 65536 || depth > 64 {
			return false
		}
		switch v := value.(type) {
		case string:
			if len(v) > 0 {
				parts = append(parts, v)
				f.Bytes += int64(len(v))
			}
		case []any:
			for _, item := range v {
				if !collect(item, keys, depth+1) {
					return false
				}
			}
		case map[string]any:
			kind, _ := v["type"].(string)
			if !keys {
				switch kind {
				case "image", "image_url", "input_image", "document", "file", "input_file", "input_audio", "audio", "video", "video_url", "item_reference", "compaction", "server_tool_use", "web_search_tool_result", "mcp_tool_use", "mcp_tool_result":
					return false
				}
			}
			ordered := make([]string, 0, len(v))
			for key := range v {
				ordered = append(ordered, key)
			}
			sort.Strings(ordered) // Stable sampling across map iteration and retries.
			for _, key := range ordered {
				if !keys {
					switch key {
					case "file_id", "file_data", "file_url", "encrypted_content", "inlineData", "fileData", "inline_data", "file_data_uri":
						return false
					case "cache_control", "metadata", "signature":
						continue
					}
				}
				childKeys := keys || key == "parameters" || key == "input_schema" || key == "schema" || key == "input" || key == "args" || key == "response"
				if keys && !collect(key, true, depth+1) {
					return false
				}
				if !collect(v[key], childKeys, depth+1) {
					return false
				}
			}
		case json.Number:
			return collect(v.String(), keys, depth+1)
		case float64:
			return collect(strconv.FormatFloat(v, 'g', -1, 64), keys, depth+1)
		case bool:
			return collect(strconv.FormatBool(v), keys, depth+1)
		case nil:
		default:
			return false
		}
		return true
	}
	hasInput := false
	for _, key := range []string{"messages", "input", "prompt", "system", "instructions", "contents", "systemInstruction", "system_instruction", "tools", "functions", "tool_choice", "function_call", "response_format", "text"} {
		v, exists := input[key]
		if !exists || v == nil {
			continue
		}
		hasInput = true
		if key == "tools" {
			if tools, ok := v.([]any); ok {
				f.Tools = int64(len(tools))
				for _, item := range tools {
					tool, ok := item.(map[string]any)
					if !ok {
						return f, false
					}
					kind, _ := tool["type"].(string)
					if kind != "" && kind != "function" && kind != "custom" {
						return f, false
					}
				}
			}
		}
		if key == "messages" || key == "input" || key == "contents" {
			if items, ok := v.([]any); ok {
				f.Messages += int64(len(items))
			}
		}
		if !collect(v, key == "tools" || key == "functions" || key == "response_format", 0) {
			return f, false
		}
	}
	f.Nodes = int64(nodes)
	if !hasInput {
		return f, false
	}
	if f.Bytes == 0 {
		return f, true
	}
	if f.Bytes <= correctionSampleBytes {
		f.Samples, f.Sampled, f.Exact = parts, f.Bytes, true
		return f, true
	}
	width, windows := int64(correctionSampleBytes/correctionSampleWindows), int64(correctionSampleWindows)
	part, base := 0, int64(0)
	for i := int64(0); i < windows; i++ {
		center := (f.Bytes - width) * i / (windows - 1)
		offset, end := max(int64(0), center-correctionSampleContext), min(f.Bytes, center+width+correctionSampleContext)
		for part < len(parts) && base+int64(len(parts[part])) <= offset {
			base += int64(len(parts[part]))
			part++
		}
		j, position := part, base
		var window strings.Builder
		for j < len(parts) && position < end {
			lo, hi := max(int64(0), offset-position), min(int64(len(parts[j])), end-position)
			sample := trimCorrectionUTF8(parts[j][lo:hi])
			window.WriteString(sample)
			position += int64(len(parts[j]))
			j++
		}
		sample := window.String()
		if len(sample) > 0 {
			f.Samples = append(f.Samples, sample)
			f.Sampled += int64(len(sample))
		}
	}
	return f, f.Sampled > 0
}

func trimCorrectionUTF8(sample string) string {
	for len(sample) > 0 && !utf8.RuneStart(sample[0]) {
		sample = sample[1:]
	}
	for len(sample) > 0 {
		r, n := utf8.DecodeLastRuneInString(sample)
		if r != utf8.RuneError || n != 1 {
			break
		}
		sample = sample[:len(sample)-1]
	}
	return sample
}

func correctionProfileName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "models/")
	// Provider aliases do not identify the underlying tokenizer.
	if strings.Contains(model, "luna") {
		return ""
	}
	for _, prefix := range []string{"gpt-4o", "chatgpt-4o", "gpt-4.1", "gpt-4.5", "gpt-5", "o1", "o3", "o4-mini"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") || strings.HasPrefix(model, prefix+".") {
			return "o200k_base"
		}
	}
	for _, prefix := range []string{"gpt-4", "gpt-3.5-turbo", "gpt-35-turbo"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return "cl100k_base"
		}
	}
	// Only model IDs explicitly covered by Google's tokenizer registry. Newer
	// Gemini models can use a different vocabulary and must not silently inherit it.
	switch model {
	case "gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.0-flash", "gemini-2.0-flash-lite", "gemini-2.0-flash-001", "gemini-2.0-flash-lite-001", "gemini-3-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro-preview-06-05", "gemini-2.5-pro-preview-05-06", "gemini-2.5-flash-preview-05-20", "gemini-2.5-flash-preview-04-17":
		return "gemma3"
	}
	return ""
}

//go:embed tokenizers/gemma3.model.gz
var correctionGemmaModel []byte

var correctionCounters = map[string]*correctionCounter{
	"o200k_base": {}, "cl100k_base": {}, "gemma3": {},
}

type correctionCounter struct {
	once  sync.Once
	count func(string) (int, error)
	err   error
}

func correctionTokenCounter(name string) (func(string) (int, error), error) {
	holder := correctionCounters[name]
	if holder == nil {
		return nil, fmt.Errorf("unsupported tokenizer %s", name)
	}
	holder.once.Do(func() {
		if name == "gemma3" {
			reader, err := gzip.NewReader(bytes.NewReader(correctionGemmaModel))
			if err != nil {
				holder.err = err
				return
			}
			defer reader.Close()
			processor, err := sentencepiece.NewProcessor(reader)
			if err != nil {
				holder.err = err
				return
			}
			holder.count = func(s string) (int, error) { return len(processor.Encode(s)), nil }
		} else {
			codec, err := tokenizer.Get(tokenizer.Encoding(name))
			if err != nil {
				holder.err = err
				return
			}
			holder.count = codec.Count
		}
	})
	return holder.count, holder.err
}

func (f correctionInputFeatures) tokenRange(model string) correctionTokenRange {
	name := correctionProfileName(model)
	profile, calibrated := tokenRangeProfiles[name]
	framing := int64(16) + 16*f.Messages + 32*f.Tools + 2*f.Nodes
	fallback := correctionTokenRange{Min: 1, Max: max(int64(1), f.Bytes+framing), Method: "utf8-byte-fallback-unvalidated"}
	// No current official local Claude tokenizer or official count samples.
	// Keep this deliberately wide; never fit to suspicious upstream usage.
	if !calibrated {
		return fallback
	}
	if f.Cache != nil {
		if value, ok := f.Cache.Load(name); ok {
			return value.(correctionTokenRange)
		}
	}
	count, err := correctionTokenCounter(name)
	if err != nil {
		fallback.Method = "utf8-byte-fallback-tokenizer-error"
		return fallback
	}
	tokens, sumSquares := int64(0), float64(0)
	densities := make([]float64, 0, len(f.Samples))
	for _, sample := range f.Samples {
		n, err := count(sample)
		if err != nil {
			fallback.Method = "utf8-byte-fallback-tokenizer-error"
			return fallback
		}
		tokens += int64(n)
		densities = append(densities, float64(n)/float64(len(sample)))
	}
	lower, upper := float64(tokens), float64(tokens)
	method := "full-text-tokenizer-range-v1/" + name
	if !f.Exact && f.Sampled > 0 {
		mean := float64(tokens) / float64(f.Sampled)
		average := float64(0)
		for _, v := range densities {
			average += v
		}
		average /= float64(len(densities))
		for _, v := range densities {
			sumSquares += (v - average) * (v - average)
		}
		error := float64(0)
		if len(densities) > 1 {
			error = 3 * math.Sqrt(sumSquares/float64(len(densities)-1)/float64(len(densities)))
		}
		lower = max(float64(0), mean-error) * float64(f.Bytes) * profile.LowerFactor
		upper = (mean + error) * float64(f.Bytes) * profile.UpperFactor
		method = "sampled-tokenizer-range-v1/" + name
	}
	result := correctionTokenRange{Min: max(int64(1), int64(math.Floor(lower))), Max: max(int64(1), int64(math.Ceil(upper))+framing), Method: method}
	if f.Cache != nil {
		f.Cache.Store(name, result)
	}
	return result
}
