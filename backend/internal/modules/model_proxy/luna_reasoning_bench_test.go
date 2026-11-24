package model_proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func BenchmarkDoraLunaLargeRequest(b *testing.B) {
	raw := []byte(`{"model":"gpt-6-luna","stream":false,"reasoning":{"effort":"low"},"messages":[{"role":"user","content":"` + strings.Repeat("long context ", 170000) + `"}],"metadata":{"id":9007199254740993}}`)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, _, err := applyUpstreamReasoning("https://www.dora2api.xyz", "/v1/chat/completions", io.NopCloser(bytes.NewReader(raw)), mappedResponseContext{}, "")
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.Copy(io.Discard, body)
		body.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestDoraLunaLargeRequestPreservesOpaquePayload(t *testing.T) {
	opaque := `{"arguments": { "id":9007199254740993, "escaped":"a\\b\"c" },"history":"` + strings.Repeat("large ", 200000) + `"}`
	raw := `{"model":"gpt-6-luna","stream":true,"input":` + opaque + `,"reasoning":{"effort":"low"}}`
	body, _, err := applyUpstreamReasoning("https://dora2api.xyz", "/v1/responses", io.NopCloser(strings.NewReader(raw)), mappedResponseContext{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Contains(got, []byte(opaque)) || !bytes.Contains(got, []byte(`"effort":"max"`)) {
		t.Fatal("rewriting policy changed opaque history/tool data")
	}
}

func TestDoraLunaDuplicateFieldsKeepLastValue(t *testing.T) {
	raw := `{"model":"other","model":"gpt-6-luna","stream":true,"reasoning":{"effort":"low","effort":"high"}}`
	body, mapping, err := applyUpstreamReasoning("https://dora2api.xyz", "/v1/responses", io.NopCloser(strings.NewReader(raw)), mappedResponseContext{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if mapping.ClientEffort == nil || mapping.ClientEffort.Value != "high" || bytes.Count(got, []byte(`"effort"`)) != 1 || !bytes.Contains(got, []byte(`"effort":"max"`)) {
		t.Fatalf("duplicate fields changed policy: %s %+v", got, mapping)
	}
}
