package model_proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamStartTimesOutAfterHeadersAndHeartbeats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": heartbeat\n\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	trackResponseHealth(response)
	started := time.Now()
	err = prepareStreamStart(context.Background(), response, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stream start timeout") {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout failed to release request: %s", elapsed)
	}
	if _, tracked := response.Body.(*healthResponseBody); !tracked {
		t.Fatal("lost model health observer")
	}
}

func TestPreparedAttemptDoesNotAcceptStalledStreamHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	started := time.Now()
	response, retryable, err := service.performAttemptWithResponseHeaderTimeout(request, route,
		func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(`{"model":"model","stream":true}`)), nil
		},
		nil, mappedResponseContext{}, false, 40*time.Millisecond)
	if err == nil || !retryable || response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("stalled stream accepted: response=%v retryable=%t error=%v", response, retryable, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("attempt exceeded timeout: %s", elapsed)
	}
	if success, observe := routeRequestResult(request.Context(), response, err); success || !observe {
		t.Fatal("stream timeout should count as one model request failure")
	}
}

func TestStreamStartReplaysPreludeAndDoesNotLimitStreamDuration(t *testing.T) {
	const prefix = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n: heartbeat\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	const tail = "data: {\"type\":\"response.completed\"}\n\n"
	releaseTail := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		select {
		case <-releaseTail:
			_, _ = io.WriteString(w, tail)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	trackResponseHealth(response)
	if err := prepareStreamStart(context.Background(), response, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	close(releaseTail)
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != prefix+tail {
		t.Fatalf("stream=%q err=%v", data, err)
	}
}

func TestStreamStartRetriesHTTP200ErrorBeforeCommitting(t *testing.T) {
	calls := 0
	response, err := attemptSmartGroupMember(context.Background(), func(time.Duration) (*http.Response, error) {
		calls++
		data := "data: {\"type\":\"error\",\"error\":{\"code\":\"overloaded\"}}\n\n"
		if calls == 2 {
			data = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}
		trackResponseHealth(response)
		return response, prepareStreamStart(context.Background(), response, time.Second)
	})
	if err != nil || calls != 2 || responseHealth(response).failed {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(data), "OK") {
		t.Fatalf("response=%q err=%v", data, err)
	}
}

func TestStreamStartPreservesMissingModelClassification(t *testing.T) {
	calls := 0
	_, err := attemptSmartGroupMember(context.Background(), func(time.Duration) (*http.Response, error) {
		calls++
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"error\":{\"code\":\"model_not_found\"}}\n\n"))}
		trackResponseHealth(response)
		return response, prepareStreamStart(context.Background(), response, time.Second)
	})
	if err == nil || calls != 1 {
		t.Fatalf("missing model should not retry: calls=%d err=%v", calls, err)
	}
}

func TestStreamStartRejectsTruncatedPreludeAndHonorsCancellation(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))}
	if err := prepareStreamStart(context.Background(), response, time.Second); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated prelude: %v", err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	response.Body = reader
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := prepareStreamStart(ctx, response, time.Second); err != context.Canceled {
		t.Fatalf("canceled context: %v", err)
	}
}

func TestStreamStartupEventsKeepReasoningAndToolCalls(t *testing.T) {
	for _, tc := range []struct {
		event   string
		startup bool
	}{
		{": ping\n\n", true},
		{"data: {\"type\":\"response.in_progress\"}\n\n", true},
		{"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n", true},
		{"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thought\"}}]}\n\n", false},
		{"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call1\"}]}}]}\n\n", false},
		{"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", false},
		{"data: [DONE]\n\n", false},
		{"event: error\n\n", false},
	} {
		if got := isStreamStartupEvent([]byte(tc.event)); got != tc.startup {
			t.Errorf("startup(%q)=%t want %t", tc.event, got, tc.startup)
		}
	}
}
