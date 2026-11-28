package model_proxy

import "strings"

// mappedThinkingSource keeps only visible conversation state when the member
// explicitly opts into mapped thinking. Neither Claude signatures nor private
// reasoning are forwarded to a different provider. The original replay payload
// stays intact for a possible native-member retry.
func mappedThinkingSource(source map[string]any) (map[string]any, error) {
	facts := inspectRequestCapabilities(source)
	if facts.NativeState {
		return nil, mediaError("opaque Claude state requires a native member")
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		if key != "thinking" {
			result[key] = value
		}
	}
	messages, _ := source["messages"].([]any)
	visible := make([]any, 0, len(messages))
	for _, entry := range messages {
		message, ok := entry.(map[string]any)
		if !ok {
			return nil, mediaError("invalid message")
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			visible = append(visible, message)
			continue
		}
		content := make([]any, 0, len(blocks))
		removed := false
		for _, entry := range blocks {
			block, _ := entry.(map[string]any)
			if firstString(block, "type") == "thinking" {
				if firstString(message, "role") != "assistant" {
					return nil, mediaError("thinking history must belong to an assistant")
				}
				removed = true
				continue
			}
			content = append(content, entry)
		}
		if !removed {
			visible = append(visible, message)
			continue
		}
		if len(content) == 0 {
			continue
		}
		copy := make(map[string]any, len(message))
		for key, value := range message {
			copy[key] = value
		}
		copy["content"] = content
		visible = append(visible, copy)
	}
	result["messages"] = visible
	if inspectRequestCapabilities(result).NativeClaude {
		return nil, mediaError("unsupported Claude state requires a native member")
	}
	return result, nil
}

// Only the provider's public reasoning summary is eligible for display. In
// particular, encrypted_content and raw reasoning content are never used as a
// Claude thinking block or signature.
func mappedReasoningSummary(item map[string]any) string {
	parts, _ := item["summary"].([]any)
	var text strings.Builder
	for _, entry := range parts {
		part, _ := entry.(map[string]any)
		if firstString(part, "type") != "summary_text" || firstString(part, "text") == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(firstString(part, "text"))
	}
	return text.String()
}
