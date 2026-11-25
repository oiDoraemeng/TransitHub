package model_proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

var errUpstreamUsageAnomaly = errors.New("upstream_usage_anomaly")

// This is a deliberately loose byte bound, not a tokenizer or a replacement
// billing estimate. Allow two tokens per complete JSON byte plus 64K of hidden
// provider context. Only self-contained text requests can use this bound.
type claudeUsageGuard struct {
	mu           sync.Mutex
	requestBytes int64
	limit        int64
	input        int64
	cacheRead    int64
	cacheWrite   int64
	seen         bool
	native       bool
	model        string
	routeID      string
	requestID    string
	upstreamID   string
	err          error
	reported     bool
}

func prepareClaudeUsageGuard(path string, source io.ReadCloser, routeID, requestID string) (io.ReadCloser, *claudeUsageGuard, error) {
	path = strings.Split(path, "?")[0]
	if path != "/v1/messages" && path != "/v1/chat/completions" {
		return source, nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(source, maxProtocolResponseBytes+1))
	closeErr := source.Close()
	if err != nil {
		return nil, nil, err
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if len(raw) > maxProtocolResponseBytes {
		return nil, nil, errors.New("usage guard request exceeds limit")
	}
	body := io.NopCloser(bytes.NewReader(raw))
	root := gjson.ParseBytes(raw)
	model := root.Get("model").String()
	if !gjson.ValidBytes(raw) || !strings.HasPrefix(strings.ToLower(model), "claude-") || !root.Get("messages").IsArray() || !selfContainedClaudeText(root) {
		return body, nil, nil
	}
	return body, &claudeUsageGuard{requestBytes: int64(len(raw)), limit: int64(len(raw))*2 + 65536,
		native: path == "/v1/messages", model: model, routeID: routeID, requestID: requestID}, nil
}

// Remote files, media, server tools and managed state may legitimately consume
// input absent from this request body. Exclude them rather than guess a bound.
func selfContainedClaudeText(root gjson.Result) bool {
	for _, field := range []string{"mcp_servers", "container", "context_management", "previous_response_id"} {
		if root.Get(field).Exists() {
			return false
		}
	}
	for _, tool := range root.Get("tools").Array() {
		kind := tool.Get("type").String()
		if kind != "" && kind != "function" && kind != "custom" {
			return false
		}
	}
	var contentOK func(gjson.Result) bool
	contentOK = func(value gjson.Result) bool {
		if value.Type == gjson.String || value.Type == gjson.Null {
			return true
		}
		if !value.IsArray() {
			return false
		}
		for _, block := range value.Array() {
			switch block.Get("type").String() {
			case "text", "thinking", "tool_use":
			case "tool_result":
				if !contentOK(block.Get("content")) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !contentOK(root.Get("system")) {
		return false
	}
	for _, message := range root.Get("messages").Array() {
		if !contentOK(message.Get("content")) {
			return false
		}
	}
	return true
}

func (g *claudeUsageGuard) inspect(raw []byte) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	if !gjson.ValidBytes(raw) {
		return nil
	}
	root := gjson.ParseBytes(raw)
	usage := root.Get("usage")
	kind := root.Get("type").String()
	if kind == "message_start" {
		usage = root.Get("message.usage")
	}
	if !usage.IsObject() {
		return nil
	}
	// Chat prompt_tokens already includes both cache buckets. Anthropic's three
	// input buckets are disjoint. Each SSE usage object is a snapshot, not an
	// increment; absent fields retain their previous values, explicit zero wins.
	set := func(field gjson.Result, dst *int64) {
		if field.Type == gjson.Number {
			g.seen = true
			value, err := strconv.ParseInt(field.Raw, 10, 64)
			if err != nil || value < 0 {
				g.err = fmt.Errorf("%w: invalid input token count", errUpstreamUsageAnomaly)
				return
			}
			*dst, g.seen = value, true
		}
	}
	native := g.native || kind == "message_start" || kind == "message_delta"
	if native {
		set(usage.Get("input_tokens"), &g.input)
		set(usage.Get("cache_read_input_tokens"), &g.cacheRead)
		set(usage.Get("cache_creation_input_tokens"), &g.cacheWrite)
	} else {
		set(usage.Get("prompt_tokens"), &g.input)
		set(usage.Get("prompt_tokens_details.cached_tokens"), &g.cacheRead)
		set(usage.Get("prompt_tokens_details.cache_creation_tokens"), &g.cacheWrite)
		if field := usage.Get("prompt_tokens_details.cache_write_tokens"); field.Exists() {
			set(field, &g.cacheWrite)
		}
	}
	total := g.input
	if native {
		// Subtraction checks also prevent integer overflow from corrupt counts.
		if g.cacheRead > g.limit || total > g.limit-g.cacheRead {
			total = g.limit + 1
		} else {
			total += g.cacheRead
			if g.cacheWrite > g.limit || total > g.limit-g.cacheWrite {
				total = g.limit + 1
			} else {
				total += g.cacheWrite
			}
		}
	}
	if total > g.limit {
		g.err = fmt.Errorf("%w: reported input exceeds request byte bound (input=%d cache_read=%d cache_write=%d request_bytes=%d bound=%d)",
			errUpstreamUsageAnomaly, g.input, g.cacheRead, g.cacheWrite, g.requestBytes, g.limit)
	}
	if g.err != nil {
		g.reportLocked()
	}
	return g.err
}

func (g *claudeUsageGuard) report() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reportLocked()
}

func (g *claudeUsageGuard) failure() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *claudeUsageGuard) reportLocked() {
	if g.reported || !g.seen {
		return
	}
	g.reported = true
	log.Printf("[model-proxy] usage_audit route_id=%q model=%q request_id=%q upstream_request_id=%q request_bytes=%d input=%d cache_read=%d cache_write=%d bound=%d anomalous=%t",
		g.routeID, g.model, g.requestID, g.upstreamID, g.requestBytes, g.input, g.cacheRead, g.cacheWrite, g.limit, g.err != nil)
}

func (g *claudeUsageGuard) attach(response *http.Response) error {
	if g == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	g.upstreamID = response.Header.Get("X-Request-ID")
	if isEventStream(response.Header) {
		response.Body = &usageGuardSSEBody{source: bufio.NewReaderSize(response.Body, 32*1024), closer: response.Body, guard: g}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
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
		return errors.New("usage guard response exceeds limit")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	err = g.inspect(raw)
	g.report()
	return err
}

// Validate complete events without buffering the generation. When usage arrives
// after content, emit an explicit protocol error and terminate: a committed
// stream cannot safely fail over or pretend that the reported usage is valid.
type usageGuardSSEBody struct {
	source  *bufio.Reader
	closer  io.Closer
	guard   *claudeUsageGuard
	pending []byte
	err     error
}

func (b *usageGuardSSEBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		frame, err := readBufferedSSEEvent(b.source, maxProtocolResponseBytes)
		b.err = err
		var data []string
		for _, line := range strings.Split(string(frame), "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(line[5:]))
			}
		}
		if usageErr := b.guard.inspect([]byte(strings.Join(data, "\n"))); usageErr != nil {
			b.err = usageErr
			_ = b.closer.Close()
			payload := `{"type":"error","error":{"type":"api_error","code":"upstream_usage_anomaly","message":"Upstream token usage exceeds the request size safety bound; reported usage was rejected."}}`
			b.pending = []byte("event: error\ndata: " + payload + "\n\n")
		} else {
			b.pending = frame
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *usageGuardSSEBody) Close() error {
	b.guard.report()
	return b.closer.Close()
}
