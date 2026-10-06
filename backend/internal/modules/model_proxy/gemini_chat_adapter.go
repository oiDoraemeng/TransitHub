package model_proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxProtocolResponseBytes = 64 << 20

type geminiGenerateRequest struct {
	SystemInstruction *geminiContent          `json:"systemInstruction"`
	Contents          []geminiContent         `json:"contents"`
	Tools             []geminiTool            `json:"tools"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MIMEType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiGenerationConfig struct {
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"topP,omitempty"`
	MaxOutputTokens  *int           `json:"maxOutputTokens,omitempty"`
	CandidateCount   *int           `json:"candidateCount,omitempty"`
	StopSequences    []string       `json:"stopSequences,omitempty"`
	ResponseMIMEType string         `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any `json:"responseSchema,omitempty"`
}

type upstreamProtocolRequest struct {
	requestURI      string
	body            io.ReadCloser
	responseAdapter func(*http.Response) error
}

func adaptUpstreamProtocol(incoming *http.Request, body io.ReadCloser) (upstreamProtocolRequest, error) {
	model, ok := geminiGenerateContentModel(incoming.URL.Path)
	if !ok {
		return upstreamProtocolRequest{requestURI: incoming.URL.RequestURI(), body: body}, nil
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	if closeErr != nil {
		return upstreamProtocolRequest{}, closeErr
	}
	converted, err := geminiToChatCompletions(model, raw)
	if err != nil {
		return upstreamProtocolRequest{}, err
	}
	return upstreamProtocolRequest{
		requestURI: "/v1/chat/completions",
		body:       io.NopCloser(bytes.NewReader(converted)),
		responseAdapter: func(response *http.Response) error {
			return chatCompletionsToGemini(response, model)
		},
	}, nil
}

func geminiGenerateContentModel(path string) (string, bool) {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ":generateContent") {
		return "", false
	}
	model := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ":generateContent")
	if model == "" || strings.Contains(model, "/") || strings.Contains(model, ":") {
		return "", false
	}
	return model, true
}

func geminiToChatCompletions(model string, raw []byte) ([]byte, error) {
	var source geminiGenerateRequest
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("decode Gemini request: %w", err)
	}
	messages := make([]map[string]any, 0, len(source.Contents)+1)
	if source.SystemInstruction != nil {
		if content := geminiPartsToOpenAIContent(source.SystemInstruction.Parts); content != nil {
			messages = append(messages, map[string]any{"role": "system", "content": content})
		}
	}
	toolCallIDs := make(map[string]string)
	toolSequence := 0
	for _, content := range source.Contents {
		role := "user"
		if strings.EqualFold(content.Role, "model") {
			role = "assistant"
		}
		message := map[string]any{"role": role}
		if value := geminiPartsToOpenAIContent(content.Parts); value != nil {
			message["content"] = value
		}
		toolCalls := make([]map[string]any, 0)
		toolResponses := make([]map[string]any, 0)
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				toolSequence++
				id := "call_" + strconv.Itoa(toolSequence)
				toolCallIDs[part.FunctionCall.Name] = id
				arguments, err := json.Marshal(part.FunctionCall.Args)
				if err != nil {
					return nil, fmt.Errorf("encode Gemini function call: %w", err)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": part.FunctionCall.Name, "arguments": string(arguments)},
				})
			}
			if part.FunctionResponse != nil {
				response, err := json.Marshal(part.FunctionResponse.Response)
				if err != nil {
					return nil, fmt.Errorf("encode Gemini function response: %w", err)
				}
				id := toolCallIDs[part.FunctionResponse.Name]
				if id == "" {
					toolSequence++
					id = "call_" + strconv.Itoa(toolSequence)
				}
				toolResponses = append(toolResponses, map[string]any{
					"role": "tool", "tool_call_id": id, "name": part.FunctionResponse.Name, "content": string(response),
				})
			}
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if _, hasContent := message["content"]; hasContent || len(toolCalls) > 0 {
			messages = append(messages, message)
		}
		messages = append(messages, toolResponses...)
	}
	request := map[string]any{"model": model, "messages": messages, "stream": false}
	if len(source.Tools) > 0 {
		tools := make([]map[string]any, 0)
		for _, tool := range source.Tools {
			for _, declaration := range tool.FunctionDeclarations {
				function := map[string]any{"name": declaration.Name}
				if declaration.Description != "" {
					function["description"] = declaration.Description
				}
				if declaration.Parameters != nil {
					function["parameters"] = declaration.Parameters
				}
				tools = append(tools, map[string]any{"type": "function", "function": function})
			}
		}
		if len(tools) > 0 {
			request["tools"] = tools
		}
	}
	applyGeminiGenerationConfig(request, source.GenerationConfig)
	return json.Marshal(request)
}

func geminiPartsToOpenAIContent(parts []geminiPart) any {
	items := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != "":
			items = append(items, map[string]any{"type": "text", "text": part.Text})
		case part.InlineData != nil && strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/"):
			items = append(items, map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:" + part.InlineData.MIMEType + ";base64," + part.InlineData.Data,
			}})
		case part.FileData != nil && strings.HasPrefix(strings.ToLower(part.FileData.MIMEType), "image/"):
			items = append(items, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.FileData.FileURI}})
		}
	}
	if len(items) == 0 {
		return nil
	}
	if len(items) == 1 && items[0]["type"] == "text" {
		return items[0]["text"]
	}
	return items
}

func applyGeminiGenerationConfig(target map[string]any, config *geminiGenerationConfig) {
	if config == nil {
		return
	}
	if config.Temperature != nil {
		target["temperature"] = *config.Temperature
	}
	if config.TopP != nil {
		target["top_p"] = *config.TopP
	}
	if config.MaxOutputTokens != nil {
		target["max_tokens"] = *config.MaxOutputTokens
	}
	if config.CandidateCount != nil {
		target["n"] = *config.CandidateCount
	}
	if len(config.StopSequences) > 0 {
		target["stop"] = config.StopSequences
	}
	if config.ResponseSchema != nil {
		target["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "gemini_response", "schema": normalizeGeminiSchema(config.ResponseSchema)},
		}
	} else if strings.EqualFold(config.ResponseMIMEType, "application/json") {
		target["response_format"] = map[string]any{"type": "json_object"}
	}
}

func normalizeGeminiSchema(value any) any {
	switch current := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(current))
		for key, item := range current {
			if key == "type" {
				if schemaType, ok := item.(string); ok {
					normalized[key] = strings.ToLower(schemaType)
					continue
				}
			}
			normalized[key] = normalizeGeminiSchema(item)
		}
		return normalized
	case []any:
		normalized := make([]any, len(current))
		for index, item := range current {
			normalized[index] = normalizeGeminiSchema(item)
		}
		return normalized
	default:
		return value
	}
}

func chatCompletionsToGemini(response *http.Response, requestedModel string) error {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return err
	}
	if len(raw) > maxProtocolResponseBytes {
		return errors.New("Chat Completions response exceeds protocol conversion limit")
	}
	var source struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return fmt.Errorf("decode Chat Completions response: %w", err)
	}
	candidates := make([]map[string]any, 0, len(source.Choices))
	for _, choice := range source.Choices {
		parts := openAIContentToGeminiParts(choice.Message.Content)
		for _, call := range choice.Message.ToolCalls {
			arguments := map[string]any{}
			if strings.TrimSpace(call.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
					return fmt.Errorf("decode Chat Completions function arguments: %w", err)
				}
			}
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": call.Function.Name, "args": arguments}})
		}
		candidates = append(candidates, map[string]any{
			"index":        choice.Index,
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": geminiFinishReason(choice.FinishReason),
		})
	}
	model := source.Model
	if model == "" {
		model = requestedModel
	}
	target := map[string]any{
		"candidates":   candidates,
		"modelVersion": model,
		"responseId":   source.ID,
		"usageMetadata": map[string]any{
			"promptTokenCount":     source.Usage.PromptTokens,
			"candidatesTokenCount": source.Usage.CompletionTokens,
			"totalTokenCount":      source.Usage.TotalTokens,
		},
	}
	converted, err := json.Marshal(target)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(converted))
	response.ContentLength = int64(len(converted))
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(converted)))
	response.Header.Del("Content-Encoding")
	return nil
}

func openAIContentToGeminiParts(content any) []map[string]any {
	parts := make([]map[string]any, 0)
	switch value := content.(type) {
	case string:
		if value != "" {
			parts = append(parts, map[string]any{"text": value})
		}
	case []any:
		for _, item := range value {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		}
	}
	return parts
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "stop", "tool_calls":
		return "STOP"
	case "":
		return "FINISH_REASON_UNSPECIFIED"
	default:
		return strings.ToUpper(reason)
	}
}
