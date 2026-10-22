package model_proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultLocalReplyMinTokens = 100

const defaultLocalReplyMaxOutputTokens = 20
const defaultLocalReplyText = "OK"

func localReplyOutputSettings(maxOutput *int, text *string) (int, string, error) {
	limit, reply := defaultLocalReplyMaxOutputTokens, defaultLocalReplyText
	if maxOutput != nil {
		limit = *maxOutput
	}
	if limit < 0 || limit > 1000000 {
		return 0, "", &requestError{Status: http.StatusBadRequest, Message: "local reply maximum output tokens must be an integer between 0 and 1000000"}
	}
	if text != nil {
		reply = *text
	}
	if strings.TrimSpace(reply) == "" || utf8.RuneCountInString(reply) > 4000 {
		return 0, "", &requestError{Status: http.StatusBadRequest, Message: "local reply text must contain 1 to 4000 characters"}
	}
	return limit, reply, nil
}

func localReplyMinTokens(value *int) (int, error) {
	if value == nil {
		return defaultLocalReplyMinTokens, nil
	}
	if *value < 1 || *value > 1000000 {
		return 0, &requestError{Status: http.StatusBadRequest, Message: "local reply minimum tokens must be an integer between 1 and 1000000"}
	}
	return *value, nil
}

// Run before member lookup, mapping, model refresh and concurrency acquisition.
// These compatibility replies are not upstream requests or health signals.
func (s *Service) serveGroupLocalReply(w http.ResponseWriter, r *http.Request, group SmartGroup, replay *replayBody, model string) bool {
	if !group.LocalReplyEnabled || r.Method != http.MethodPost {
		return false
	}
	input, err := replay.Object()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return true
	}
	// An explicit small output cap is a separate rule. It intentionally also
	// applies to large, cached, keyword-matching and opaque inputs.
	if !matchesLocalReplyOutputLimit(input, r.URL.Path, group.LocalReplyMaxOutputTokens) {
		var keywordText strings.Builder
		var inputText *strings.Builder
		if len(group.Keywords) > 0 {
			inputText = &keywordText
		}
		tokens, known := estimateLocalReplyTokens(input, inputText)
		threshold := group.LocalReplyMinTokens
		if threshold <= 0 {
			threshold = defaultLocalReplyMinTokens
		}
		if !known || tokens >= threshold || (inputText != nil && matchesAnyKeyword(keywordText.String(), group.Keywords)) {
			return false
		}
	}
	replyText := group.LocalReplyText
	if replyText == "" {
		replyText = defaultLocalReplyText
	}
	id, err := randomID("")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "failed to create response ID")
		return true
	}
	streaming, _ := input["stream"].(bool)
	created := time.Now().Unix()
	switch r.URL.Path {
	case "/v1/chat/completions":
		writeLocalChatReply(w, input, model, id, created, streaming, replyText)
	case "/v1/responses":
		writeLocalResponsesReply(w, model, id, created, streaming, replyText)
	case compactPath:
		// Compact clients replace their context with output. Retain the original
		// input when skipping compaction, rather than silently discarding history.
		output := make([]any, 0)
		switch value := input["input"].(type) {
		case []any:
			output = append(output, value...)
		case string:
			output = append(output, map[string]any{"role": "user", "content": value})
		}
		output = append(output, localResponseMessage("msg_"+id, "completed", replyText))
		writeLocalJSON(w, map[string]any{"id": "cmp_" + id, "object": "response.compaction", "created_at": created, "model": model, "output": output, "usage": localResponsesUsage()})
	case "/v1/messages":
		writeLocalMessagesReply(w, model, id, streaming, replyText)
	default:
		if _, action, ok := nativeGeminiModelAction(r.URL.Path); ok {
			writeLocalGeminiReply(w, model, id, action == "streamGenerateContent", replyText)
		} else {
			return false
		}
	}
	return true
}

func matchesLocalReplyOutputLimit(input map[string]any, path string, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	var value any
	switch path {
	case "/v1/chat/completions":
		value = input["max_completion_tokens"]
		if value == nil {
			value = input["max_tokens"]
		}
	case "/v1/responses", compactPath:
		value = input["max_output_tokens"]
	case "/v1/messages":
		value = input["max_tokens"]
	default:
		if _, _, ok := nativeGeminiModelAction(path); !ok {
			return false
		}
		config, _ := input["generationConfig"].(map[string]any)
		if config == nil {
			config, _ = input["generation_config"].(map[string]any)
		}
		value = config["maxOutputTokens"]
		if value == nil {
			value = config["max_output_tokens"]
		}
	}
	limit, ok := value.(float64)
	return ok && limit > 0 && limit <= float64(threshold) && limit == float64(int(limit))
}

// Like member input limits, this is a local text estimate (four Unicode
// characters per token), not usage reported by a model. Do not treat opaque
// context or media as zero tokens: their actual input size is unknown locally.
func estimateLocalReplyTokens(input map[string]any, inputText *strings.Builder) (int, bool) {
	if hasLocalReplyContextHint(input) {
		return 0, false
	}
	characters := 0
	for _, key := range []string{"input", "messages", "prompt", "instructions", "system", "contents", "systemInstruction", "tools", "functions"} {
		count, known := localReplyTextSize(input[key], inputText)
		if !known {
			return 0, false
		}
		characters += count
	}
	return (characters + 3) / 4, true
}

// Actual cache reads are only known after upstream inference. Preserve requests
// that explicitly use caching or reference server-side history, even when their
// visible new text is short. Without these hints we count all supplied history;
// never subtract cached tokens or use the last user message alone.
func hasLocalReplyContextHint(input map[string]any) bool {
	for _, key := range []string{
		"previous_response_id", "previousResponseId", "conversation",
		"cachedContent", "cached_content", "continuationToken", "continuation_token",
		"cache_control", "cacheControl", "prompt_cache_key", "promptCacheKey",
		"prompt_cache_retention", "prompt_cache_options", "prompt_cache_breakpoint",
	} {
		if value := input[key]; value != nil && value != "" {
			return true
		}
	}
	return false
}

func localReplyTextSize(value any, inputText *strings.Builder) (int, bool) {
	switch value := value.(type) {
	case string:
		if inputText != nil {
			inputText.WriteString(value)
			inputText.WriteByte('\n')
		}
		return utf8.RuneCountInString(value), true
	case []any:
		size := 0
		for _, item := range value {
			count, known := localReplyTextSize(item, inputText)
			if !known {
				return 0, false
			}
			size += count
		}
		return size, true
	case map[string]any:
		if hasLocalReplyContextHint(value) {
			return 0, false
		}
		kind, _ := value["type"].(string)
		switch kind {
		case "input_image", "image", "image_url", "input_file", "file", "document", "input_audio", "audio", "video", "item_reference", "compaction":
			return 0, false
		}
		size := 0
		for key, child := range value {
			switch key {
			case "inlineData", "inline_data", "fileData", "file_data", "encrypted_content", "image_url", "file_id":
				return 0, false
			case "role", "type", "id", "call_id", "tool_call_id", "status":
				continue
			}
			count, known := localReplyTextSize(child, inputText)
			if !known {
				return 0, false
			}
			size += count
		}
		return size, true
	case float64:
		// Tokenized prompt arrays must not appear to have empty input.
		return 4, true
	default:
		return 0, true
	}
}

func writeLocalJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func localReplyStream(w http.ResponseWriter) func(string, map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return func(event string, payload map[string]any) {
		if event != "" {
			_, _ = fmt.Fprintf(w, "event: %s\n", event)
		}
		_, _ = io.WriteString(w, "data: ")
		_ = json.NewEncoder(w).Encode(payload)
		_, _ = io.WriteString(w, "\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func writeLocalChatReply(w http.ResponseWriter, input map[string]any, model, id string, created int64, streaming bool, text string) {
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	base := func(object string, choices []any) map[string]any {
		return map[string]any{"id": "chatcmpl-" + id, "object": object, "created": created, "model": model, "choices": choices}
	}
	if !streaming {
		response := base("chat.completion", []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text, "refusal": nil}, "finish_reason": "stop", "logprobs": nil}})
		response["usage"] = usage
		writeLocalJSON(w, response)
		return
	}
	emit := localReplyStream(w)
	options, _ := input["stream_options"].(map[string]any)
	includeUsage := options["include_usage"] == true
	chunk := func(delta map[string]any, finish any) {
		response := base("chat.completion.chunk", []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}})
		if includeUsage {
			response["usage"] = nil
		}
		emit("", response)
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	chunk(map[string]any{"content": text}, nil)
	chunk(map[string]any{}, "stop")
	if includeUsage {
		response := base("chat.completion.chunk", []any{})
		response["usage"] = usage
		emit("", response)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func localResponsesUsage() map[string]any {
	return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

func localOutputText(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
}

func localResponseMessage(id, status, text string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, localOutputText(text))
	}
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": status, "content": content}
}

func writeLocalResponsesReply(w http.ResponseWriter, model, id string, created int64, streaming bool, text string) {
	messageID := "msg_" + id
	message := localResponseMessage(messageID, "completed", text)
	response := func(status string, output []any) map[string]any {
		var usage, completedAt any
		if status == "completed" {
			usage, completedAt = localResponsesUsage(), created
		}
		return map[string]any{"id": "resp_" + id, "object": "response", "created_at": created, "completed_at": completedAt, "status": status, "error": nil, "incomplete_details": nil, "instructions": nil, "max_output_tokens": nil, "model": model, "output": output, "parallel_tool_calls": true, "previous_response_id": nil, "reasoning": map[string]any{"effort": nil, "summary": nil}, "store": false, "temperature": 1, "text": map[string]any{"format": map[string]any{"type": "text"}}, "tool_choice": "auto", "tools": []any{}, "top_p": 1, "truncation": "disabled", "usage": usage, "metadata": map[string]any{}}
	}
	if !streaming {
		writeLocalJSON(w, response("completed", []any{message}))
		return
	}
	writeEvent := localReplyStream(w)
	sequence := 0
	emit := func(kind string, payload map[string]any) {
		payload["type"], payload["sequence_number"] = kind, sequence
		sequence++
		writeEvent(kind, payload)
	}
	emit("response.created", map[string]any{"response": response("in_progress", []any{})})
	emit("response.in_progress", map[string]any{"response": response("in_progress", []any{})})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": localResponseMessage(messageID, "in_progress", "")})
	emit("response.content_part.added", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "part": localOutputText("")})
	emit("response.output_text.delta", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "delta": text, "logprobs": []any{}})
	emit("response.output_text.done", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "text": text, "logprobs": []any{}})
	emit("response.content_part.done", map[string]any{"item_id": messageID, "output_index": 0, "content_index": 0, "part": localOutputText(text)})
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": message})
	emit("response.completed", map[string]any{"response": response("completed", []any{message})})
}

func writeLocalMessagesReply(w http.ResponseWriter, model, id string, streaming bool, text string) {
	message := map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
	if !streaming {
		writeLocalJSON(w, message)
		return
	}
	emit := localReplyStream(w)
	message["content"], message["stop_reason"] = []any{}, nil
	emit("message_start", map[string]any{"type": "message_start", "message": message})
	emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func writeLocalGeminiReply(w http.ResponseWriter, model, id string, streaming bool, text string) {
	response := map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}, "finishReason": "STOP"}}, "modelVersion": model, "responseId": id, "usageMetadata": map[string]any{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0}}
	if streaming {
		localReplyStream(w)("", response)
		return
	}
	writeLocalJSON(w, response)
}
