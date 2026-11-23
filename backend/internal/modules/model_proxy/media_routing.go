package model_proxy

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type requestMedia struct {
	Audio, Video, PDF, FileReference bool
	Model                            string
}

func (m requestMedia) present() bool { return m.Audio || m.Video || m.PDF }

type requestMediaContextKey struct{}

// Inspect content, including conversation history, rather than arbitrary tool
// schemas or text which might merely describe an attachment.
func inspectRequestMedia(payload map[string]any) requestMedia {
	var result requestMedia
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				visit(item)
			}
		case map[string]any:
			kind, _ := v["type"].(string)
			switch kind {
			case "input_audio", "audio", "audio_url":
				result.Audio = true
			case "input_video", "video", "video_url":
				result.Video = true
			}
			for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
				if item, ok := v[key].(map[string]any); ok {
					mime := firstString(item, "mimeType", "mime_type", "media_type")
					result.addMIME(mime)
					result.addURI(firstString(item, "fileUri", "file_uri"), "")
					if key == "fileData" || key == "file_data" {
						if strings.EqualFold(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]), "application/pdf") {
							result.FileReference = true
						}
					}
				}
			}
			for _, key := range []string{"image_url", "video_url", "audio_url", "file_data", "file_url"} {
				value := v[key]
				if item, ok := value.(map[string]any); ok {
					value = item["url"]
				}
				if uri, ok := value.(string); ok {
					result.addURI(uri, "")
				}
			}
			if item, ok := v["file"].(map[string]any); ok {
				result.addURI(firstString(item, "file_data", "file_url"), firstString(item, "filename"))
			}
			if kind == "input_file" {
				result.addURI(firstString(v, "file_data", "file_url"), firstString(v, "filename"))
			}
			for _, key := range []string{"messages", "input", "contents", "content", "parts", "systemInstruction", "system_instruction"} {
				if child, ok := v[key]; ok {
					visit(child)
				}
			}
		}
	}
	visit(payload)
	return result
}

func (m *requestMedia) addURI(uri, filename string) {
	mime := mediaMIME(uri, filename)
	m.addMIME(mime)
	if mime == "application/pdf" && strings.Contains(uri, "://") {
		m.FileReference = true
	}
}

func (m *requestMedia) addMIME(mime string) {
	mime = strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	m.Audio = m.Audio || strings.HasPrefix(mime, "audio/")
	m.Video = m.Video || strings.HasPrefix(mime, "video/")
	m.PDF = m.PDF || mime == "application/pdf"
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := value[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func mediaMIME(uri, filename string) string {
	if strings.HasPrefix(strings.ToLower(uri), "data:") {
		if index := strings.IndexAny(uri[5:], ";,"); index >= 0 {
			return strings.ToLower(uri[5 : 5+index])
		}
	}
	if filename == "" {
		if parsed, err := url.Parse(uri); err == nil {
			filename = parsed.Path
		}
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	}
	return ""
}

func withRequestMedia(incoming *http.Request, payload map[string]any, model string) (*http.Request, requestMedia) {
	media := inspectRequestMedia(payload)
	media.Model = model
	return incoming.WithContext(context.WithValue(incoming.Context(), requestMediaContextKey{}, media)), media
}

func nativeGeminiModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini-") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini‑")
}

func routeTargetModel(route Route, model string) string {
	if route.ModelMappingEnabled {
		if target := strings.TrimSpace(route.ModelMapping[model]); target != "" {
			return target
		}
	}
	return model
}

func filterMediaRoutes(routes []Route, media requestMedia, incoming *http.Request) []Route {
	_, _, geminiRequest := nativeGeminiModelAction(incoming.URL.Path)
	nativePDF := media.PDF && (media.FileReference || geminiRequest && isNativeGeminiStreamingRequest(incoming))
	if !media.Audio && !media.Video && !nativePDF {
		return routes
	}
	result := make([]Route, 0, len(routes))
	for _, route := range routes {
		if !nativeGeminiModel(routeTargetModel(route, media.Model)) {
			continue
		}
		// Stay within the caller's smart group and model allowlist. A failed or
		// full Ultra member must never fall through to a text-only GPT alias.
		if (media.Audio || media.Video) && !strings.EqualFold(strings.Join(strings.Fields(route.Name), " "), "gemini ultra") {
			continue
		}
		result = append(result, route)
	}
	return result
}
