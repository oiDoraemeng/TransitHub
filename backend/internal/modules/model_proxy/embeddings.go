package model_proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
)

const embeddingsPath = "/v1/embeddings"

// Embeddings are a local compatibility response, not an inference or a health
// signal. Keep this ahead of routing, concurrency, mapping and upstream calls.
func (s *Service) serveLocalEmbeddings(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != embeddingsPath {
		return false
	}
	var input struct {
		Model          string          `json:"model"`
		Input          json.RawMessage `json:"input"`
		Dimensions     int             `json:"dimensions"`
		EncodingFormat string          `json:"encoding_format"`
	}
	limit := s.maxRequestBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	if err := json.NewDecoder(body).Decode(&input); err != nil {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must be JSON")
		}
		return true
	}
	dimensions := input.Dimensions
	if dimensions == 0 {
		dimensions = 1536
		if input.Model == "text-embedding-3-large" {
			dimensions = 3072
		}
	}
	count := embeddingInputCount(input.Input)
	// Bound response amplification while supporting full 2,048-input batches
	// with the standard embedding dimensions.
	if dimensions < 1 || dimensions > 65536 || count > 2048 || int64(count)*int64(dimensions) > 8<<20 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "embedding dimensions or batch size is too large")
		return true
	}
	var vector any = make([]float32, dimensions)
	if input.EncodingFormat == "base64" {
		// IEEE 754 float32 zero has four zero bytes in little-endian format.
		vector = base64.StdEncoding.EncodeToString(make([]byte, dimensions*4))
	}
	type embedding struct {
		Object    string `json:"object"`
		Embedding any    `json:"embedding"`
		Index     int    `json:"index"`
	}
	data := make([]embedding, count)
	for i := range data {
		data[i] = embedding{Object: "embedding", Embedding: vector, Index: i}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string      `json:"object"`
		Data   []embedding `json:"data"`
		Model  string      `json:"model"`
		Usage  struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}{Object: "list", Data: data, Model: input.Model})
	return true
}

func embeddingInputCount(raw json.RawMessage) int {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return 1
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return 0
	}
	first := bytes.TrimSpace(items[0])
	if len(first) > 0 && (first[0] == '"' || first[0] == '[') {
		return len(items) // Batch of texts or token arrays.
	}
	return 1 // A single array of token IDs.
}
