package model_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/bits"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A deliberately conservative local estimate, not a Claude tokenizer or a
// measurement of hidden input. Count the complete visible history, system text,
// tool definitions/results and JSON structure; never just the last message.
// ASCII uses 3 bytes/token, other Unicode code points 2 tokens each, plus
// message framing. Cache hints do not hide text supplied in the same request.
func estimateCorrectionInput(input map[string]any) (int64, bool) {
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
	// Hosted tools can add context that is absent from the submitted request.
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

func opaqueCorrectionContent(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if opaqueCorrectionContent(child) {
				return true
			}
		}
	case map[string]any:
		kind, _ := v["type"].(string)
		switch kind {
		case "image", "image_url", "input_image", "document", "file", "input_file", "input_audio", "audio", "video", "video_url", "item_reference", "compaction", "server_tool_use", "web_search_tool_result", "mcp_tool_use", "mcp_tool_result":
			return true
		}
		for key, child := range v {
			// A tool schema can describe arbitrary fields called image or file.
			if key == "parameters" || key == "input_schema" || key == "schema" {
				continue
			}
			switch key {
			case "file_id", "file_data", "file_url", "encrypted_content", "inlineData", "fileData":
				return true
			}
			if opaqueCorrectionContent(child) {
				return true
			}
		}
	}
	return false
}

type inputUsageSnapshot struct {
	Input      int64 `json:"input"` // Inclusive in Chat/Responses, ordinary in Messages.
	Read       int64 `json:"cache_read"`
	Write      int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	FiveMinute int64 `json:"cache_write_5m"`
	OneHour    int64 `json:"cache_write_1h"`
}

type inputUsageCorrector struct {
	policy                      inputUsagePolicy
	messages                    bool
	raw, corrected              inputUsageSnapshot
	adjusted                    bool
	routeID, requestID          string
	once                        sync.Once
	chat                        bool
	writePath                   string
	ttlSeen                     bool
	readSeen                    bool
	readPath                    string
	peakOriginal, peakCorrected inputUsageSnapshot
}

func addUsage(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func usageInteger(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	return n, err == nil && n >= 0
}

// v*budget/total, with integer rounding and no overflowing intermediate product.
func usageFraction(v, budget, total int64) int64 {
	if v == 0 || budget == 0 || total == 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(v), uint64(budget))
	q, _ := bits.Div64(hi, lo, uint64(total))
	return int64(q)
}

func capUsage(raw inputUsageSnapshot, limit int64, messages bool) (inputUsageSnapshot, bool) {
	if limit <= 0 {
		return raw, false
	}
	caches, ok := addUsage(raw.Read, raw.Write)
	if !ok {
		return raw, false
	}
	total := raw.Input
	ordinary := raw.Input
	if messages {
		total, ok = addUsage(raw.Input, caches)
		if !ok {
			return raw, false
		}
	} else {
		ordinary = raw.Input - caches
		if ordinary < 0 {
			ordinary = 0
		}
	}
	if total <= limit && caches <= total {
		return raw, false
	}
	corrected := raw
	if total > limit {
		total = limit
	}
	if ordinary > total {
		ordinary = total
	}
	budget := total - ordinary
	if caches > 0 {
		corrected.Write = usageFraction(raw.Write, budget, caches)
		corrected.Read = budget - corrected.Write
	} else {
		corrected.Read = 0
		corrected.Write = 0
	}
	if messages {
		corrected.Input = ordinary
	} else {
		corrected.Input = total
	}
	ttl, ok := addUsage(raw.FiveMinute, raw.OneHour)
	if ok && ttl > 0 {
		corrected.FiveMinute = usageFraction(raw.FiveMinute, corrected.Write, ttl)
		corrected.OneHour = corrected.Write - corrected.FiveMinute
	}
	return corrected, true
}

func (c *inputUsageCorrector) rewrite(raw []byte) []byte {
	if !gjson.ValidBytes(raw) {
		return raw
	}
	root := gjson.ParseBytes(raw)
	path := "usage"
	if c.messages && root.Get("type").String() == "message_start" {
		path = "message.usage"
	}
	if !c.messages && strings.HasPrefix(root.Get("type").String(), "response.") {
		path = "response.usage"
	}
	usage := root.Get(path)
	if !usage.IsObject() {
		return raw
	}
	// Only protocol usage objects qualify. A tool's argument named usage is
	// never walked recursively or interpreted as accounting.
	// A message_start starts a fresh cumulative snapshot. Explicit zero values
	// overwrite old fields; missing fields preserve them. Never sum SSE events.
	snapshot := c.raw
	if root.Get("type").String() == "message_start" {
		snapshot = inputUsageSnapshot{}
	}
	inputKey, outputKey, readKey := "input_tokens", "output_tokens", "input_tokens_details.cached_tokens"
	writeKeys := []string{"cache_creation_input_tokens", "input_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cache_creation_input_tokens"}
	if c.messages {
		readKey = "cache_read_input_tokens"
		writeKeys = []string{"cache_creation_input_tokens"}
	} else if c.chat || usage.Get("prompt_tokens").Exists() {
		inputKey, outputKey, readKey = "prompt_tokens", "completion_tokens", "prompt_tokens_details.cached_tokens"
		writeKeys = []string{"cache_creation_input_tokens", "prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_input_tokens"}
	}
	// Handle partial Chat snapshots too, when only completion/cache fields occur.
	if !c.messages && (usage.Get("completion_tokens").Exists() || usage.Get("prompt_tokens_details").Exists()) {
		inputKey, outputKey, readKey = "prompt_tokens", "completion_tokens", "prompt_tokens_details.cached_tokens"
		writeKeys = []string{"cache_creation_input_tokens", "prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_input_tokens"}
	}
	if inputKey == "prompt_tokens" {
		c.chat = true
	}
	c.readSeen = c.readSeen || usage.Get(readKey).Exists()
	updates := []struct {
		path   string
		target *int64
	}{{inputKey, &snapshot.Input}, {outputKey, &snapshot.Output}, {readKey, &snapshot.Read}}
	readKeys := []string{readKey}
	if !c.messages {
		readKeys = append(readKeys, "cache_read_input_tokens", "cached_tokens")
	}
	for _, key := range readKeys {
		if usage.Get(key).Exists() {
			c.readPath = key
			c.readSeen = true
			updates = append(updates, struct {
				path   string
				target *int64
			}{key, &snapshot.Read})
			break
		}
	}
	for _, key := range writeKeys {
		if usage.Get(key).Exists() {
			c.writePath = key
			updates = append(updates, struct {
				path   string
				target *int64
			}{key, &snapshot.Write})
			break
		}
	}
	if c.messages {
		c.ttlSeen = c.ttlSeen || usage.Get("cache_creation").Exists()
		updates = append(updates, struct {
			path   string
			target *int64
		}{"cache_creation.ephemeral_5m_input_tokens", &snapshot.FiveMinute}, struct {
			path   string
			target *int64
		}{"cache_creation.ephemeral_1h_input_tokens", &snapshot.OneHour})
	}
	for _, update := range updates {
		value := usage.Get(update.path)
		if value.Exists() {
			n, ok := usageInteger(value)
			if !ok {
				return raw
			}
			*update.target = n
		}
	}
	corrected, changed := capUsage(snapshot, c.policy.Limit, c.messages)
	c.raw = snapshot
	c.corrected = corrected
	if !changed && !c.adjusted {
		return raw
	}
	patches := map[string]int64{inputKey: corrected.Input}
	if c.messages {
		patches["cache_read_input_tokens"] = corrected.Read
		patches["cache_creation_input_tokens"] = corrected.Write
		if c.ttlSeen {
			patches["cache_creation.ephemeral_5m_input_tokens"] = corrected.FiveMinute
			patches["cache_creation.ephemeral_1h_input_tokens"] = corrected.OneHour
		}
	} else {
		if c.readSeen || corrected.Read != 0 {
			patches[readKey] = corrected.Read
		}
		for _, key := range readKeys {
			if usage.Get(key).Exists() {
				patches[key] = corrected.Read
			}
		}
		if c.readPath != "" {
			patches[c.readPath] = corrected.Read
		}
		for _, key := range writeKeys {
			if usage.Get(key).Exists() {
				patches[key] = corrected.Write
			}
		}
		if c.writePath != "" {
			patches[c.writePath] = corrected.Write
		}
		if usage.Get("total_tokens").Exists() {
			total, ok := addUsage(corrected.Input, corrected.Output)
			if !ok {
				return raw
			}
			patches["total_tokens"] = total
		}
	}
	encoded := raw
	for key, value := range patches {
		var err error
		encoded, err = sjson.SetBytes(encoded, path+"."+key, value)
		if err != nil {
			return raw
		}
	}
	c.adjusted = c.adjusted || changed
	peakTotal := c.peakOriginal.Input
	rawTotal := snapshot.Input
	if c.messages {
		peakTotal += c.peakOriginal.Read + c.peakOriginal.Write
		rawTotal += snapshot.Read + snapshot.Write
	}
	if rawTotal >= peakTotal {
		c.peakOriginal = snapshot
		c.peakCorrected = corrected
	}
	return encoded
}

func (c *inputUsageCorrector) audit() {
	c.once.Do(func() {
		if !c.adjusted {
			return
		}
		// Original numerical snapshots are retained, without prompts or secrets.
		record := struct {
			Route                       string `json:"route_id"`
			Request                     string `json:"request_id,omitempty"`
			Estimate                    int64  `json:"estimated_input"`
			Limit                       int64  `json:"input_limit"`
			Allowance                   int    `json:"allowance_percent"`
			Messages                    bool   `json:"messages_input_excludes_cache"`
			Original, Corrected         inputUsageSnapshot
			PeakOriginal, PeakCorrected inputUsageSnapshot
		}{c.routeID, c.requestID, c.policy.Estimate, c.policy.Limit, c.policy.AllowancePercent, c.messages, c.raw, c.corrected, c.peakOriginal, c.peakCorrected}
		encoded, _ := json.Marshal(record)
		log.Printf("[usage-correction] %s", encoded)
	})
}

func correctResponseUsage(response *http.Response, policy inputUsagePolicy, path, routeID string) error {
	switch path {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses":
	default:
		return nil
	}
	c := &inputUsageCorrector{policy: policy, messages: path == "/v1/messages", chat: path == "/v1/chat/completions", routeID: routeID, requestID: response.Header.Get("X-Request-Id")}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Set("X-TransitHub-Usage-Policy", "estimated-input-cap")
	response.Header.Set("X-TransitHub-Input-Token-Limit", strconv.FormatInt(policy.Limit, 10))
	if isEventStream(response.Header) {
		response.Body = &correctedUsageBody{source: bufio.NewReader(response.Body), close: response.Body, corrector: c}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		response.Header.Del("ETag")
		response.Header.Del("Content-MD5")
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
	closeErr := response.Body.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(raw) > maxProtocolResponseBytes {
		return fmt.Errorf("usage correction response exceeds %d bytes", maxProtocolResponseBytes)
	}
	encoded := c.rewrite(raw)
	c.audit()
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	response.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	response.Header.Del("ETag")
	response.Header.Del("Content-MD5")
	return nil
}

type correctedUsageBody struct {
	source     *bufio.Reader
	close      io.ReadCloser
	corrector  *inputUsageCorrector
	pending    []byte
	pendingErr error
}

func (b *correctedUsageBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.pendingErr != nil {
			b.corrector.audit()
			return 0, b.pendingErr
		}
		var frame []byte
		for {
			line, err := b.source.ReadBytes('\n')
			frame = append(frame, line...)
			if len(frame) > maxProtocolResponseBytes {
				b.pendingErr = fmt.Errorf("usage correction SSE event exceeds %d bytes", maxProtocolResponseBytes)
				b.corrector.audit()
				return 0, b.pendingErr
			}
			if err != nil {
				b.pendingErr = err
				break
			}
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				break
			}
		}
		b.pending = rewriteUsageEvent(frame, b.corrector.rewrite)
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *correctedUsageBody) Close() error { b.corrector.audit(); return b.close.Close() }

// Preserve event names/IDs, CRLF, tool payloads and unknown metadata. JSON is
// patched by path, so tool integers and signatures never pass through float64.
func rewriteUsageEvent(frame []byte, rewrite func([]byte) []byte) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	first := -1
	var data []byte
	for i, line := range lines {
		line = bytes.TrimRight(line, "\r\n")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first < 0 {
			first = i
		} else {
			data = append(data, '\n')
		}
		value := line[5:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		data = append(data, value...)
	}
	if first < 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return frame
	}
	encoded := rewrite(data)
	if bytes.Equal(encoded, data) {
		return frame
	}
	var out []byte
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if i != first {
				continue
			}
			out = append(out, []byte("data: ")...)
			out = append(out, encoded...)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				out = append(out, '\r', '\n')
			} else if bytes.HasSuffix(line, []byte("\n")) {
				out = append(out, '\n')
			}
		} else {
			out = append(out, line...)
		}
	}
	return out
}
