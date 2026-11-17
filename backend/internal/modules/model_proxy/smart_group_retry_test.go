package model_proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSmartGroupSkipsPermanentAndQuotaRetries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			closed := false
			_, err := attemptSmartGroupMember(context.Background(), func(time.Duration) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Body: &cancelOnCloseBody{ReadCloser: io.NopCloser(strings.NewReader("failed")), cancel: func() { closed = true }}}, nil
			})
			if err == nil || calls != 1 || !closed {
				t.Fatalf("calls=%d closed=%v err=%v", calls, closed, err)
			}
		})
	}
}

func TestSmartGroupTimeoutMovesToAlternativeBeforeRetry(t *testing.T) {
	calls := 0
	_, err := attemptSmartGroupMemberWithPolicy(context.Background(), smartGroupRetryPolicy{hasAlternatives: true}, func(time.Duration) (*http.Response, error) {
		calls++
		return nil, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("timeout retried before failover: calls=%d err=%v", calls, err)
	}
}

func TestSmartGroupStartupBudgetBoundsRetryAndDoesNotRestart(t *testing.T) {
	deadline := time.Now().Add(400 * time.Millisecond)
	var timeouts []time.Duration
	_, err := attemptSmartGroupMemberWithPolicy(context.Background(), smartGroupRetryPolicy{deadline: deadline}, func(timeout time.Duration) (*http.Response, error) {
		timeouts = append(timeouts, timeout)
		return nil, errors.New("transient failure")
	})
	if err == nil || len(timeouts) != 2 || timeouts[0] > 400*time.Millisecond || timeouts[1] >= 200*time.Millisecond {
		t.Fatalf("budget restarted: timeouts=%v err=%v", timeouts, err)
	}
	calls := 0
	_, err = attemptSmartGroupMemberWithPolicy(context.Background(), smartGroupRetryPolicy{deadline: time.Now().Add(-time.Second)}, func(time.Duration) (*http.Response, error) {
		calls++
		return nil, nil
	})
	if !errors.Is(err, errSmartGroupStartupBudget) || calls != 0 {
		t.Fatalf("expired request reached upstream: calls=%d err=%v", calls, err)
	}
}

func TestPreparedAttemptStartupDeadlineDoesNotCancelAcceptedStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	service, route := preparedProbeService(t, server)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response, _, err := service.performAttemptWithResponseHeaderTimeout(request, route, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"model","stream":true}`)), nil
	}, nil, mappedResponseContext{}, false, 100*time.Millisecond)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer response.Body.Close()
	time.Sleep(150 * time.Millisecond)
	close(release)
	data, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(data), "[DONE]") || responseHealth(response).failed {
		t.Fatalf("accepted stream canceled by startup budget: %q %v", data, err)
	}
}
