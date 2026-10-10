package model_proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const proxyTestURL = "https://api.ipify.org?format=json"

type cachedEgressClient struct {
	ciphertext string
	client     *http.Client
}

func normalizeProxyURL(raw string) (normalized, protocol, address string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", errors.New("invalid proxy URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", "", "", errors.New("proxy URL must use http, https, socks5 or socks5h")
	}
	if parsed.Hostname() == "" || parsed.Port() == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("proxy URL must contain only scheme, host, port and optional credentials")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", "", "", errors.New("proxy port must be between 1 and 65535")
	}
	parsed.Path = ""
	address = parsed.Scheme + "://" + net.JoinHostPort(parsed.Hostname(), parsed.Port())
	return parsed.String(), parsed.Scheme, address, nil
}

func newModelHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// doWithResponseHeaderTimeout limits only the wait for response headers. Once
// headers arrive, streaming may continue for as long as the caller permits.
func doWithResponseHeaderTimeout(client *http.Client, request *http.Request, timeout time.Duration) (*http.Response, error) {
	if timeout <= 0 {
		return client.Do(request)
	}
	requestContext, cancel := context.WithCancel(request.Context())
	timer := time.AfterFunc(timeout, cancel)
	response, err := client.Do(request.WithContext(requestContext))
	if !timer.Stop() && request.Context().Err() == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		return nil, fmt.Errorf("upstream response header timeout after %s", timeout)
	}
	if err != nil {
		cancel()
		return response, err
	}
	if response == nil || response.Body == nil {
		cancel()
		return response, nil
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (s *Service) ListEgressProxies(ctx context.Context, userID string) ([]EgressProxy, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.repository.ListEgressProxies(ctx, userID, accountID)
}

func (s *Service) CreateEgressProxy(ctx context.Context, userID string, input CreateEgressProxyRequest) (EgressProxy, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return EgressProxy{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return EgressProxy{}, &requestError{Status: http.StatusBadRequest, Message: "proxy name is required"}
	}
	normalized, protocol, address, err := normalizeProxyURL(input.URL)
	if err != nil {
		return EgressProxy{}, &requestError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if s.cipher == nil || !s.cipher.Available() {
		return EgressProxy{}, errEncryptionKeyUnavailable
	}
	ciphertext, err := s.cipher.Encrypt(normalized)
	if err != nil {
		return EgressProxy{}, err
	}
	id, err := randomID("mproxy_")
	if err != nil {
		return EgressProxy{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	proxy := EgressProxy{ID: id, UserID: userID, AdminAccountID: accountID, Name: input.Name, Protocol: protocol, Address: address, URLCiphertext: ciphertext, Enabled: enabled}
	if err := s.repository.CreateEgressProxy(ctx, proxy); err != nil {
		return EgressProxy{}, err
	}
	created, err := s.repository.GetEgressProxy(ctx, id)
	if created == nil {
		return proxy, err
	}
	return *created, err
}

func (s *Service) UpdateEgressProxy(ctx context.Context, userID, proxyID string, input UpdateEgressProxyRequest) (EgressProxy, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return EgressProxy{}, err
	}
	proxy, err := s.repository.GetEgressProxy(ctx, proxyID)
	if err != nil || proxy == nil || proxy.UserID != userID || proxy.AdminAccountID != accountID {
		return EgressProxy{}, &requestError{Status: http.StatusNotFound, Message: "model proxy not found"}
	}
	if input.Name != nil {
		proxy.Name = strings.TrimSpace(*input.Name)
		if proxy.Name == "" {
			return EgressProxy{}, &requestError{Status: http.StatusBadRequest, Message: "proxy name is required"}
		}
	}
	if input.URL != nil && strings.TrimSpace(*input.URL) != "" {
		normalized, protocol, address, normalizeErr := normalizeProxyURL(*input.URL)
		if normalizeErr != nil {
			return EgressProxy{}, &requestError{Status: http.StatusBadRequest, Message: normalizeErr.Error()}
		}
		ciphertext, encryptErr := s.cipher.Encrypt(normalized)
		if encryptErr != nil {
			return EgressProxy{}, encryptErr
		}
		proxy.Protocol, proxy.Address, proxy.URLCiphertext = protocol, address, ciphertext
		proxy.LastTestStatus, proxy.LastTestError, proxy.LastTestExitIP = "untested", "", ""
		proxy.LastTestLatencyMS, proxy.LastTestedAt = nil, nil
	}
	if input.Enabled != nil {
		proxy.Enabled = *input.Enabled
	}
	if err := s.repository.UpdateEgressProxy(ctx, *proxy); err != nil {
		return EgressProxy{}, err
	}
	updated, err := s.repository.GetEgressProxy(ctx, proxy.ID)
	if updated == nil {
		return *proxy, err
	}
	return *updated, err
}

func (s *Service) DeleteEgressProxy(ctx context.Context, userID, proxyID string) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	return s.repository.DeleteEgressProxy(ctx, userID, accountID, proxyID)
}

func (s *Service) TestEgressProxy(ctx context.Context, userID, proxyID string) (EgressProxyTestResult, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return EgressProxyTestResult{}, err
	}
	proxy, err := s.repository.GetEgressProxy(ctx, proxyID)
	if err != nil || proxy == nil || proxy.UserID != userID || proxy.AdminAccountID != accountID {
		return EgressProxyTestResult{}, &requestError{Status: http.StatusNotFound, Message: "model proxy not found"}
	}
	proxyURL, err := s.cipher.Decrypt(proxy.URLCiphertext)
	if err != nil {
		return EgressProxyTestResult{}, err
	}
	client, err := newModelHTTPClient(proxyURL)
	if err != nil {
		return EgressProxyTestResult{}, err
	}
	defer client.CloseIdleConnections()
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(testCtx, http.MethodGet, proxyTestURL, nil)
	if err != nil {
		return EgressProxyTestResult{}, err
	}
	started := time.Now()
	response, requestErr := client.Do(request)
	latency := time.Since(started).Milliseconds()
	result := EgressProxyTestResult{LatencyMS: latency}
	if requestErr == nil {
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			requestErr = fmt.Errorf("proxy test returned HTTP %d", response.StatusCode)
		} else {
			var payload struct {
				IP string `json:"ip"`
			}
			if decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&payload); decodeErr != nil || strings.TrimSpace(payload.IP) == "" {
				requestErr = errors.New("proxy test returned an invalid response")
			} else {
				result.Success, result.ExitIP, result.Message = true, strings.TrimSpace(payload.IP), "proxy is reachable"
			}
		}
	}
	if requestErr != nil {
		result.Message = requestErr.Error()
	}
	if err := s.repository.SaveEgressProxyTest(ctx, proxy.ID, result); err != nil {
		return EgressProxyTestResult{}, err
	}
	return result, nil
}

func (s *Service) validateEgressProxy(ctx context.Context, userID, accountID, proxyID string) error {
	if strings.TrimSpace(proxyID) == "" {
		return nil
	}
	proxy, err := s.repository.GetEgressProxy(ctx, proxyID)
	if err != nil || proxy == nil || proxy.UserID != userID || proxy.AdminAccountID != accountID || !proxy.Enabled {
		return &requestError{Status: http.StatusBadRequest, Message: "an enabled model proxy in this workspace is required"}
	}
	return nil
}

func (s *Service) dataClientForRoute(ctx context.Context, route Route) (*http.Client, error) {
	if strings.TrimSpace(route.ProxyID) == "" {
		return s.dataClient, nil
	}
	proxy, err := s.repository.GetEgressProxy(ctx, route.ProxyID)
	if err != nil || proxy == nil || proxy.UserID != route.UserID || proxy.AdminAccountID != route.AdminAccountID {
		return nil, errors.New("selected model proxy is unavailable")
	}
	if !proxy.Enabled {
		return nil, errors.New("selected model proxy is disabled")
	}
	s.egressClientsMu.Lock()
	cached, ok := s.egressClients[proxy.ID]
	s.egressClientsMu.Unlock()
	if ok && cached.ciphertext == proxy.URLCiphertext {
		return cached.client, nil
	}
	proxyURL, err := s.cipher.Decrypt(proxy.URLCiphertext)
	if err != nil {
		return nil, err
	}
	client, err := newModelHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	s.egressClientsMu.Lock()
	if s.egressClients == nil {
		s.egressClients = make(map[string]cachedEgressClient)
	}
	if previous, exists := s.egressClients[proxy.ID]; exists {
		if previous.ciphertext == proxy.URLCiphertext {
			s.egressClientsMu.Unlock()
			client.CloseIdleConnections()
			return previous.client, nil
		}
		previous.client.CloseIdleConnections()
	}
	s.egressClients[proxy.ID] = cachedEgressClient{ciphertext: proxy.URLCiphertext, client: client}
	s.egressClientsMu.Unlock()
	return client, nil
}
