package model_proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const healthResponseLimit = 64 << 10

// Observe errors hidden in HTTP 200 JSON/SSE without changing response bytes or
// buffering a successful stream. Keep only a bounded JSON prefix or SSE line.
type healthResponseBody struct {
	io.ReadCloser
	streaming       bool
	buffer          []byte
	failed          bool
	modelMissing    bool
	credentialError bool
	oversized       bool
}

func trackResponseHealth(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	body := &healthResponseBody{ReadCloser: response.Body, streaming: isEventStream(response.Header)}
	response.Body = body
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Inspect failures before retry code closes their bodies. Preserve the
		// prefix for non-retried responses returned to the caller.
		source := body.ReadCloser
		deadline := time.AfterFunc(2*time.Second, func() { _ = source.Close() })
		prefix, _ := io.ReadAll(io.LimitReader(source, healthResponseLimit))
		deadline.Stop()
		body.inspectJSON(prefix)
		body.ReadCloser = &prefixResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix), body.ReadCloser), closer: body.ReadCloser}
	}
}

type prefixResponseBody struct {
	io.Reader
	closer io.Closer
}

func (b *prefixResponseBody) Close() error { return b.closer.Close() }

func (b *healthResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.streaming {
		for _, c := range p[:n] {
			if c == '\n' {
				if !b.oversized {
					b.inspectSSELine(b.buffer)
				}
				b.buffer, b.oversized = b.buffer[:0], false
			} else if len(b.buffer) < healthResponseLimit {
				b.buffer = append(b.buffer, c)
			} else {
				b.oversized = true
			}
		}
	} else if !b.oversized {
		if len(b.buffer)+n <= healthResponseLimit {
			b.buffer = append(b.buffer, p[:n]...)
		} else {
			b.oversized = true
			b.buffer = nil
		}
	}
	if err == io.EOF && !b.oversized {
		if b.streaming {
			b.inspectSSELine(b.buffer)
		} else {
			b.inspectJSON(b.buffer)
		}
	}
	return n, err
}

func (b *healthResponseBody) inspectSSELine(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.Equal(line, []byte("event: error")) || bytes.Equal(line, []byte("event: response.failed")) {
		b.failed = true
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		b.inspectJSON(bytes.TrimSpace(line[5:]))
	}
}

func (b *healthResponseBody) inspectJSON(data []byte) {
	if !bytes.Contains(data, []byte(`"error"`)) && !bytes.Contains(data, []byte("response.failed")) {
		return
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	var kind string
	_ = json.Unmarshal(payload["type"], &kind)
	if kind == "error" || kind == "response.failed" {
		b.failed = true
	}
	if raw := payload["error"]; len(raw) > 0 && string(raw) != "null" {
		b.inspectError(raw)
	}
	if raw := payload["response"]; len(raw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil {
			if raw := response["error"]; len(raw) > 0 && string(raw) != "null" {
				b.inspectError(raw)
			}
		}
	}
}

func (b *healthResponseBody) inspectError(raw []byte) {
	b.failed = true
	text := strings.ToLower(string(raw))
	for _, marker := range []string{"model_not_found", "unknown_model", "unsupported_model", "model_not_supported", "model does not exist", "model not found", "model is not supported", "模型不存在", "不支持该模型"} {
		if strings.Contains(text, marker) {
			b.modelMissing = true
		}
	}
	for _, marker := range []string{"invalid_api_key", "invalid api key", "invalid authentication", "authentication_error", "invalid_token", "api key revoked"} {
		if strings.Contains(text, marker) {
			b.credentialError = true
		}
	}
}

func responseHealth(response *http.Response) *healthResponseBody {
	if response != nil {
		if body, ok := response.Body.(*healthResponseBody); ok {
			return body
		}
	}
	return &healthResponseBody{}
}
