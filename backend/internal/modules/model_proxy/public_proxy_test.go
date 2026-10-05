package model_proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSupportedPublicEndpoint(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/v1/models", true},
		{http.MethodGet, "/v1beta/models", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodGet, "/v1/chat/completions", false},
		{http.MethodPost, "/v1/embeddings", false},
	}
	for _, test := range tests {
		if got := supportedPublicEndpoint(test.method, test.path); got != test.want {
			t.Errorf("supportedPublicEndpoint(%q, %q)=%v want %v", test.method, test.path, got, test.want)
		}
	}
}

func TestCanonicalPublicPath(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
		ok     bool
	}{
		{http.MethodGet, "/v1/models", "/v1/models", true},
		{http.MethodGet, "/v1beta/models", "/v1/models", true},
		{http.MethodPost, "/v1/responses", "/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", "/v1/responses", true},
		{http.MethodPost, "/v1/chat/completions", "/v1/chat/completions", true},
		{http.MethodGet, "/v1/responses/compact", "", false},
		{http.MethodPost, "/v1beta/models", "", false},
	}
	for _, test := range tests {
		got, ok := canonicalPublicPath(test.method, test.path)
		if got != test.want || ok != test.ok {
			t.Errorf("canonicalPublicPath(%q, %q)=(%q, %v), want (%q, %v)", test.method, test.path, got, ok, test.want, test.ok)
		}
	}
}

func TestWaitForProxyRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if waitForProxyRetry(ctx, 0) {
		t.Fatal("canceled request must not retry")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("canceled retry wait should return immediately")
	}
}

func TestReplayBodyMemoryAndModel(t *testing.T) {
	body, err := newReplayBody(io.NopCloser(strings.NewReader(`{"model":"gpt-test","input":"hello"}`)), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if body.file != nil {
		t.Fatal("small body should remain in memory")
	}
	model, err := body.ModelID()
	if err != nil || model != "gpt-test" {
		t.Fatalf("ModelID=%q err=%v", model, err)
	}
	for range 2 {
		reader, err := body.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(reader)
		reader.Close()
		if !strings.Contains(string(data), "hello") {
			t.Fatalf("replayed body = %q", data)
		}
	}
}

func TestReplayBodySpoolsAndLimits(t *testing.T) {
	large := `{"model":"gpt-test","input":"` + strings.Repeat("x", memoryReplayLimit+128) + `"}`
	body, err := newReplayBody(io.NopCloser(strings.NewReader(large)), int64(len(large)+1))
	if err != nil {
		t.Fatal(err)
	}
	if body.file == nil {
		t.Fatal("large body should use a temporary file")
	}
	name := body.file.Name()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newReplayBody(io.NopCloser(strings.NewReader("too large")), 3); err != errBodyTooLarge {
		t.Fatalf("size error=%v", err)
	}
	_ = name
}
