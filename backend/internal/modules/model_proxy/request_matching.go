package model_proxy

import "strings"

type requestCapabilities struct {
	PDF, CodeExecution, JSONSchema, WebSearch, NativeClaude bool
	Thinking, NativeState                                   bool
}

type requestCapabilitiesContextKey struct{}

func reservedToolVersion(kind, prefix string) bool {
	version := strings.TrimPrefix(kind, prefix)
	if !strings.HasPrefix(kind, prefix) || len(version) != 8 {
		return false
	}
	for _, char := range version {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func inspectRequestCapabilities(payload map[string]any) requestCapabilities {
	facts := requestCapabilities{}
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		facts.Thinking = firstString(thinking, "type") == "enabled" || firstString(thinking, "type") == "adaptive"
	}
	for _, key := range []string{"container", "context_management", "mcp_servers"} {
		facts.NativeState = facts.NativeState || payload[key] != nil
	}
	if config, ok := payload["output_config"].(map[string]any); ok {
		format, _ := config["format"].(map[string]any)
		facts.JSONSchema = firstString(format, "type") == "json_schema"
	}
	if config, ok := payload["text"].(map[string]any); ok {
		format, _ := config["format"].(map[string]any)
		facts.JSONSchema = facts.JSONSchema || firstString(format, "type") == "json_schema"
	}
	if config, ok := payload["response_format"].(map[string]any); ok {
		facts.JSONSchema = facts.JSONSchema || firstString(config, "type") == "json_schema"
	}
	// Visit message content, not arbitrary schemas, tool results, or prompt text.
	var content func(any)
	content = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				content(child)
			}
		case map[string]any:
			kind := firstString(v, "type")
			switch kind {
			case "thinking":
				facts.Thinking = true
			case "redacted_thinking":
				facts.Thinking, facts.NativeState = true, true
			case "server_tool_use", "web_search_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "mcp_tool_use", "mcp_tool_result":
				facts.NativeState = true
			}
			if kind == "tool_result" || kind == "function_call_output" {
				return
			}
			if kind == "document" {
				source, _ := v["source"].(map[string]any)
				facts.NativeState = facts.NativeState || firstString(source, "type") == "file"
				facts.PDF = facts.PDF || firstString(source, "media_type") == "application/pdf" || mediaMIME(firstString(source, "url"), "") == "application/pdf"
			}
			if kind == "input_file" || kind == "file" || kind == "input_image" || kind == "image_url" {
				file, _ := v["file"].(map[string]any)
				uri := firstString(v, "file_data", "file_url", "image_url")
				if image, ok := v["image_url"].(map[string]any); ok {
					uri = firstString(image, "url")
				}
				facts.PDF = facts.PDF || mediaMIME(uri, firstString(v, "filename")) == "application/pdf" || mediaMIME(firstString(file, "file_data", "file_url"), firstString(file, "filename")) == "application/pdf"
			}
			for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
				if data, ok := v[key].(map[string]any); ok {
					facts.PDF = facts.PDF || strings.EqualFold(firstString(data, "mimeType", "mime_type"), "application/pdf")
				}
			}
			// Never recurse into client tool output or definitions.
			for _, key := range []string{"messages", "input", "contents", "content", "parts"} {
				content(v[key])
			}
		}
	}
	for _, key := range []string{"messages", "input", "contents"} {
		content(payload[key])
	}
	tools, _ := payload["tools"].([]any)
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		kind := firstString(tool, "type")
		versioned := reservedToolVersion(kind, "code_execution_")
		facts.CodeExecution = facts.CodeExecution || kind == "code_interpreter" || versioned || kind == "code_execution"
		facts.WebSearch = facts.WebSearch || reservedToolVersion(kind, "web_search_") || kind == "web_search" || kind == "web_search_preview"
		// Dynamic filtering and opaque provider state cannot move to Responses.
		facts.NativeState = facts.NativeState || reservedToolVersion(kind, "web_search_") && kind != "web_search_20250305" || reservedToolVersion(kind, "web_fetch_") || reservedToolVersion(kind, "computer_") || reservedToolVersion(kind, "tool_search_tool_regex_") || reservedToolVersion(kind, "tool_search_tool_bm25_")
		_, nativeGemini := tool["codeExecution"]
		_, nativeGeminiSnake := tool["code_execution"]
		facts.CodeExecution = facts.CodeExecution || nativeGemini || nativeGeminiSnake
	}
	facts.NativeClaude = facts.Thinking || facts.NativeState
	return facts
}

func (f requestCapabilities) matches(capabilities []string) bool {
	for _, capability := range capabilities {
		if f.has(capability) {
			return true
		}
	}
	return false
}

func (f requestCapabilities) has(capability string) bool {
	switch capability {
	case "pdf":
		return f.PDF
	case "code_execution":
		return f.CodeExecution
	case "json_schema":
		return f.JSONSchema
	case "web_search":
		return f.WebSearch
	case "native_claude":
		return f.NativeClaude
	}
	return false
}

func (f requestCapabilities) present() bool {
	return f.PDF || f.CodeExecution || f.JSONSchema || f.WebSearch || f.NativeClaude
}

// A dedicated member must support the whole request, including mixed tools.
// Skip policies still exclude a member on any selected capability.
func (f requestCapabilities) coveredBy(capabilities []string) bool {
	selected := map[string]bool{}
	for _, capability := range capabilities {
		selected[capability] = true
	}
	for _, capability := range []string{"pdf", "code_execution", "json_schema", "web_search", "native_claude"} {
		if f.has(capability) && !selected[capability] {
			return false
		}
	}
	return f.present()
}

func (route Route) thinkingAllowed() bool {
	return route.ThinkingEnabled == nil || *route.ThinkingEnabled
}

func canSimulateMappedThinking(route Route, facts requestCapabilities, model ...string) bool {
	return route.thinkingAllowed() && route.SimulateThinking && route.ModelMappingEnabled &&
		facts.Thinking && !facts.NativeState && len(model) > 0 && strings.HasPrefix(routeTargetModel(route, model[0]), "gpt-")
}

func effectiveRequestCapabilities(route Route, facts requestCapabilities, model ...string) (requestCapabilities, bool) {
	simulated := canSimulateMappedThinking(route, facts, model...)
	if simulated {
		// Only the explicitly supported thinking format is transferable. Opaque
		// provider state must never be cleared along with the thinking flag.
		facts.NativeClaude = false
	}
	return facts, simulated
}

func requestPolicyAllows(route Route, facts requestCapabilities, model ...string) bool {
	if facts.Thinking && !route.thinkingAllowed() {
		return false
	}
	facts, simulated := effectiveRequestCapabilities(route, facts, model...)
	matched := facts.matches(route.RequestMatchCapabilities)
	switch route.RequestMatchMode {
	case RequestMatchModeMatchOrFallback:
		// Keywords and the fallback stage are checked separately. Even a backup
		// must cover every requested capability and target a mapped GPT model.
		return route.ModelMappingEnabled && len(model) > 0 && strings.HasPrefix(routeTargetModel(route, model[0]), "gpt-") &&
			(!facts.present() || facts.coveredBy(route.RequestMatchCapabilities))
	case KeywordMatchModeRequireMatch:
		return (matched || simulated) && (facts.coveredBy(route.RequestMatchCapabilities) || simulated && !facts.present())
	case KeywordMatchModeSkipOnMatch:
		return !matched
	default:
		return true
	}
}

func filterRoutesByRequest(facts requestCapabilities, routes []Route, model ...string) []Route {
	filtered := make([]Route, 0, len(routes))
	for _, route := range routes {
		effective, _ := effectiveRequestCapabilities(route, facts, model...)
		if effective.NativeClaude && len(model) > 0 && strings.HasPrefix(routeTargetModel(route, model[0]), "gpt-") {
			continue
		}
		if requestPolicyAllows(route, facts, model...) {
			filtered = append(filtered, route)
		}
	}
	return filtered
}

func normalizeRequestCapabilities(values []string) ([]string, error) {
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		if value != "pdf" && value != "code_execution" && value != "json_schema" && value != "web_search" && value != "native_claude" {
			return nil, &requestError{Status: 400, Message: "invalid requestMatchCapabilities"}
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, nil
}
