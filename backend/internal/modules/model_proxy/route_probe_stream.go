package model_proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Keep manual tests finite, while automatic inference probes exercise the same
// streaming protocol used by stream-only members.
func newStreamingRouteProbeRequest(ctx context.Context, spec routeProbeSpec) (*http.Request, []byte, bool, error) {
	request, body, err := newRouteProbeRequest(ctx, spec)
	if err != nil {
		return nil, nil, false, err
	}
	switch spec.Path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages":
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, nil, false, err
		}
		payload["stream"] = true
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, false, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
	default:
		if _, _, ok := nativeGeminiModelAction(spec.Path); !ok {
			return request, body, false, nil
		}
		request.URL.Path = strings.Replace(request.URL.Path, ":generateContent", ":streamGenerateContent", 1)
		request.URL.RawQuery = "alt=sse"
	}
	request.Header.Set("Accept", "text/event-stream")
	return request, body, true, nil
}

// A 200 header or an initial role/heartbeat event alone cannot prove recovery.
// Read bounded SSE events until a protocol completion, rejecting truncated and
// error streams. The caller's configured timeout bounds an upstream that stalls.
func validateRouteProbeStream(body io.Reader, path string) bool {
	scanner := bufio.NewScanner(io.LimitReader(body, 1<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	state := routeProbeStreamState{path: path}
	var event string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				if done, success := state.consume(event, strings.Join(data, "\n")); done {
					return success
				}
			}
			event, data = "", nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	return false
}

type routeProbeStreamState struct {
	path       string
	seenResult bool
	finished   bool
}

func (s *routeProbeStreamState) consume(event, data string) (done, success bool) {
	if data == "[DONE]" {
		return true, s.path == "/v1/chat/completions" && s.seenResult && s.finished
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &payload) != nil || len(payload) == 0 {
		return true, false
	}
	if probeJSONError(payload) {
		return true, false
	}
	var kind string
	_ = json.Unmarshal(payload["type"], &kind)
	if kind == "" {
		kind = event
	}
	if kind == "error" || kind == "response.failed" || event == "error" {
		return true, false
	}
	switch s.path {
	case "/v1/chat/completions":
		var choices []struct {
			Delta        json.RawMessage `json:"delta"`
			FinishReason string          `json:"finish_reason"`
		}
		if raw, ok := payload["choices"]; ok {
			if json.Unmarshal(raw, &choices) != nil {
				return true, false
			}
			for _, choice := range choices {
				s.seenResult = s.seenResult || len(choice.Delta) > 0 && string(choice.Delta) != "null"
				s.finished = s.finished || choice.FinishReason != ""
			}
		}
	case "/v1/responses":
		if kind == "response.completed" || kind == "response.incomplete" {
			var response map[string]json.RawMessage
			if json.Unmarshal(payload["response"], &response) != nil || probeJSONError(response) {
				return true, false
			}
			var status string
			_ = json.Unmarshal(response["status"], &status)
			var output []json.RawMessage
			valid := json.Unmarshal(response["output"], &output) == nil && string(response["output"]) != "null"
			if kind == "response.incomplete" {
				var details struct {
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal(response["incomplete_details"], &details)
				// The deliberately small probe output cap can stop reasoning models.
				return true, valid && status == "incomplete" && details.Reason == "max_output_tokens"
			}
			return true, valid && status == "completed"
		}
	case "/v1/messages":
		switch kind {
		case "message_start":
			var message struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(payload["message"], &message) != nil || message.Role != "assistant" {
				return true, false
			}
			s.seenResult = true
		case "message_delta":
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			if json.Unmarshal(payload["delta"], &delta) != nil {
				return true, false
			}
			s.finished = delta.StopReason != ""
		case "message_stop":
			return true, s.seenResult && s.finished
		}
	default:
		if _, _, ok := nativeGeminiModelAction(s.path); ok {
			var candidates []struct {
				FinishReason string `json:"finishReason"`
			}
			if raw, ok := payload["candidates"]; ok {
				if json.Unmarshal(raw, &candidates) != nil {
					return true, false
				}
				for _, candidate := range candidates {
					if candidate.FinishReason != "" {
						return true, candidate.FinishReason == "STOP" || candidate.FinishReason == "MAX_TOKENS"
					}
				}
			}
		}
	}
	return false, false
}

func probeJSONError(payload map[string]json.RawMessage) bool {
	raw, ok := payload["error"]
	return ok && string(raw) != "null"
}
