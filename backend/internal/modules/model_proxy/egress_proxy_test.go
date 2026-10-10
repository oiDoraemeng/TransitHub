package model_proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNormalizeProxyURL(t *testing.T) {
	normalized, protocol, address, err := normalizeProxyURL(" SOCKS5://user:pass@proxy.example.com:1080/ ")
	if err != nil {
		t.Fatalf("normalize proxy URL: %v", err)
	}
	if normalized != "socks5://user:pass@proxy.example.com:1080" {
		t.Fatalf("normalized URL = %q", normalized)
	}
	if protocol != "socks5" {
		t.Fatalf("protocol = %q", protocol)
	}
	if address != "socks5://proxy.example.com:1080" {
		t.Fatalf("sanitized address = %q", address)
	}
}

func TestNormalizeProxyURLRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{
		"proxy.example.com:8080",
		"ftp://proxy.example.com:21",
		"http://proxy.example.com",
		"http://proxy.example.com:8080/path",
		"http://proxy.example.com:70000",
	} {
		if _, _, _, err := normalizeProxyURL(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestDirectModelClientIgnoresEnvironmentProxy(t *testing.T) {
	var proxyRequests atomic.Int32
	environmentProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "unexpected proxy request", http.StatusBadGateway)
	}))
	defer environmentProxy.Close()
	t.Setenv("HTTP_PROXY", environmentProxy.URL)
	t.Setenv("http_proxy", environmentProxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer target.Close()

	client, err := newModelHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("direct request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "direct" || proxyRequests.Load() != 0 {
		t.Fatalf("body=%q proxy requests=%d", body, proxyRequests.Load())
	}
}

func TestConfiguredModelProxyIsUsed(t *testing.T) {
	var proxyRequests atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		if r.URL.Host != "upstream.invalid" || r.URL.Path != "/v1/models" {
			http.Error(w, fmt.Sprintf("unexpected target %s", r.URL.String()), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxyServer.Close()

	client, err := newModelHTTPClient(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("http://upstream.invalid/v1/models")
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "proxied" || proxyRequests.Load() != 1 {
		t.Fatalf("body=%q proxy requests=%d", body, proxyRequests.Load())
	}
}

func TestResponseHeaderTimeoutStopsWaitingForHeaders(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := doWithResponseHeaderTimeout(client, request, 20*time.Millisecond)
	if response != nil {
		t.Fatal("timed out request must not return a response")
	}
	if err == nil || !strings.Contains(err.Error(), "response header timeout after 20ms") {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("header timeout took %s", elapsed)
	}
}

func TestResponseHeaderTimeoutDoesNotLimitBody(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("complete body")),
		}, nil
	})}
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doWithResponseHeaderTimeout(client, request, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	time.Sleep(30 * time.Millisecond)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "complete body" {
		t.Fatalf("body=%q", body)
	}
}
