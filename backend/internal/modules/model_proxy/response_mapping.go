package model_proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var mappedModelTargets = map[string]struct{}{
	"gpt-6-luna":   {},
	"gpt-5.6-luna": {},
}

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
	Enabled             bool
	CompatibilityFields bool
	SourceModel         string
	TargetModel         string
	ResponsesAPI        bool
	ReasoningMode       string
	ReasoningEffort     string
	ClientEffort        *clientReasoningEffort
	BufferedStream      bool
	GeminiModelDetail   bool
}

func newMappedResponseContext(incoming *http.Request, replay *replayBody, sourceModel string, mapping map[string]string) (mappedResponseContext, error) {
	sourceModel = strings.TrimSpace(sourceModel)
	targetModel := strings.TrimSpace(mapping[sourceModel])
	if sourceModel == "" {
		return mappedResponseContext{}, nil
	}
	_, compatibleTarget := mappedModelTargets[targetModel]
	_, compatibleSource := mappedAstraCompatibleModels[sourceModel]
	compatibilityFields := targetModel != sourceModel && compatibleTarget && compatibleSource
	context := mappedResponseContext{
		Enabled:             true,
		CompatibilityFields: compatibilityFields,
		SourceModel:         sourceModel,
		TargetModel:         targetModel,
		ResponsesAPI:        incoming.URL.Path == "/v1/responses" || incoming.URL.Path == "/v1/responses/compact",
		GeminiModelDetail:   incoming.Method == http.MethodGet && strings.HasPrefix(incoming.URL.Path, "/v1beta/models/"),
	}
	if !context.ResponsesAPI || !context.CompatibilityFields {
		return context, nil
	}
	payload, err := replay.Object()
	if err != nil {
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
		return fmt.Errorf("mapped response exceeds %d bytes", maxProtocolResponseBytes)
	}
	encoded, err := rewriteMappedJSON(raw, context, false)
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
	if _, ok := payload["model"]; ok {
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
	if hasMappedResponseMetadata(payload) {
		restoreClientReasoningEffort(payload, context.ClientEffort)
	}
}

// Patch protocol metadata only. In particular, decoding the entire response
// through map[string]any would round tool arguments containing large integers.
func rewriteMappedJSON(raw []byte, context mappedResponseContext, streaming bool) ([]byte, error) {
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("invalid model response JSON")
	}
	root := gjson.ParseBytes(raw)
	// Native Gemini streamGenerateContent without alt=sse returns a JSON array.
	if root.IsArray() {
		result := raw
		for index, item := range root.Array() {
			patched, err := rewriteMappedJSON([]byte(item.Raw), context, streaming)
			if err != nil {
				return nil, err
			}
			result, err = sjson.SetRawBytes(result, strconv.Itoa(index), patched)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	if !root.IsObject() {
		return nil, fmt.Errorf("model response must be a JSON object")
	}
	patchObject := func(data []byte, responsesAPI bool, detail bool) ([]byte, error) {
		metadata := make(map[string]any)
		for _, key := range []string{"model", "access_programs", "reasoning", "reasoning_effort"} {
			value := gjson.GetBytes(data, key)
			if value.Exists() {
				decoder := json.NewDecoder(strings.NewReader(value.Raw))
				decoder.UseNumber()
				var decoded any
				if err := decoder.Decode(&decoded); err != nil {
					return nil, err
				}
				metadata[key] = decoded
			}
		}
		rewriteMappedResponseObject(metadata, context, responsesAPI)
		for _, key := range []string{"model", "access_programs", "reasoning", "reasoning_effort"} {
			var err error
			if value, exists := metadata[key]; exists {
				data, err = sjson.SetBytes(data, key, value)
			} else if gjson.GetBytes(data, key).Exists() {
				data, err = sjson.DeleteBytes(data, key)
			}
			if err != nil {
				return nil, err
			}
		}
		fields := map[string]string{"modelVersion": context.SourceModel}
		if detail {
			fields["name"] = "models/" + strings.TrimPrefix(context.SourceModel, "models/")
			fields["baseModelId"] = strings.TrimPrefix(context.SourceModel, "models/")
			fields["displayName"] = context.SourceModel
		}
		for field, value := range fields {
			if gjson.GetBytes(data, field).Exists() {
				var err error
				data, err = sjson.SetBytes(data, field, value)
				if err != nil {
					return nil, err
				}
			}
		}
		return data, nil
	}
	metadata := gjson.GetManyBytes(raw, "model", "access_programs", "reasoning")
	hasMetadata := metadata[0].Exists() || metadata[1].Exists() || metadata[2].Exists()
	result, err := patchObject(raw, context.ResponsesAPI && context.CompatibilityFields && (!streaming || hasMetadata), context.GeminiModelDetail)
	if err != nil {
		return nil, err
	}
	if streaming {
		nestedField := ""
		if context.ResponsesAPI || strings.HasPrefix(gjson.GetBytes(raw, "type").String(), "response.") {
			nestedField = "response"
		}
		if gjson.GetBytes(raw, "type").String() == "message_start" {
			nestedField = "message"
		}
		if nested := gjson.GetBytes(result, nestedField); nestedField != "" && nested.IsObject() {
			hasMetadata := nested.Get("model").Exists() || nested.Get("access_programs").Exists() || nested.Get("reasoning").Exists()
			patched, err := patchObject([]byte(nested.Raw), context.ResponsesAPI && context.CompatibilityFields && hasMetadata, false)
			if err != nil {
				return nil, err
			}
			return sjson.SetRawBytes(result, nestedField, patched)
		}
	}
	return result, nil
}

func hasMappedResponseMetadata(payload map[string]any) bool {
	for _, key := range []string{"model", "access_programs", "reasoning"} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

type mappedSSEBody struct {
	source     *bufio.Reader
	close      io.Closer
	context    mappedResponseContext
	pending    []byte
	pendingErr error
}

func newMappedSSEBody(source io.ReadCloser, context mappedResponseContext) io.ReadCloser {
	return &mappedSSEBody{source: bufio.NewReaderSize(source, 32*1024), close: source, context: context}
}

func (body *mappedSSEBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 {
		if body.pendingErr != nil {
			return 0, body.pendingErr
		}
		var frame []byte
		for {
			line, err := body.source.ReadBytes('\n')
			frame = append(frame, line...)
			if err != nil {
				body.pendingErr = err
				break
			}
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				break
			}
		}
		body.pending = rewriteMappedSSEEvent(frame, body.context)
	}
	n := copy(p, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *mappedSSEBody) Close() error { return body.close.Close() }

// SSE permits a JSON event to span several data fields. Keep other fields and
// framing intact while emitting the rewritten JSON in the first data field.
func rewriteMappedSSEEvent(frame []byte, context mappedResponseContext) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data []byte
	first, count := -1, 0
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
		count++
	}
	if first < 0 {
		return frame
	}
	if count == 1 {
		lines[first] = rewriteMappedSSELine(lines[first], context)
		return bytes.Join(lines, nil)
	}
	encoded, err := rewriteMappedJSON(data, context, true)
	if err != nil {
		return frame
	}
	var result []byte
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if i != first {
				continue
			}
			result = append(result, []byte("data: ")...)
			result = append(result, encoded...)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				result = append(result, '\r', '\n')
			} else if bytes.HasSuffix(line, []byte("\n")) {
				result = append(result, '\n')
			}
		} else {
			result = append(result, line...)
		}
	}
	return result
}

func rewriteMappedSSELine(line []byte, context mappedResponseContext) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte(nil), line...)
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return append([]byte(nil), line...)
	}
	encoded, err := rewriteMappedJSON(data, context, true)
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
