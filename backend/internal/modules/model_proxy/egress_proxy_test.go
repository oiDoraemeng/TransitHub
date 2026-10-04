package model_proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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
