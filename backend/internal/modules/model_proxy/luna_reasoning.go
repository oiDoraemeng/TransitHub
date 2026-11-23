package model_proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	if !gjson.ValidBytes(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, mapping, errors.New("request body must contain one JSON object")
	}
	raw, err = canonicalPatchObject(raw)
	if err != nil {
		return nil, mapping, err
	}
	for _, key := range []string{"reasoning", "stream_options"} {
		raw, err = canonicalPatchMetadata(raw, key)
		if err != nil {
			return nil, mapping, err
		}
	}
	fields := gjson.GetManyBytes(raw, "model", "reasoning", "reasoning_effort", "stream", "background", "audio", "modalities", "stream_options")
	model := fields[0].String()
	if _, luna := mappedModelTargets[strings.ToLower(strings.TrimSpace(model))]; !luna {
		return io.NopCloser(bytes.NewReader(raw)), mapping, nil
	}
	// Patch only policy fields. Decoding every message/tool into map[string]any
	// multiplies live allocations for multi-MiB requests at high concurrency.
	reasoning := fields[1]
	effort := reasoning.Get("effort")
	original := &clientReasoningEffort{}
	original.NestedPresent, original.FlatPresent = effort.Exists(), fields[2].Exists()
	original.Present = original.NestedPresent || original.FlatPresent
	if !original.NestedPresent {
		effort = fields[2]
	}
	if original.Present {
		decoder := json.NewDecoder(strings.NewReader(effort.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&original.Value); err != nil {
			return nil, mapping, err
		}
	}
	options := &sjson.Options{Optimistic: true, ReplaceInPlace: true}
	encoded := raw
	set := func(path string, value any) {
		if err == nil {
			encoded, err = sjson.SetBytesOptions(encoded, path, value, options)
		}
	}
	if endpoint.Path == "/v1/responses" {
		if !reasoning.IsObject() {
			set("reasoning", map[string]any{"effort": "max"})
		} else {
			set("reasoning.effort", "max")
		}
		// Conflicting legacy fields must not override the canonical value.
		if err == nil {
			encoded, err = sjson.DeleteBytes(encoded, "reasoning_effort")
		}
	} else {
		set("reasoning_effort", "max")
		if reasoning.IsObject() {
			set("reasoning.effort", "max")
		}
	}
	if err != nil {
		return nil, mapping, err
	}
	if !mapping.Enabled {
		mapping.SourceModel = model
	}
	mapping.Enabled = true
	mapping.ClientEffort = original
	// A buffered dora response may take longer than the startup timeout even
	// while the model is generating. Observe its stream internally, then return
	// the original JSON protocol after completion. Audio/background requests
	// retain their native response semantics.
	stream := fields[3]
	if stream.Type != gjson.True && (stream.Type == gjson.False || stream.Type == gjson.Null) && !fields[4].Bool() && fields[5].Type == gjson.Null && fields[6].Type == gjson.Null {
		mapping.BufferedStream = true
		set("stream", true)
		if endpoint.Path == "/v1/chat/completions" {
			if !fields[7].IsObject() {
				set("stream_options", map[string]any{"include_usage": true})
			} else {
				set("stream_options.include_usage", true)
			}
		}
	}
	if err != nil {
		return nil, mapping, err
	}
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
