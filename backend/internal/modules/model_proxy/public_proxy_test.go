package model_proxy

import (
	"bufio"
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
		{http.MethodPost, "/v1/responses", true},
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

func TestCleanupGateBodyDeletesBeforeFirstEventIsReleased(t *testing.T) {
	var cleanupCalls int
	body := newCleanupGateBody(
		io.NopCloser(strings.NewReader("data: first\n\n"+"data: second\n\n")),
		func() error {
			cleanupCalls++
			return nil
		},
	)

	first := make([]byte, len("data: first\n\n"))
	n, err := body.Read(first)
	if err != nil {
		t.Fatalf("first Read error: %v", err)
	}
	if string(first[:n]) != "data: first\n\n" {
		t.Fatalf("first event = %q", first[:n])
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls after first event = %d, want 1", cleanupCalls)
	}

	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("remaining Read error: %v", err)
	}
	if string(rest) != "data: second\n\n" {
		t.Fatalf("remaining events = %q", rest)
	}
}

func TestReadFirstSSEEventPreservesBufferedBytes(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("event: message\r\ndata: {}\r\n\r\nrest"))
	event, err := readFirstSSEEvent(reader)
	if err != nil {
		t.Fatalf("readFirstSSEEvent error: %v", err)
	}
	if string(event) != "event: message\r\ndata: {}\r\n\r\n" {
		t.Fatalf("event = %q", event)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("remaining reader error: %v", err)
	}
	if string(rest) != "rest" {
		t.Fatalf("remaining reader = %q", rest)
	}
}
