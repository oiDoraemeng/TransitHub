package model_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const mappedModelTarget = "gpt-6-luna"

var mappedAstraCompatibleModels = map[string]struct{}{
	"gpt-6":         {},
	"gpt-6-astra":   {},
	"gpt-6-sol":     {},
	"gpt-6.1-sol":   {},
	"gpt-5.6":       {},
	"gpt-5.6-terra": {},
	"gpt-5.6-sol":   {},
	"gpt-5.5":       {},
	"gpt-5.4":       {},
}

type mappedResponseContext struct {
	Enabled         bool
	SourceModel     string
	TargetModel     string
	ResponsesAPI    bool
	ReasoningMode   string
	ReasoningEffort string
}

func newMappedResponseContext(incoming *http.Request, replay *replayBody, sourceModel string, mapping map[string]string) (mappedResponseContext, error) {
	sourceModel = strings.TrimSpace(sourceModel)
	targetModel := strings.TrimSpace(mapping[sourceModel])
	if targetModel != mappedModelTarget {
		return mappedResponseContext{}, nil
	}
	if _, ok := mappedAstraCompatibleModels[sourceModel]; !ok {
		return mappedResponseContext{}, nil
	}
	context := mappedResponseContext{
		Enabled:      true,
		SourceModel:  sourceModel,
		TargetModel:  targetModel,
		ResponsesAPI: incoming.URL.Path == "/v1/responses" || incoming.URL.Path == "/v1/responses/compact",
	}
	if !context.ResponsesAPI {
		return context, nil
	}
	reader, err := replay.Open()
	if err != nil {
		return mappedResponseContext{}, err
	}
	defer reader.Close()
	var payload map[string]any
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return mappedResponseContext{}, err
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning != nil {
		context.ReasoningMode, _ = reasoning["mode"].(string)
		context.ReasoningEffort, _ = reasoning["effort"].(string)
	}
	if context.ReasoningEffort == "" {
		context.ReasoningEffort, _ = payload["reasoning_effort"].(string)
	}
	return context, nil
}

func rewriteMappedResponse(response *http.Response, context mappedResponseContext) error {
	if !context.Enabled {
		return nil
	}
	if isEventStream(response.Header) {
		response.Body = newMappedSSEBody(response.Body, context)
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
		return fmt.Errorf("mapped response exceeds %d bytes", maxProtocolResponseBytes)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	rewriteMappedResponseObject(payload, context, context.ResponsesAPI)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	response.Header.Set("Content-Length", fmt.Sprintf("%d", len(encoded)))
	response.Header.Del("Content-Encoding")
	return nil
}

func rewriteMappedResponseObject(payload map[string]any, context mappedResponseContext, responsesAPI bool) {
	if model, ok := payload["model"].(string); ok && (model == context.TargetModel || model == "") {
		payload["model"] = context.SourceModel
	}
	if responsesAPI {
		payload["access_programs"] = map[string]any{"cyber": "standard"}
		if context.ReasoningMode != "" || context.ReasoningEffort != "" {
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = make(map[string]any)
				payload["reasoning"] = reasoning
			}
			if context.ReasoningMode != "" {
				reasoning["mode"] = context.ReasoningMode
			}
			if context.ReasoningEffort != "" {
				reasoning["effort"] = context.ReasoningEffort
			}
		}
	}
}

func rewriteMappedSSEPayload(payload map[string]any, context mappedResponseContext) {
	if nested, ok := payload["response"].(map[string]any); ok {
		rewriteMappedResponseObject(nested, context, context.ResponsesAPI)
		return
	}
	rewriteMappedResponseObject(payload, context, false)
}

type mappedSSEBody struct {
	source  *bufio.Reader
	close   io.Closer
	context mappedResponseContext
	pending []byte
}

func newMappedSSEBody(source io.ReadCloser, context mappedResponseContext) io.ReadCloser {
	return &mappedSSEBody{source: bufio.NewReaderSize(source, 32*1024), close: source, context: context}
}

func (body *mappedSSEBody) Read(p []byte) (int, error) {
	for len(body.pending) == 0 {
		line, err := body.source.ReadBytes('\n')
		if len(line) > 0 {
			body.pending = rewriteMappedSSELine(line, body.context)
		}
		if len(body.pending) > 0 {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	n := copy(p, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *mappedSSEBody) Close() error { return body.close.Close() }

func rewriteMappedSSELine(line []byte, context mappedResponseContext) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte(nil), line...)
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return append([]byte(nil), line...)
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return append([]byte(nil), line...)
	}
	rewriteMappedSSEPayload(payload, context)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return append([]byte(nil), line...)
	}
	result := make([]byte, 0, len(encoded)+len(line)-len(data))
	result = append(result, trimmed[:len(trimmed)-len(data)]...)
	result = append(result, encoded...)
	if bytes.HasSuffix(line, []byte("\r\n")) {
		result = append(result, '\r', '\n')
	} else if bytes.HasSuffix(line, []byte("\n")) {
		result = append(result, '\n')
	}
	return result
}
