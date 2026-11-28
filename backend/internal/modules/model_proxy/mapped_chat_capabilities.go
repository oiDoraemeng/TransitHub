package model_proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Chat requests with server capabilities share the bounded Responses adapter.
// Ordinary Chat requests continue to use their existing upstream protocol.
func chatToMappedMessages(source map[string]any) (map[string]any, error) {
	result := map[string]any{}
	for _, key := range []string{"model", "stream", "temperature", "top_p", "thinking", "container", "context_management", "mcp_servers", "output_config"} {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	if n, ok := source["n"]; ok && numberAsInt64(n) != 1 {
		return nil, mediaError("mapped server capabilities support one Chat choice")
	}
	if source["audio"] != nil || source["modalities"] != nil || source["logprobs"] == true || source["logit_bias"] != nil {
		return nil, mediaError("unsupported Chat output option for mapped server capabilities")
	}
	if value, ok := source["max_completion_tokens"]; ok {
		result["max_tokens"] = value
	} else if value, ok := source["max_tokens"]; ok {
		result["max_tokens"] = value
	}
	if stop, ok := source["stop"]; ok && stop != nil {
		if text, ok := stop.(string); ok {
			result["stop_sequences"] = []any{text}
		} else {
			result["stop_sequences"] = stop
		}
	}
	var system []any
	var messages []any
	input, _ := source["messages"].([]any)
	for _, entry := range input {
		message, ok := entry.(map[string]any)
		if !ok {
			return nil, mediaError("invalid Chat message")
		}
		role := firstString(message, "role")
		if role == "tool" {
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": message["content"]}}})
			continue
		}
		blocks, err := chatMappedContent(message["content"])
		if err != nil {
			return nil, err
		}
		if role == "system" || role == "developer" {
			for _, entry := range blocks {
				if firstString(entry.(map[string]any), "type") != "text" {
					return nil, mediaError("Chat system content must be text")
				}
			}
			system = append(system, blocks...)
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, mediaError("unsupported Chat message role")
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			if role != "assistant" {
				return nil, mediaError("Chat tool calls must belong to an assistant")
			}
			for _, entry := range calls {
				call, _ := entry.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if firstString(call, "type") != "function" {
					return nil, mediaError("unsupported Chat tool call")
				}
				var args any
				decoder := json.NewDecoder(strings.NewReader(firstString(function, "arguments")))
				decoder.UseNumber()
				if err := decoder.Decode(&args); err != nil {
					return nil, mediaError("invalid Chat function arguments")
				}
				if err := decoder.Decode(new(any)); err != io.EOF {
					return nil, mediaError("Chat function arguments must contain one JSON value")
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call["id"], "name": function["name"], "input": args})
			}
		}
		if len(blocks) > 0 {
			messages = append(messages, map[string]any{"role": role, "content": blocks})
		}
	}
	result["messages"] = messages
	if len(system) > 0 {
		result["system"] = system
	}
	var tools []any
	if definitions, ok := source["tools"].([]any); ok {
		for _, entry := range definitions {
			tool, ok := entry.(map[string]any)
			if !ok {
				return nil, mediaError("invalid Chat tool")
			}
			if firstString(tool, "type") == "function" {
				function, _ := tool["function"].(map[string]any)
				tools = append(tools, map[string]any{"type": "custom", "name": function["name"], "description": function["description"], "input_schema": function["parameters"]})
			} else {
				tools = append(tools, tool)
			}
		}
	}
	result["tools"] = tools
	switch choice := source["tool_choice"].(type) {
	case string:
		if choice == "required" {
			choice = "any"
		}
		result["tool_choice"] = map[string]any{"type": choice}
	case map[string]any:
		if firstString(choice, "type") == "function" {
			function, _ := choice["function"].(map[string]any)
			result["tool_choice"] = map[string]any{"type": "tool", "name": function["name"]}
		} else if firstString(choice, "type") == "code_interpreter" {
			result["tool_choice"] = map[string]any{"type": "tool", "name": "code_execution"}
		} else if kind := firstString(choice, "type"); kind == "web_search" || kind == "web_search_preview" {
			result["tool_choice"] = map[string]any{"type": "tool", "name": kind}
		} else {
			result["tool_choice"] = choice
		}
	}
	if format, ok := source["response_format"].(map[string]any); ok {
		if firstString(format, "type") != "json_schema" {
			return nil, mediaError("mapped server capabilities require json_schema response_format")
		}
		schema, _ := format["json_schema"].(map[string]any)
		result["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema["schema"]}}
	}
	return result, nil
}

func chatMappedContent(content any) ([]any, error) {
	if content == nil {
		return nil, nil
	}
	if text, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	entries, ok := content.([]any)
	if !ok {
		return nil, mediaError("invalid Chat content")
	}
	blocks := make([]any, 0, len(entries))
	for _, entry := range entries {
		block, ok := entry.(map[string]any)
		if !ok {
			return nil, mediaError("invalid Chat content block")
		}
		switch firstString(block, "type") {
		case "image_url":
			image, _ := block["image_url"].(map[string]any)
			uri := firstString(image, "url")
			source := map[string]any{"type": "url", "url": uri}
			if strings.HasPrefix(uri, "data:") {
				parts := strings.SplitN(strings.TrimPrefix(uri, "data:"), ";base64,", 2)
				if len(parts) != 2 {
					return nil, mediaError("unsupported Chat image data URI")
				}
				source = map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": source})
		case "file":
			file, _ := block["file"].(map[string]any)
			uri := firstString(file, "file_data", "file_url")
			if uri == "" {
				return nil, mediaError("Chat file IDs cannot transfer between providers")
			}
			source := map[string]any{"type": "url", "url": uri}
			if strings.HasPrefix(uri, "data:") {
				parts := strings.SplitN(strings.TrimPrefix(uri, "data:"), ";base64,", 2)
				if len(parts) != 2 || parts[0] != "application/pdf" {
					return nil, mediaError("mapped Chat file must be a PDF")
				}
				source = map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
			}
			blocks = append(blocks, map[string]any{"type": "document", "source": source})
		default:
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

func mappedMessagesToChat(response *http.Response, stream bool) error {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	var source map[string]any
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	err := decoder.Decode(&source)
	response.Body.Close()
	if err != nil {
		return err
	}
	message := map[string]any{"role": "assistant"}
	var texts, reasoning []string
	var calls []any
	for _, entry := range source["content"].([]any) {
		block := entry.(map[string]any)
		switch firstString(block, "type") {
		case "text":
			texts = append(texts, firstString(block, "text"))
		case "thinking":
			reasoning = append(reasoning, firstString(block, "thinking"))
		case "tool_use":
			args, _ := json.Marshal(block["input"])
			calls = append(calls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": string(args)}})
		}
	}
	message["content"] = strings.Join(texts, "")
	if len(reasoning) > 0 {
		message["reasoning_content"] = strings.Join(reasoning, "\n")
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := "stop"
	if source["stop_reason"] == "tool_use" {
		finish = "tool_calls"
	} else if source["stop_reason"] == "max_tokens" {
		finish = "length"
	}
	usage, _ := source["usage"].(map[string]any)
	prompt := numberAsInt64(usage["input_tokens"]) + numberAsInt64(usage["cache_read_input_tokens"])
	output := numberAsInt64(usage["output_tokens"])
	chatUsage := map[string]any{"prompt_tokens": prompt, "completion_tokens": output, "total_tokens": prompt + output, "prompt_tokens_details": map[string]any{"cached_tokens": usage["cache_read_input_tokens"]}}
	created := time.Now().Unix()
	chat := map[string]any{"id": source["id"], "object": "chat.completion", "created": created, "model": source["model"], "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": chatUsage, "transithub": source["transithub"]}
	var encoded []byte
	if stream {
		var buf bytes.Buffer
		emit := func(delta map[string]any, stop any, withUsage bool) {
			chunk := map[string]any{"id": source["id"], "object": "chat.completion.chunk", "created": created, "model": source["model"], "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": stop}}, "transithub": source["transithub"]}
			if withUsage {
				chunk["usage"] = chatUsage
			}
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(&buf, "data: %s\n\n", raw)
		}
		emit(map[string]any{"role": "assistant"}, nil, false)
		delta := map[string]any{"content": message["content"]}
		if value, ok := message["reasoning_content"]; ok {
			delta["reasoning_content"] = value
		}
		if len(calls) > 0 {
			for index, call := range calls {
				call.(map[string]any)["index"] = index
			}
			delta["tool_calls"] = calls
		}
		emit(delta, nil, false)
		emit(map[string]any{}, finish, true)
		buf.WriteString("data: [DONE]\n\n")
		encoded = buf.Bytes()
		response.Header.Set("Content-Type", "text/event-stream")
	} else {
		encoded, err = json.Marshal(chat)
		if err != nil {
			return err
		}
		response.Header.Set("Content-Type", "application/json")
	}
	response.Header.Del("Content-Length")
	response.Header.Del("Content-Encoding")
	response.Header.Set("X-TransitHub-Response-Adapter", "chat-responses")
	response.ContentLength = int64(len(encoded))
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	return nil
}
