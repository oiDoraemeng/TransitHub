package model_proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxMappedResponseBytes = 8 << 20
const maxMappedRequestBytes = 16 << 20

// Only the buffered compatibility path is bounded here; ordinary streaming
// requests retain their existing concurrency policy.
var mappedResponseSlots = make(chan struct{}, 8)

// Protocol conversion follows the effective target model. Capability policies
// select members before this point; they must not disable conversion after a
// Claude alias has already been mapped to GPT. Direct GPT requests in Messages
// format use the same adapter without inheriting any smart-group policy.
func adaptMemberUpstreamProtocol(incoming *http.Request, body io.ReadCloser, route Route) (upstreamProtocolRequest, error) {
	media, _ := incoming.Context().Value(requestMediaContextKey{}).(requestMedia)
	chat := incoming.URL.Path == "/v1/chat/completions"
	if incoming.URL.Path != "/v1/messages" && !chat || !route.ModelMappingEnabled && !strings.HasPrefix(media.Model, "gpt-") {
		return adaptMediaUpstreamProtocol(incoming, body)
	}
	if media.Model != "" && !strings.HasPrefix(routeTargetModel(route, media.Model), "gpt-") {
		return adaptMediaUpstreamProtocol(incoming, body)
	}
	if chat {
		if facts, ok := incoming.Context().Value(requestCapabilitiesContextKey{}).(requestCapabilities); ok && !facts.present() {
			return adaptMediaUpstreamProtocol(incoming, body)
		}
	}
	select {
	case mappedResponseSlots <- struct{}{}:
	default:
		body.Close()
		return upstreamProtocolRequest{}, fmt.Errorf("mapped response adapter is busy")
	}
	var once sync.Once
	release := func() { once.Do(func() { <-mappedResponseSlots }) }
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(body, maxMappedRequestBytes+1))
	body.Close()
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	if len(raw) > maxMappedRequestBytes {
		return upstreamProtocolRequest{}, mediaError("mapped request exceeds 16 MiB; use a native member")
	}
	var source map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&source); err != nil {
		return upstreamProtocolRequest{}, err
	}
	if !strings.HasPrefix(firstString(source, "model"), "gpt-") {
		return adaptMediaUpstreamProtocol(incoming, io.NopCloser(bytes.NewReader(raw)))
	}
	if chat {
		facts := inspectRequestCapabilities(source)
		if !facts.present() {
			return adaptMediaUpstreamProtocol(incoming, io.NopCloser(bytes.NewReader(raw)))
		}
		source, err = chatToMappedMessages(source)
		if err != nil {
			return upstreamProtocolRequest{}, err
		}
	}
	facts := inspectRequestCapabilities(source)
	if facts.Thinking && !route.thinkingAllowed() {
		return upstreamProtocolRequest{}, mediaError("thinking is disabled for this member")
	}
	mappedThinking := canSimulateMappedThinking(route, facts, firstString(source, "model"))
	simulateThinking := mappedThinking
	if config, ok := source["thinking"].(map[string]any); ok && firstString(config, "type") == "disabled" {
		simulateThinking = false
	}
	if mappedThinking {
		source, err = mappedThinkingSource(source)
		if err != nil {
			return upstreamProtocolRequest{}, err
		}
	} else if facts.NativeClaude {
		return upstreamProtocolRequest{}, mediaError("native Claude thinking, signatures and provider state require a native member")
	}
	schema, err := compileMappedOutputSchema(source)
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	converted, execution, err := messagesToResponses(source)
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	if simulateThinking {
		converted["reasoning"] = map[string]any{"effort": "high", "summary": "auto"}
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	stream, _ := source["stream"].(bool)
	handedOff = true
	return upstreamProtocolRequest{requestURI: "/v1/responses", body: io.NopCloser(bytes.NewReader(encoded)), bufferResponse: true, maxResponseBytes: maxMappedResponseBytes, release: release,
		responseAdapter: func(response *http.Response) error {
			if err := offloadResponseToMessagesChecked(response, firstString(source, "model"), stream && !chat, execution, schema, simulateThinking); err != nil {
				return err
			}
			if chat {
				return mappedMessagesToChat(response, stream)
			}
			return nil
		}}, nil
}

func messagesToResponses(source map[string]any) (map[string]any, bool, error) {
	if inspectRequestCapabilities(source).NativeClaude {
		return nil, false, mediaError("native Claude thinking, signatures and provider state cannot be mapped")
	}
	result := map[string]any{"model": source["model"], "stream": true, "store": false}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	if value, ok := source["max_tokens"]; ok {
		result["max_output_tokens"] = value
	}
	if stops, ok := source["stop_sequences"].([]any); ok && len(stops) > 0 {
		return nil, false, mediaError("mapped Responses requests do not support stop_sequences")
	}
	if source["context_management"] != nil || source["mcp_servers"] != nil {
		return nil, false, mediaError("mapped Responses requests do not support context_management or mcp_servers")
	}
	var instructions []string
	switch system := source["system"].(type) {
	case string:
		instructions = append(instructions, system)
	case []any:
		for _, entry := range system {
			block, ok := entry.(map[string]any)
			if !ok || firstString(block, "type") != "text" {
				return nil, false, mediaError("unsupported system content")
			}
			instructions = append(instructions, firstString(block, "text"))
		}
	}
	if len(instructions) > 0 {
		result["instructions"] = strings.Join(instructions, "\n")
	}
	var input []any
	messages, _ := source["messages"].([]any)
	for _, entry := range messages {
		message, ok := entry.(map[string]any)
		if !ok {
			return nil, false, mediaError("invalid message")
		}
		role := firstString(message, "role")
		if role != "user" && role != "assistant" {
			return nil, false, mediaError("unsupported message role")
		}
		var blocks []any
		if text, ok := message["content"].(string); ok {
			blocks = []any{map[string]any{"type": "text", "text": text}}
		} else {
			var ok bool
			blocks, ok = message["content"].([]any)
			if !ok {
				return nil, false, mediaError("message content must be text or content blocks")
			}
		}
		var parts []any
		flush := func() {
			if len(parts) > 0 {
				input = append(input, map[string]any{"role": role, "content": parts})
				parts = nil
			}
		}
		for _, entry := range blocks {
			block, ok := entry.(map[string]any)
			if !ok {
				return nil, false, mediaError("invalid content block")
			}
			switch firstString(block, "type") {
			case "text":
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				parts = append(parts, map[string]any{"type": kind, "text": block["text"]})
			case "document", "image":
				if role != "user" {
					return nil, false, mediaError("attachments must be in user messages")
				}
				src, _ := block["source"].(map[string]any)
				kind := firstString(block, "type")
				part := map[string]any{}
				if kind == "image" {
					part["type"] = "input_image"
					if firstString(src, "type") == "base64" {
						part["image_url"] = "data:" + firstString(src, "media_type") + ";base64," + firstString(src, "data")
					} else if firstString(src, "type") == "url" {
						part["image_url"] = src["url"]
					} else {
						return nil, false, mediaError("unsupported image source")
					}
				} else {
					part["type"] = "input_file"
					switch firstString(src, "type") {
					case "base64":
						if firstString(src, "media_type") != "application/pdf" {
							return nil, false, mediaError("mapped documents must be PDFs")
						}
						part["filename"], part["file_data"] = "document.pdf", "data:application/pdf;base64,"+firstString(src, "data")
					case "url":
						part["file_url"] = src["url"]
					default:
						return nil, false, mediaError("file IDs cannot be transferred between providers")
					}
				}
				parts = append(parts, part)
			case "thinking", "redacted_thinking": // Native reasoning tokens cannot be moved across providers.
			case "tool_use":
				flush()
				args, err := json.Marshal(block["input"])
				if err != nil {
					return nil, false, err
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": block["id"], "name": block["name"], "arguments": string(args)})
			case "tool_result":
				flush()
				text, ok := block["content"].(string)
				if !ok {
					raw, err := json.Marshal(block["content"])
					if err != nil {
						return nil, false, err
					}
					text = string(raw)
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": block["tool_use_id"], "output": text})
			default:
				return nil, false, mediaError("content cannot be transferred to the mapped Responses provider")
			}
		}
		flush()
	}
	result["input"] = input
	execution := false
	search := false
	var include []string
	var tools []any
	definitions, _ := source["tools"].([]any)
	for _, entry := range definitions {
		tool, ok := entry.(map[string]any)
		if !ok {
			return nil, false, mediaError("invalid tool definition")
		}
		kind := firstString(tool, "type")
		if inspectRequestCapabilities(map[string]any{"tools": []any{tool}}).CodeExecution {
			if execution {
				return nil, false, mediaError("only one server code execution tool may be mapped")
			}
			execution = true
			container := map[string]any{"type": "auto"}
			if kind == "code_interpreter" && tool["container"] != nil {
				provided, ok := tool["container"].(map[string]any)
				if !ok || firstString(provided, "type") != "auto" {
					return nil, false, mediaError("code interpreter container IDs cannot transfer between providers")
				}
				for key, value := range provided {
					switch key {
					case "type", "memory_limit":
						container[key] = value
					case "file_ids":
						ids, ok := value.([]any)
						if !ok || len(ids) != 0 {
							return nil, false, mediaError("code interpreter file IDs cannot transfer between providers")
						}
					default:
						return nil, false, mediaError("unsupported code interpreter container option: " + key)
					}
				}
			}
			tools = append(tools, map[string]any{"type": "code_interpreter", "container": container})
			include = append(include, "code_interpreter_call.outputs")
		} else if kind == "web_search" || kind == "web_search_preview" {
			if search {
				return nil, false, mediaError("only one server search tool may be mapped")
			}
			search = true
			tools = append(tools, tool)
			include = append(include, "web_search_call.action.sources")
		} else if kind == "web_search_20250305" {
			if search {
				return nil, false, mediaError("only one server search tool may be mapped")
			}
			search = true
			mapped := map[string]any{"type": "web_search"}
			filters := map[string]any{}
			if domains, ok := tool["allowed_domains"]; ok {
				filters["allowed_domains"] = domains
			}
			if domains, exists := tool["blocked_domains"]; exists {
				blocked, valid := domains.([]any)
				if !valid || len(blocked) > 0 {
					return nil, false, mediaError("mapped web search does not support blocked_domains; use a native member")
				}
			}
			if len(filters) > 0 {
				mapped["filters"] = filters
			}
			if location, ok := tool["user_location"].(map[string]any); ok {
				converted := map[string]any{"type": "approximate"}
				for key, value := range location {
					switch key {
					case "type":
						if value != "approximate" {
							return nil, false, mediaError("unsupported mapped web search location type")
						}
					case "city", "region", "country", "timezone":
						converted[key] = value
					default:
						return nil, false, mediaError("unsupported mapped web search location option: " + key)
					}
				}
				mapped["user_location"] = converted
			}
			if maxUses, ok := tool["max_uses"]; ok {
				result["max_tool_calls"] = maxUses
			}
			for key := range tool {
				switch key {
				case "type", "name", "max_uses", "allowed_domains", "blocked_domains", "user_location", "cache_control":
				default:
					return nil, false, mediaError("unsupported mapped web search option: " + key)
				}
			}
			tools = append(tools, mapped)
			include = append(include, "web_search_call.action.sources")
		} else if kind == "" || kind == "custom" {
			tools = append(tools, map[string]any{"type": "function", "name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]})
		} else {
			return nil, false, mediaError("unsupported server tool in mapped Responses request")
		}
	}
	if len(tools) > 0 {
		result["tools"] = tools
	}
	if len(include) > 0 {
		result["include"] = include
	}
	if choice, ok := source["tool_choice"].(map[string]any); ok {
		switch firstString(choice, "type") {
		case "auto", "none":
			result["tool_choice"] = choice["type"]
		case "any":
			result["tool_choice"] = "required"
		case "tool":
			if firstString(choice, "name") == "code_execution" && execution {
				result["tool_choice"] = map[string]any{"type": "code_interpreter"}
			} else if name := firstString(choice, "name"); (name == "web_search" || name == "web_search_preview") && search {
				result["tool_choice"] = map[string]any{"type": name}
			} else {
				result["tool_choice"] = map[string]any{"type": "function", "name": choice["name"]}
			}
		default:
			return nil, false, mediaError("unsupported tool_choice")
		}
	}
	if config, ok := source["output_config"].(map[string]any); ok {
		if format, ok := config["format"].(map[string]any); ok {
			if firstString(format, "type") != "json_schema" {
				return nil, false, mediaError("unsupported output format")
			}
			result["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "mapped_output", "strict": true, "schema": format["schema"]}}
			// Some compatible gateways accept text.format but do not forward it.
			// Keep the actual format requirement visible to the model as well;
			// the final response still has to pass the compiled schema validator.
			schemaJSON, err := json.Marshal(format["schema"])
			if err != nil {
				return nil, false, err
			}
			instructions = append(instructions, "Final response format: return a single JSON value that satisfies the JSON Schema below. Do not wrap the final answer in Markdown or add text outside the JSON. Tool calls may precede the final answer.\nJSON Schema:\n"+string(schemaJSON))
			result["instructions"] = strings.Join(instructions, "\n\n")
		}
	}
	return result, execution, nil
}

func offloadResponseToMessages(response *http.Response, model string, stream, execution bool) error {
	return offloadResponseToMessagesChecked(response, model, stream, execution, nil)
}

func offloadResponseToMessagesChecked(response *http.Response, model string, stream, execution bool, schema *jsonschema.Schema, thinking ...bool) error {
	simulateThinking := len(thinking) > 0 && thinking[0]
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	if err := bufferDoraStream(response, true); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxMappedResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return err
	}
	if len(raw) > maxMappedResponseBytes {
		return fmt.Errorf("mapped response too large")
	}
	var source map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&source); err != nil {
		return err
	}
	if source["error"] != nil || (source["status"] != "completed" && source["status"] != "incomplete") {
		return fmt.Errorf("mapped response did not complete")
	}
	blocks := []any{}
	summaryAvailable := false
	thinkingBlocks := 0
	stop := "end_turn"
	if source["status"] == "incomplete" {
		stop = "max_tokens"
	}
	executed := false
	var finalText strings.Builder
	records := []any{}
	citations := []any{}
	output, _ := source["output"].([]any)
	for _, entry := range output {
		item, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid mapped output")
		}
		switch firstString(item, "type") {
		case "message":
			content, _ := item["content"].([]any)
			for _, entry := range content {
				part, ok := entry.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid mapped message")
				}
				if firstString(part, "type") == "output_text" || firstString(part, "type") == "refusal" {
					annotations, _ := part["annotations"].([]any)
					for _, annotation := range annotations {
						citation, ok := annotation.(map[string]any)
						if ok && firstString(citation, "type") == "url_citation" {
							citations = append(citations, citation)
						}
					}
					text := firstString(part, "text", "refusal")
					if text != "" {
						// Commentary is valid prose but cannot precede a JSON contract.
						if schema != nil && firstString(item, "phase") == "commentary" {
							continue
						}
						finalText.WriteString(text)
						blocks = append(blocks, map[string]any{"type": "text", "text": text})
					}
				}
			}
		case "function_call":
			var args any
			decoder := json.NewDecoder(strings.NewReader(firstString(item, "arguments")))
			decoder.UseNumber()
			if err := decoder.Decode(&args); err != nil {
				return err
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return fmt.Errorf("mapped function arguments must contain one JSON value")
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": item["call_id"], "name": item["name"], "input": args})
			stop = "tool_use"
		case "web_search_call":
			if firstString(item, "status") != "completed" {
				return fmt.Errorf("mapped search did not complete")
			}
			records = append(records, map[string]any{"provider": "responses", "type": "web_search_call", "id": item["id"], "status": item["status"], "action": item["action"]})
		case "code_interpreter_call":
			results, _ := item["outputs"].([]any)
			var logs []string
			for _, entry := range results {
				result, ok := entry.(map[string]any)
				if ok && firstString(result, "type") == "logs" {
					logs = append(logs, firstString(result, "logs"))
				} else {
					return fmt.Errorf("mapped code output requires an unsupported file/image transfer")
				}
			}
			if item["status"] != "completed" || len(results) == 0 {
				return fmt.Errorf("mapped provider supplied no completed execution logs")
			}
			executed = true
			// Responses supplies logs, but no process exit code or separate stderr.
			// Preserve the real record as text instead of inventing the fields
			// required by Claude's native code_execution_result block.
			record, err := json.Marshal(map[string]any{"provider": "responses", "type": "code_interpreter_call", "id": item["id"], "status": item["status"], "code": item["code"], "logs": strings.Join(logs, "\n")})
			if err != nil {
				return err
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": "Code execution record:\n" + string(record)})
		case "reasoning":
			if simulateThinking {
				summary := mappedReasoningSummary(item)
				summaryAvailable = summaryAvailable || summary != ""
				signature := pickMappedThinkingSignature()
				thinkingBlocks++
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": summary, "signature": signature})
			}
		default:
			return fmt.Errorf("unsupported mapped response output: %s", firstString(item, "type"))
		}
	}
	if simulateThinking && thinkingBlocks == 0 {
		// An upstream may omit its summary. Preserve the requested shape while
		// leaving its unavailable content empty rather than manufacturing it.
		signature := pickMappedThinkingSignature()
		blocks = append([]any{map[string]any{"type": "thinking", "thinking": "", "signature": signature}}, blocks...)
	}
	if execution && !executed {
		return fmt.Errorf("mapped provider did not execute the requested server tool")
	}
	// A function call is an intermediate tool turn, not the final structured
	// answer. Preserve it so the client can execute the tool and continue.
	if schema != nil {
		if source["status"] != "completed" || executed {
			return fmt.Errorf("mapped structured output did not complete as a JSON response")
		}
		if stop == "end_turn" {
			value, err := jsonschema.UnmarshalJSON(strings.NewReader(finalText.String()))
			if err != nil {
				return fmt.Errorf("mapped structured output is not JSON: %w", err)
			}
			if err = schema.Validate(value); err != nil {
				return fmt.Errorf("mapped structured output violates the requested schema: %w", err)
			}
		}
	}
	if schema == nil {
		seen := map[string]bool{}
		var sources []string
		for _, entry := range citations {
			citation := entry.(map[string]any)
			url := firstString(citation, "url")
			if (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) && !seen[url] && !strings.Contains(finalText.String(), url) {
				sources = append(sources, url)
				seen[url] = true
			}
		}
		if len(sources) > 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": "Sources:\n" + strings.Join(sources, "\n")})
		}
	}
	usage, _ := source["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	cached := numberAsInt64(details["cached_tokens"])
	messageUsage := map[string]any{"input_tokens": max(int64(0), numberAsInt64(usage["input_tokens"])-cached), "output_tokens": numberAsInt64(usage["output_tokens"]), "cache_read_input_tokens": cached, "cache_creation_input_tokens": 0}
	message := map[string]any{"id": source["id"], "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": messageUsage}
	metadata := map[string]any{"upstream_protocol": "responses", "upstream_model": model, "tool_records": records, "citations": citations}
	if simulateThinking {
		metadata["thinking"] = map[string]any{"mode": "mapped_summary", "signature_status": "reserved", "summary_available": summaryAvailable, "history_policy": "visible-content-only"}
		response.Header.Set("X-TransitHub-Thinking-Mode", "mapped-summary")
	}
	message["transithub"] = metadata
	var encoded []byte
	if stream {
		var buf bytes.Buffer
		emit := func(kind string, event map[string]any) {
			event["type"] = kind
			raw, _ := json.Marshal(event)
			fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", kind, raw)
		}
		start := map[string]any{}
		for k, v := range message {
			start[k] = v
		}
		start["content"] = []any{}
		start["stop_reason"] = nil
		emit("message_start", map[string]any{"message": start})
		for index, block := range blocks {
			content := block.(map[string]any)
			start := map[string]any{}
			for key, value := range content {
				start[key] = value
			}
			kind := firstString(content, "type")
			if kind == "text" {
				start["text"] = ""
			}
			if kind == "tool_use" {
				start["input"] = map[string]any{}
			}
			if kind == "thinking" {
				start["thinking"] = ""
				// Anthropic sends the signature in a signature_delta, not in
				// content_block_start.
				start["signature"] = ""
			}
			emit("content_block_start", map[string]any{"index": index, "content_block": start})
			if kind == "text" {
				emit("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "text_delta", "text": content["text"]}})
			}
			if kind == "tool_use" {
				args, _ := json.Marshal(content["input"])
				emit("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}})
			}
			if kind == "thinking" {
				if content["thinking"] != "" {
					emit("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": content["thinking"]}})
				}
				emit("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": content["signature"]}})
			}
			emit("content_block_stop", map[string]any{"index": index})
		}
		emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": messageUsage})
		emit("message_stop", map[string]any{})
		encoded = buf.Bytes()
		response.Header.Set("Content-Type", "text/event-stream")
	} else {
		encoded, err = json.Marshal(message)
		if err != nil {
			return err
		}
		response.Header.Set("Content-Type", "application/json")
	}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = int64(len(encoded))
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.Header.Set("X-TransitHub-Upstream-Protocol", "responses")
	response.Header.Set("X-TransitHub-Upstream-Model", model)
	response.Header.Set("X-TransitHub-Response-Adapter", "messages-responses")
	return nil
}

func compileMappedOutputSchema(source map[string]any) (*jsonschema.Schema, error) {
	config, _ := source["output_config"].(map[string]any)
	format, _ := config["format"].(map[string]any)
	if format == nil {
		return nil, nil
	}
	if firstString(format, "type") != "json_schema" {
		return nil, mediaError("unsupported output format")
	}
	raw, err := json.Marshal(format["schema"])
	if err != nil || len(raw) > 64<<10 {
		return nil, mediaError("output schema exceeds 64 KiB")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, mediaError("invalid output schema JSON")
	}
	compiler := jsonschema.NewCompiler()
	// Only local resources and the library's embedded metaschemas are allowed.
	// Schema compilation must never fetch a client-supplied URL or local file.
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err = compiler.AddResource("https://transithub.invalid/output", value); err != nil {
		return nil, mediaError("invalid output schema")
	}
	schema, err := compiler.Compile("https://transithub.invalid/output")
	if err != nil {
		return nil, mediaError("unsupported or invalid output schema: " + err.Error())
	}
	return schema, nil
}

func numberAsInt64(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		value, _ := n.Int64()
		return value
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
