package model_proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type mediaInputError struct{ message string }

func (e *mediaInputError) Error() string { return e.message }
func mediaError(message string) error    { return &mediaInputError{message} }

// Native Gemini media must bypass the Chat -> Responses compatibility chain:
// that chain cannot represent audio/video and would discard these parts.
func adaptMediaUpstreamProtocol(incoming *http.Request, body io.ReadCloser) (upstreamProtocolRequest, error) {
	media, _ := incoming.Context().Value(requestMediaContextKey{}).(requestMedia)
	if !media.present() {
		return adaptUpstreamProtocol(incoming, body)
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	if closeErr != nil {
		return upstreamProtocolRequest{}, closeErr
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return upstreamProtocolRequest{}, mediaError("invalid media request JSON")
	}
	model, _, native := nativeGeminiModelAction(incoming.URL.Path)
	if native {
		if nativeGeminiModel(model) {
			return upstreamProtocolRequest{requestURI: incoming.URL.RequestURI(), body: io.NopCloser(bytes.NewReader(raw))}, nil
		}
		if media.Audio || media.Video || media.FileReference {
			return upstreamProtocolRequest{}, mediaError("this attachment requires a native Gemini route")
		}
		if normalizeGeminiMediaKeys(payload) {
			raw, err = json.Marshal(payload)
			if err != nil {
				return upstreamProtocolRequest{}, err
			}
		}
		return adaptUpstreamProtocol(incoming, io.NopCloser(bytes.NewReader(raw)))
	}
	model = firstString(payload, "model")
	if media.Audio || media.Video || media.PDF && (media.FileReference || nativeGeminiModel(model)) {
		if !nativeGeminiModel(model) {
			return upstreamProtocolRequest{}, mediaError("this attachment requires a native Gemini model")
		}
		if incoming.URL.Path != "/v1/chat/completions" && incoming.URL.Path != "/v1/responses" {
			return upstreamProtocolRequest{}, mediaError("use Chat Completions, Responses or native Gemini for audio/video input")
		}
		converted, err := openAIMediaToGemini(payload, incoming.URL.Path)
		if err != nil {
			return upstreamProtocolRequest{}, err
		}
		encoded, err := json.Marshal(converted)
		if err != nil {
			return upstreamProtocolRequest{}, err
		}
		stream, _ := payload["stream"].(bool)
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		return upstreamProtocolRequest{
			requestURI: "/v1beta/models/" + url.PathEscape(model) + ":" + action,
			body:       io.NopCloser(bytes.NewReader(encoded)),
			responseAdapter: func(response *http.Response) error {
				return geminiMediaToOpenAI(response, incoming.URL.Path, media.Model, stream)
			},
		}, nil
	}
	// Some Chat clients put every upload in image_url. A PDF must retain its
	// bytes but use the file content type before reaching the GPT converter.
	if normalizePDFContent(payload) {
		raw, err = json.Marshal(payload)
		if err != nil {
			return upstreamProtocolRequest{}, err
		}
	}
	return adaptUpstreamProtocol(incoming, io.NopCloser(bytes.NewReader(raw)))
}

func normalizeGeminiMediaKeys(value any) bool {
	changed := false
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			changed = normalizeGeminiMediaKeys(item) || changed
		}
	case map[string]any:
		for from, to := range map[string]string{"inline_data": "inlineData", "file_data": "fileData", "mime_type": "mimeType", "file_uri": "fileUri", "system_instruction": "systemInstruction"} {
			if item, ok := v[from]; ok {
				if _, present := v[to]; !present {
					v[to] = item
				}
				delete(v, from)
				changed = true
			}
		}
		for _, key := range []string{"contents", "parts", "inlineData", "fileData", "systemInstruction"} {
			changed = normalizeGeminiMediaKeys(v[key]) || changed
		}
	}
	return changed
}

func normalizePDFContent(value any) bool {
	changed := false
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			changed = normalizePDFContent(item) || changed
		}
	case map[string]any:
		kind := firstString(v, "type")
		uri := firstString(v, "image_url")
		if image, ok := v["image_url"].(map[string]any); ok {
			uri = firstString(image, "url")
		}
		if (kind == "image_url" || kind == "input_image") && strings.EqualFold(mediaMIME(uri, ""), "application/pdf") {
			delete(v, "image_url")
			if kind == "input_image" {
				v["type"], v["filename"], v["file_data"] = "input_file", "document.pdf", uri
			} else {
				v["type"], v["file"] = "file", map[string]any{"filename": "document.pdf", "file_data": uri}
			}
			changed = true
		}
		for _, key := range []string{"messages", "input", "content"} {
			changed = normalizePDFContent(v[key]) || changed
		}
	}
	return changed
}

func openAIMediaToGemini(source map[string]any, endpoint string) (map[string]any, error) {
	if n, ok := source["n"]; ok && fmt.Sprint(n) != "1" {
		return nil, mediaError("Gemini media supports one response candidate (n=1)")
	}
	var messages []any
	if endpoint == "/v1/chat/completions" {
		messages, _ = source["messages"].([]any)
	} else {
		if text, ok := source["input"].(string); ok {
			messages = []any{map[string]any{"role": "user", "content": text}}
		} else {
			messages, _ = source["input"].([]any)
		}
	}
	var contents, system []any
	if text := firstString(source, "instructions"); text != "" {
		system = append(system, map[string]any{"text": text})
	}
	toolNames := map[string]string{}
	for _, entry := range messages {
		message, ok := entry.(map[string]any)
		if !ok {
			return nil, mediaError("media messages must be objects")
		}
		kind, role := firstString(message, "type"), firstString(message, "role")
		if kind == "reasoning" {
			continue
		}
		if kind == "function_call" {
			toolNames[firstString(message, "call_id")] = firstString(message, "name")
			args, err := mediaToolArguments(message["arguments"])
			if err != nil {
				return nil, err
			}
			contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": message["name"], "args": args}}}})
			continue
		}
		if role == "tool" || kind == "function_call_output" {
			callID := firstString(message, "tool_call_id", "call_id")
			name := toolNames[callID]
			if name == "" {
				return nil, mediaError("tool output is missing its matching function call")
			}
			output := message["content"]
			if kind == "function_call_output" {
				output = message["output"]
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"output": output}}}}})
			continue
		}
		if endpoint == "/v1/responses" && kind != "" && kind != "message" {
			return nil, mediaError("unsupported Responses media input item: " + kind)
		}
		parts, err := openAIMediaParts(message["content"])
		if err != nil {
			return nil, err
		}
		if role == "system" || role == "developer" {
			system = append(system, parts...)
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, item := range calls {
				call, ok := item.(map[string]any)
				if !ok {
					return nil, mediaError("invalid tool call")
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					return nil, mediaError("only function tools are supported for Gemini media")
				}
				toolNames[firstString(call, "id")] = firstString(fn, "name")
				args, err := mediaToolArguments(fn["arguments"])
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": fn["name"], "args": args}})
			}
		}
		if len(parts) == 0 {
			continue
		}
		if role == "assistant" {
			role = "model"
		} else {
			role = "user"
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	result := map[string]any{"contents": contents}
	if len(system) > 0 {
		result["systemInstruction"] = map[string]any{"parts": system}
	}
	config := map[string]any{}
	for sourceKey, target := range map[string]string{"temperature": "temperature", "top_p": "topP", "max_tokens": "maxOutputTokens", "max_completion_tokens": "maxOutputTokens", "max_output_tokens": "maxOutputTokens"} {
		if value, ok := source[sourceKey]; ok {
			config[target] = value
		}
	}
	// The modern Chat token limit wins over the legacy value deterministically.
	if value, ok := source["max_completion_tokens"]; ok {
		config["maxOutputTokens"] = value
	}
	if stop, ok := source["stop"].(string); ok {
		config["stopSequences"] = []string{stop}
	} else if stop, ok := source["stop"]; ok {
		config["stopSequences"] = stop
	}
	format, _ := source["response_format"].(map[string]any)
	if text, ok := source["text"].(map[string]any); ok {
		if f, ok := text["format"].(map[string]any); ok {
			format = f
		}
	}
	if format != nil {
		switch firstString(format, "type") {
		case "json_object":
			config["responseMimeType"] = "application/json"
		case "json_schema":
			config["responseMimeType"] = "application/json"
			if schema, ok := format["json_schema"].(map[string]any); ok {
				config["responseSchema"] = schema["schema"]
			} else {
				config["responseSchema"] = format["schema"]
			}
		}
	}
	if len(config) > 0 {
		result["generationConfig"] = config
	}
	if tools, ok := source["tools"].([]any); ok && len(tools) > 0 {
		declarations := []any{}
		for _, value := range tools {
			tool, ok := value.(map[string]any)
			if !ok || firstString(tool, "type") != "function" {
				return nil, mediaError("only function tools are supported for Gemini media")
			}
			if fn, ok := tool["function"].(map[string]any); ok {
				tool = fn
			}
			declaration := map[string]any{"name": tool["name"]}
			for _, key := range []string{"description", "parameters"} {
				if value, ok := tool[key]; ok {
					declaration[key] = value
				}
			}
			declarations = append(declarations, declaration)
		}
		result["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	if choice, ok := source["tool_choice"]; ok {
		mode := "AUTO"
		names := []string{}
		if s, ok := choice.(string); ok {
			if s == "none" {
				mode = "NONE"
			}
			if s == "required" {
				mode = "ANY"
			}
		}
		if value, ok := choice.(map[string]any); ok {
			name := firstString(value, "name")
			if fn, ok := value["function"].(map[string]any); ok {
				name = firstString(fn, "name")
			}
			if name != "" {
				mode = "ANY"
				names = append(names, name)
			}
		}
		calling := map[string]any{"mode": mode}
		if len(names) > 0 {
			calling["allowedFunctionNames"] = names
		}
		result["toolConfig"] = map[string]any{"functionCallingConfig": calling}
	}
	return result, nil
}

func mediaToolArguments(value any) (any, error) {
	if text, ok := value.(string); ok {
		var args any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&args) != nil {
			return nil, mediaError("invalid function arguments JSON")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, mediaError("invalid function arguments JSON")
		}
		return args, nil
	}
	if value == nil {
		return map[string]any{}, nil
	}
	return value, nil
}

func openAIMediaParts(content any) ([]any, error) {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil, nil
		}
		return []any{map[string]any{"text": text}}, nil
	}
	if content == nil {
		return nil, nil
	}
	items, ok := content.([]any)
	if !ok {
		return nil, mediaError("invalid media content array")
	}
	var parts []any
	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			return nil, mediaError("invalid media content part")
		}
		kind := firstString(part, "type")
		if kind == "text" || kind == "input_text" || kind == "output_text" {
			parts = append(parts, map[string]any{"text": part["text"]})
			continue
		}
		uri, filename, mime := "", "", ""
		switch kind {
		case "image_url", "input_image", "video_url", "input_video", "audio_url":
			key := "image_url"
			if kind == "video_url" || kind == "input_video" {
				key = "video_url"
			}
			if kind == "audio_url" {
				key = "audio_url"
			}
			uri = firstString(part, key)
			if value, ok := part[key].(map[string]any); ok {
				uri = firstString(value, "url")
			}
			mime = mediaMIME(uri, "")
			if mime == "" && key == "image_url" {
				mime = "image/jpeg"
			}
			if mime == "" && key == "video_url" {
				mime = "video/mp4"
			}
		case "input_audio":
			value, ok := part["input_audio"].(map[string]any)
			if !ok {
				return nil, mediaError("input_audio requires data and format")
			}
			format := strings.ToLower(firstString(value, "format"))
			if format != "wav" && format != "mp3" {
				return nil, mediaError("supported audio formats are wav and mp3")
			}
			mime = "audio/wav"
			if format == "mp3" {
				mime = "audio/mpeg"
			}
			uri = "data:" + mime + ";base64," + firstString(value, "data")
		case "file", "input_file":
			value := part
			if file, ok := part["file"].(map[string]any); ok {
				value = file
			}
			filename = firstString(value, "filename")
			uri = firstString(value, "file_data", "file_url")
			if uri == "" {
				return nil, mediaError("file input requires inline file_data or a Gemini-accessible file_url; OpenAI file IDs cannot be reused")
			}
			mime = mediaMIME(uri, filename)
			if !strings.HasPrefix(uri, "data:") && !strings.Contains(uri, "://") {
				uri = "data:" + mime + ";base64," + uri
			}
		default:
			return nil, mediaError("unsupported media content type: " + kind)
		}
		if uri == "" || mime == "" {
			return nil, mediaError("attachment requires a URL/data URI with a known MIME type")
		}
		if strings.HasPrefix(strings.ToLower(uri), "data:") {
			comma := strings.IndexByte(uri, ',')
			if comma < 0 || !strings.HasSuffix(strings.ToLower(uri[:comma]), ";base64") {
				return nil, mediaError("attachment must use a base64 data URI")
			}
			data := uri[comma+1:]
			if decoded, err := base64.StdEncoding.DecodeString(data); err != nil || len(decoded) == 0 {
				return nil, mediaError("attachment contains invalid or empty Base64 data")
			}
			parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}})
		} else {
			parsed, err := url.Parse(uri)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http" && parsed.Scheme != "gs") {
				return nil, mediaError("unsupported attachment URL scheme")
			}
			parts = append(parts, map[string]any{"fileData": map[string]any{"mimeType": mime, "fileUri": uri}})
		}
	}
	return parts, nil
}

type geminiMediaReply struct {
	Candidates []struct {
		Index   int `json:"index"`
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				Thought      bool   `json:"thought"`
				FunctionCall *struct {
					Name string `json:"name"`
					Args any    `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		Finish string `json:"finishReason"`
	} `json:"candidates"`
	Usage struct {
		Input   int `json:"promptTokenCount"`
		Output  int `json:"candidatesTokenCount"`
		Thought int `json:"thoughtsTokenCount"`
		Cached  int `json:"cachedContentTokenCount"`
		Total   int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error json.RawMessage `json:"error"`
}

type mediaReplyWriter struct {
	endpoint, model, id string
	created             int64
	text                strings.Builder
	tools               []any
	usage               map[string]any
	finish              string
	finished            bool
}

func newMediaReplyWriter(endpoint, model string) *mediaReplyWriter {
	id, _ := randomID("media_")
	return &mediaReplyWriter{endpoint: endpoint, model: model, id: id, created: time.Now().Unix(), finish: "stop"}
}
func (w *mediaReplyWriter) consume(reply geminiMediaReply) string {
	var delta strings.Builder
	for _, candidate := range reply.Candidates {
		if candidate.Index != 0 {
			continue
		}
		if candidate.Finish != "" {
			w.finished = true
		}
		for _, part := range candidate.Content.Parts {
			if !part.Thought {
				delta.WriteString(part.Text)
			}
			if part.FunctionCall != nil {
				args, _ := json.Marshal(part.FunctionCall.Args)
				id, _ := randomID("call_")
				w.tools = append(w.tools, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": part.FunctionCall.Name, "arguments": string(args)}})
			}
		}
		switch candidate.Finish {
		case "MAX_TOKENS":
			w.finish = "length"
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
			w.finish = "content_filter"
		}
	}
	w.text.WriteString(delta.String())
	if reply.Usage.Total > 0 {
		w.usage = map[string]any{"prompt_tokens": reply.Usage.Input, "completion_tokens": reply.Usage.Output + reply.Usage.Thought, "total_tokens": reply.Usage.Total, "prompt_tokens_details": map[string]any{"cached_tokens": reply.Usage.Cached}, "completion_tokens_details": map[string]any{"reasoning_tokens": reply.Usage.Thought}}
	}
	if len(w.tools) > 0 && w.finish == "stop" {
		w.finish = "tool_calls"
	}
	return delta.String()
}
func (w *mediaReplyWriter) object() map[string]any {
	if w.endpoint == "/v1/chat/completions" {
		message := map[string]any{"role": "assistant", "content": w.text.String()}
		if len(w.tools) > 0 {
			message["tool_calls"] = w.tools
		}
		return map[string]any{"id": w.id, "object": "chat.completion", "created": w.created, "model": w.model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": w.finish}}, "usage": w.usage}
	}
	output := []any{map[string]any{"id": "msg_" + w.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": w.text.String(), "annotations": []any{}}}}}
	for _, value := range w.tools {
		tool := value.(map[string]any)
		fn := tool["function"].(map[string]any)
		output = append(output, map[string]any{"type": "function_call", "id": tool["id"], "call_id": tool["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
	}
	status := "completed"
	var incomplete any
	if w.finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	usage := map[string]any{}
	if w.usage != nil {
		usage = map[string]any{"input_tokens": w.usage["prompt_tokens"], "output_tokens": w.usage["completion_tokens"], "total_tokens": w.usage["total_tokens"], "input_tokens_details": w.usage["prompt_tokens_details"], "output_tokens_details": w.usage["completion_tokens_details"]}
	}
	return map[string]any{"id": w.id, "object": "response", "created_at": w.created, "model": w.model, "status": status, "output": output, "usage": usage, "error": nil, "incomplete_details": incomplete}
}

func geminiMediaToOpenAI(response *http.Response, endpoint, model string, stream bool) error {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	builder := newMediaReplyWriter(endpoint, model)
	if !isEventStream(response.Header) {
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
		response.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > maxProtocolResponseBytes {
			return fmt.Errorf("Gemini response exceeds media conversion limit")
		}
		var reply geminiMediaReply
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&reply); err != nil {
			return err
		}
		if len(reply.Error) > 0 && string(reply.Error) != "null" {
			return fmt.Errorf("Gemini media response reported an error")
		}
		if !stream {
			builder.consume(reply)
			if !builder.finished {
				return io.ErrUnexpectedEOF
			}
			encoded, err := json.Marshal(builder.object())
			if err != nil {
				return err
			}
			response.Body = io.NopCloser(bytes.NewReader(encoded))
			response.ContentLength = int64(len(encoded))
			response.Header.Set("Content-Type", "application/json")
			response.Header.Del("Content-Length")
			response.Header.Del("Content-Encoding")
			return nil
		}
		response.Body = io.NopCloser(strings.NewReader("data: " + string(raw) + "\n\n"))
		response.Header.Set("Content-Type", "text/event-stream")
	}
	if !stream {
		source := response.Body
		defer source.Close()
		buffer := bufio.NewReader(source)
		bytesRead := 0
		for {
			event, err := readFirstSSEEvent(buffer)
			bytesRead += len(event)
			if bytesRead > maxProtocolResponseBytes {
				return fmt.Errorf("Gemini response exceeds media conversion limit")
			}
			data := geminiMediaEventData(event)
			if data != "" && data != "[DONE]" {
				var reply geminiMediaReply
				decoder := json.NewDecoder(strings.NewReader(data))
				decoder.UseNumber()
				if decoder.Decode(&reply) != nil {
					return fmt.Errorf("invalid Gemini media event")
				}
				if len(reply.Error) > 0 && string(reply.Error) != "null" {
					return fmt.Errorf("Gemini media stream reported an error")
				}
				builder.consume(reply)
			}
			if err != nil {
				if err != io.EOF {
					return err
				}
				break
			}
		}
		if !builder.finished {
			return io.ErrUnexpectedEOF
		}
		encoded, err := json.Marshal(builder.object())
		if err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(encoded))
		response.ContentLength = int64(len(encoded))
		response.Header.Set("Content-Type", "application/json")
		response.Header.Del("Content-Length")
		response.Header.Del("Content-Encoding")
		return nil
	}
	source := response.Body
	reader, writer := io.Pipe()
	response.Body = &mediaStreamBody{PipeReader: reader, source: source}
	response.ContentLength = -1
	response.Header.Set("Content-Type", "text/event-stream")
	response.Header.Del("Content-Length")
	response.Header.Del("Content-Encoding")
	go func() {
		defer source.Close()
		defer writer.Close()
		write := func(event string, value any) error {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if event != "" {
				if _, err = fmt.Fprintf(writer, "event: %s\n", event); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(writer, "data: %s\n\n", raw)
			return err
		}
		sequence := 0
		bytesRead := 0
		emit := func(kind string, fields map[string]any) error {
			fields["type"] = kind
			fields["sequence_number"] = sequence
			sequence++
			return write(kind, fields)
		}
		if endpoint == "/v1/responses" {
			r := builder.object()
			r["status"] = "in_progress"
			r["output"] = []any{}
			if emit("response.created", map[string]any{"response": r}) != nil {
				return
			}
		}
		textStarted := false
		startText := func() error {
			if textStarted {
				return nil
			}
			textStarted = true
			if err := emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_" + builder.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
				return err
			}
			return emit("response.content_part.added", map[string]any{"item_id": "msg_" + builder.id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		buffer := bufio.NewReaderSize(source, 32*1024)
		for {
			event, readErr := readFirstSSEEvent(buffer)
			bytesRead += len(event)
			if bytesRead > maxProtocolResponseBytes {
				writer.CloseWithError(fmt.Errorf("Gemini response exceeds media conversion limit"))
				return
			}
			data := geminiMediaEventData(event)
			if data != "" && data != "[DONE]" {
				var reply geminiMediaReply
				decoder := json.NewDecoder(strings.NewReader(data))
				decoder.UseNumber()
				if err := decoder.Decode(&reply); err != nil {
					writer.CloseWithError(err)
					return
				}
				if len(reply.Error) > 0 && string(reply.Error) != "null" {
					if endpoint == "/v1/responses" {
						_ = emit("error", map[string]any{"error": json.RawMessage(reply.Error)})
					} else {
						_ = write("", map[string]any{"error": json.RawMessage(reply.Error)})
					}
					return
				}
				delta := builder.consume(reply)
				if endpoint == "/v1/chat/completions" {
					if delta != "" {
						if write("", map[string]any{"id": builder.id, "object": "chat.completion.chunk", "created": builder.created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": delta}, "finish_reason": nil}}}) != nil {
							return
						}
					}
				} else if delta != "" {
					if startText() != nil {
						return
					}
					if emit("response.output_text.delta", map[string]any{"item_id": "msg_" + builder.id, "output_index": 0, "content_index": 0, "delta": delta}) != nil {
						return
					}
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					writer.CloseWithError(readErr)
					return
				}
				break
			}
		}
		if !builder.finished {
			writer.CloseWithError(io.ErrUnexpectedEOF)
			return
		}
		if endpoint == "/v1/chat/completions" {
			delta := map[string]any{}
			if len(builder.tools) > 0 {
				tools := []any{}
				for index, value := range builder.tools {
					tool := value.(map[string]any)
					tool["index"] = index
					tools = append(tools, tool)
				}
				delta["tool_calls"] = tools
			}
			if write("", map[string]any{"id": builder.id, "object": "chat.completion.chunk", "created": builder.created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": builder.finish}}}) != nil {
				return
			}
			if builder.usage != nil {
				if write("", map[string]any{"id": builder.id, "object": "chat.completion.chunk", "created": builder.created, "model": model, "choices": []any{}, "usage": builder.usage}) != nil {
					return
				}
			}
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		} else {
			if startText() != nil {
				return
			}
			if emit("response.output_text.done", map[string]any{"item_id": "msg_" + builder.id, "output_index": 0, "content_index": 0, "text": builder.text.String()}) != nil {
				return
			}
			result := builder.object()
			output := result["output"].([]any)
			if emit("response.content_part.done", map[string]any{"item_id": "msg_" + builder.id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": builder.text.String(), "annotations": []any{}}}) != nil {
				return
			}
			for index, item := range output {
				if index > 0 {
					if emit("response.output_item.added", map[string]any{"output_index": index, "item": item}) != nil {
						return
					}
				}
				if emit("response.output_item.done", map[string]any{"output_index": index, "item": item}) != nil {
					return
				}
			}
			kind := "response.completed"
			if result["status"] == "incomplete" {
				kind = "response.incomplete"
			}
			_ = emit(kind, map[string]any{"response": result})
		}
	}()
	return nil
}

func geminiMediaEventData(event []byte) string {
	var lines []string
	for _, line := range strings.Split(string(event), "\n") {
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(line[5:]))
		}
	}
	return strings.Join(lines, "\n")
}

type mediaStreamBody struct {
	*io.PipeReader
	source io.ReadCloser
}

func (b *mediaStreamBody) Close() error { _ = b.source.Close(); return b.PipeReader.Close() }
