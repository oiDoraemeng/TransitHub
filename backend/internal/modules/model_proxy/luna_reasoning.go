package model_proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
)

// Keep the client's metadata separate from the upstream-only policy. In
// particular, an omitted effort must never leak our injected max downstream.
type clientReasoningEffort struct {
	Value         any
	Present       bool
	NestedPresent bool
	FlatPresent   bool
}

func forceDoraLunaReasoning(baseURL, requestURI string, body io.ReadCloser, mapping mappedResponseContext) (io.ReadCloser, mappedResponseContext, error) {
	site, err := url.Parse(baseURL)
	if err != nil || (!strings.EqualFold(site.Hostname(), "dora2api.xyz") && !strings.EqualFold(site.Hostname(), "www.dora2api.xyz")) {
		return body, mapping, nil
	}
	endpoint, err := url.ParseRequestURI(requestURI)
	if err != nil || (endpoint.Path != "/v1/responses" && endpoint.Path != "/v1/chat/completions") {
		return body, mapping, nil
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil {
		return nil, mapping, err
	}
	if closeErr != nil {
		return nil, mapping, closeErr
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // Preserve large integers in tool arguments and metadata.
	if err := decoder.Decode(&payload); err != nil {
		return nil, mapping, err
	}
	model, _ := payload["model"].(string)
	if _, luna := mappedModelTargets[strings.ToLower(strings.TrimSpace(model))]; !luna {
		return io.NopCloser(bytes.NewReader(raw)), mapping, nil
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	original := &clientReasoningEffort{}
	original.Value, original.NestedPresent = reasoning["effort"]
	flat, flatPresent := payload["reasoning_effort"]
	original.FlatPresent = flatPresent
	original.Present = original.NestedPresent || flatPresent
	if !original.NestedPresent {
		original.Value = flat
	}
	if endpoint.Path == "/v1/responses" {
		if reasoning == nil {
			reasoning = make(map[string]any)
			payload["reasoning"] = reasoning
		}
		reasoning["effort"] = "max"
		// Conflicting legacy fields must not override the canonical value.
		delete(payload, "reasoning_effort")
	} else {
		payload["reasoning_effort"] = "max"
		if reasoning != nil {
			reasoning["effort"] = "max"
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, mapping, err
	}
	if !mapping.Enabled {
		mapping.SourceModel = model
	}
	mapping.Enabled = true
	mapping.ClientEffort = original
	return io.NopCloser(bytes.NewReader(encoded)), mapping, nil
}

func restoreClientReasoningEffort(payload map[string]any, original *clientReasoningEffort) {
	if original == nil {
		return
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning != nil || original.NestedPresent {
		if original.Present {
			if reasoning == nil {
				reasoning = make(map[string]any)
				payload["reasoning"] = reasoning
			}
			reasoning["effort"] = original.Value
		} else {
			delete(reasoning, "effort")
			if len(reasoning) == 0 {
				delete(payload, "reasoning")
			}
		}
	}
	if _, exists := payload["reasoning_effort"]; exists || original.FlatPresent {
		if original.Present {
			payload["reasoning_effort"] = original.Value
		} else {
			delete(payload, "reasoning_effort")
		}
	}
}
