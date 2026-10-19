package model_proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Wait before committing downstream headers, so an upstream that only sends
// headers or startup/heartbeat events can still be retried. Replay every byte
// once ready; the deadline no longer applies to the rest of the stream.
func prepareStreamStart(ctx context.Context, response *http.Response, timeout time.Duration) error {
	if timeout <= 0 || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || !isEventStream(response.Header) {
		return nil
	}
	health, tracked := response.Body.(*healthResponseBody)
	source := response.Body
	if tracked {
		source = health.ReadCloser
	}
	reader := bufio.NewReaderSize(source, 32*1024)
	type result struct {
		prefix []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var prefix []byte
		for len(prefix) < firstSSEEventLimit {
			event, err := readFirstSSEEvent(reader)
			prefix = append(prefix, event...)
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				done <- result{prefix, err}
				return
			}
			if len(prefix) > firstSSEEventLimit {
				break
			}
			if !isStreamStartupEvent(event) {
				done <- result{prefix, nil}
				return
			}
		}
		done <- result{nil, errSSEEventTooLarge}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		_ = source.Close()
		return ctx.Err()
	case <-timer.C:
		_ = source.Close()
		return fmt.Errorf("upstream stream start timeout after %s", timeout)
	case ready := <-done:
		if ready.err != nil {
			_ = source.Close()
			return ready.err
		}
		replay := &prefixResponseBody{Reader: io.MultiReader(bytes.NewReader(ready.prefix), reader), closer: source}
		if tracked {
			// Preserve the outer health observer used by retry/circuit decisions.
			for _, line := range bytes.Split(ready.prefix, []byte{'\n'}) {
				health.inspectSSELine(line)
			}
			health.ReadCloser = replay
		} else {
			response.Body = replay
		}
		return nil
	}
}

func isStreamStartupEvent(event []byte) bool {
	var kind string
	var data []string
	for _, line := range strings.Split(string(event), "\n") {
		field, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), ":")
		value = strings.TrimSpace(value)
		switch field {
		case "event":
			kind = value
		case "data":
			data = append(data, value)
		}
	}
	if len(data) == 0 {
		return kind != "error" && kind != "response.failed"
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &payload) != nil {
		return false
	}
	if probeJSONError(payload) {
		return false
	}
	var payloadType string
	_ = json.Unmarshal(payload["type"], &payloadType)
	if payloadType != "" {
		kind = payloadType
	}
	switch kind {
	case "response.created", "response.queued", "response.in_progress", "message_start", "ping":
		return true
	}
	var choices []struct {
		Delta        map[string]json.RawMessage `json:"delta"`
		FinishReason *string                    `json:"finish_reason"`
	}
	if json.Unmarshal(payload["choices"], &choices) == nil && len(choices) > 0 {
		for _, choice := range choices {
			if choice.FinishReason != nil {
				return false
			}
			for field, value := range choice.Delta {
				if field != "role" && string(value) != "null" && string(value) != `""` && string(value) != "[]" && string(value) != "{}" {
					return false
				}
			}
		}
		return true
	}
	return false
}
