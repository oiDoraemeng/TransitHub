package model_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Finish the startup deadline exactly once. Once semantic output arrives,
// generation uses the downstream context; a completed timer cannot race a
// successful startup and cancel an accepted response.
type upstreamStartupGuard struct {
	state   atomic.Int32 // 0: waiting, 1: accepted, 2: timed out
	timer   *time.Timer
	start   time.Time
	elapsed time.Duration
}

func newUpstreamStartupGuard(timeout time.Duration, cancel func()) *upstreamStartupGuard {
	g := &upstreamStartupGuard{start: time.Now()}
	g.timer = time.AfterFunc(timeout, func() {
		if g.state.CompareAndSwap(0, 2) {
			cancel()
		}
	})
	return g
}

func (g *upstreamStartupGuard) accept() {
	if g != nil && g.state.CompareAndSwap(0, 1) {
		g.elapsed = max(time.Nanosecond, time.Since(g.start))
		g.timer.Stop()
	}
}

type bufferedChatChoice struct {
	message  map[string]any
	tools    map[int]map[string]any
	finish   any
	logprobs map[string]any
}

// Consume a bounded SSE response before committing downstream headers. Require
// an explicit terminal event, keep usage/tool calls, and reject truncated or
// in-band failed streams instead of returning a partial HTTP 200 JSON response.
func bufferDoraStream(response *http.Response, responsesAPI bool) error {
	if !isEventStream(response.Header) {
		return nil // An upstream ignoring stream=true may still return valid JSON.
	}
	source := response.Body
	defer source.Close()
	// Bound a stream that starts then stalls. This is an idle deadline, not a
	// total generation deadline; active long responses can complete normally.
	idle := time.AfterFunc(smartGroupFirstResponseHeaderTimeout, func() { _ = source.Close() })
	defer idle.Stop()
	reader := bufio.NewReaderSize(io.LimitReader(source, maxProtocolResponseBytes+1), 32*1024)
	choices := make(map[int]*bufferedChatChoice)
	result := make(map[string]any)
	total := 0
	terminal := false
	for {
		event, err := readBufferedSSEEvent(reader, maxProtocolResponseBytes-total)
		total += len(event)
		if total > maxProtocolResponseBytes {
			return fmt.Errorf("buffered stream exceeds %d bytes", maxProtocolResponseBytes)
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		idle.Reset(smartGroupFirstResponseHeaderTimeout)
		var lines []string
		for _, line := range strings.Split(string(event), "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(lines) == 0 {
			continue
		}
		data := strings.Join(lines, "\n")
		if data == "[DONE]" {
			if responsesAPI {
				return io.ErrUnexpectedEOF
			}
			terminal = true
			break
		}
		var chunk map[string]any
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&chunk); err != nil {
			return fmt.Errorf("decode buffered stream: %w", err)
		}
		if responseHealth(response).failed || probeJSONErrorRaw(chunk) {
			return errors.New("upstream stream reported an error")
		}
		if responsesAPI {
			kind, _ := chunk["type"].(string)
			if kind == "response.completed" || kind == "response.incomplete" {
				final, ok := chunk["response"].(map[string]any)
				if !ok || final["error"] != nil || (final["status"] != "completed" && final["status"] != "incomplete") {
					return errors.New("upstream response has no successful terminal object")
				}
				result, terminal = final, true
				break
			}
			continue
		}
		for key, value := range chunk {
			if key != "choices" && key != "object" && value != nil {
				result[key] = value
			}
		}
		items, _ := chunk["choices"].([]any)
		for _, item := range items {
			c, ok := item.(map[string]any)
			if !ok {
				return errors.New("invalid streamed chat choice")
			}
			index, err := streamIndex(c["index"])
			if err != nil {
				return err
			}
			choice := choices[index]
			if choice == nil {
				choice = &bufferedChatChoice{message: map[string]any{"role": "assistant", "content": nil}, tools: make(map[int]map[string]any)}
				choices[index] = choice
			}
			if finish := c["finish_reason"]; finish != nil {
				choice.finish = finish
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				for key, value := range delta {
					if value == nil {
						continue
					}
					switch key {
					case "tool_calls":
						tools, _ := value.([]any)
						for _, item := range tools {
							tool, ok := item.(map[string]any)
							if !ok {
								return errors.New("invalid streamed tool call")
							}
							i, err := streamIndex(tool["index"])
							if err != nil {
								return err
							}
							if choice.tools[i] == nil {
								choice.tools[i] = make(map[string]any)
							}
							mergeStreamFields(choice.tools[i], tool, true)
						}
					case "function_call":
						function, _ := value.(map[string]any)
						existing, _ := choice.message[key].(map[string]any)
						if existing == nil {
							existing = make(map[string]any)
							choice.message[key] = existing
						}
						mergeStreamFields(existing, function, false)
					default:
						if part, ok := value.(string); ok && key != "role" {
							appendStreamString(choice.message, key, part)
						} else if parts, ok := value.([]any); ok {
							old, _ := choice.message[key].([]any)
							choice.message[key] = append(old, parts...)
						} else {
							choice.message[key] = value
						}
					}
				}
			}
			if logs, ok := c["logprobs"].(map[string]any); ok {
				if choice.logprobs == nil {
					choice.logprobs = make(map[string]any)
				}
				for key, value := range logs {
					if parts, ok := value.([]any); ok {
						old, _ := choice.logprobs[key].([]any)
						choice.logprobs[key] = append(old, parts...)
					} else {
						choice.logprobs[key] = value
					}
				}
			}
		}
	}
	if !terminal {
		return io.ErrUnexpectedEOF
	}
	if !responsesAPI {
		if len(choices) == 0 {
			return errors.New("upstream stream has no chat choices")
		}
		indices := make([]int, 0, len(choices))
		for index := range choices {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		items := make([]any, 0, len(indices))
		for _, index := range indices {
			choice := choices[index]
			if choice.finish == nil {
				return io.ErrUnexpectedEOF
			}
			if len(choice.tools) > 0 {
				toolIndices := make([]int, 0, len(choice.tools))
				for i := range choice.tools {
					toolIndices = append(toolIndices, i)
				}
				sort.Ints(toolIndices)
				tools := make([]any, 0, len(toolIndices))
				for _, i := range toolIndices {
					tools = append(tools, choice.tools[i])
				}
				choice.message["tool_calls"] = tools
			}
			materializeStreamFields(choice.message)
			items = append(items, map[string]any{"index": index, "message": choice.message, "finish_reason": choice.finish, "logprobs": choice.logprobs})
		}
		result["object"], result["choices"] = "chat.completion", items
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", fmt.Sprint(len(encoded)))
	response.Header.Del("Content-Encoding")
	return nil
}

func readBufferedSSEEvent(reader *bufio.Reader, limit int) ([]byte, error) {
	var event []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > limit-len(event) {
			return nil, errors.New("buffered stream exceeds response limit")
		}
		event = append(event, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return event, err
		}
		if bytes.HasSuffix(event, []byte("\n\n")) || bytes.HasSuffix(event, []byte("\r\n\r\n")) {
			return event, nil
		}
	}
}

func probeJSONErrorRaw(payload map[string]any) bool {
	kind, _ := payload["type"].(string)
	return payload["error"] != nil || kind == "error" || kind == "response.failed"
}

func streamIndex(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("stream index is missing")
	}
	i, err := number.Int64()
	if err != nil || i < 0 || i > 4096 {
		return 0, errors.New("invalid stream index")
	}
	return int(i), nil
}

func mergeStreamFields(dst, src map[string]any, tool bool) {
	for key, value := range src {
		if key == "index" || value == nil {
			continue
		}
		if fields, ok := value.(map[string]any); ok {
			nested, _ := dst[key].(map[string]any)
			if nested == nil {
				nested = make(map[string]any)
				dst[key] = nested
			}
			mergeStreamFields(nested, fields, false)
		} else if part, ok := value.(string); ok && (!tool || key != "type" && key != "id") {
			appendStreamString(dst, key, part)
		} else {
			dst[key] = value
		}
	}
}

func appendStreamString(dst map[string]any, key, part string) {
	builder, ok := dst[key].(*strings.Builder)
	if !ok {
		builder = &strings.Builder{}
		if old, ok := dst[key].(string); ok {
			builder.WriteString(old)
		}
		dst[key] = builder
	}
	builder.WriteString(part)
}

func materializeStreamFields(fields map[string]any) {
	for key, value := range fields {
		switch v := value.(type) {
		case *strings.Builder:
			fields[key] = v.String()
		case map[string]any:
			materializeStreamFields(v)
		case []any:
			for _, item := range v {
				if nested, ok := item.(map[string]any); ok {
					materializeStreamFields(nested)
				}
			}
		}
	}
}
