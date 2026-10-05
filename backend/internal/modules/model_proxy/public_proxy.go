package model_proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"transithub/backend/internal/modules/upstream"
)

const (
	memoryReplayLimit  = 1 << 20
	directMaxAttempts  = 3
	firstSSEEventLimit = 1 << 20
)

func (s *Service) PublicHandler() http.Handler {
	return http.HandlerFunc(s.handlePublic)
}

func (s *Service) handlePublic(w http.ResponseWriter, r *http.Request) {
	if !supportedPublicEndpoint(r.Method, r.URL.Path) {
		writeOpenAIError(w, http.StatusNotFound, "unsupported_endpoint", "only /v1/models, /v1/responses and /v1/chat/completions are supported")
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing Bearer API key")
		return
	}
	target, err := s.ResolvePublicTarget(r.Context(), token)
	if err != nil {
		var reqErr *requestError
		if errors.As(err, &reqErr) {
			writeOpenAIError(w, reqErr.Status, "invalid_api_key", reqErr.Message)
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "failed to resolve proxy key")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" && target.Group != nil {
		s.writeGroupModels(w, target.Group)
		return
	}
	if target.Route != nil {
		s.proxyDirect(w, r, *target.Route)
		return
	}
	s.proxySmartGroup(w, r, *target.Group)
}

func supportedPublicEndpoint(method, path string) bool {
	return (method == http.MethodGet && path == "/v1/models") ||
		(method == http.MethodPost && (path == "/v1/responses" || path == "/v1/chat/completions"))
}

func (s *Service) writeGroupModels(w http.ResponseWriter, group *SmartGroup) {
	data := make([]map[string]any, 0, len(group.Models))
	for _, model := range group.Models {
		raw := make(map[string]any, len(model.Raw)+1)
		for key, value := range model.Raw {
			raw[key] = value
		}
		raw["id"] = model.ID
		if _, ok := raw["object"]; !ok {
			raw["object"] = "model"
		}
		raw["effective_concurrency"] = model.EffectiveConcurrency
		data = append(data, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ModelsResponse{Object: "list", Data: data})
}

func (s *Service) proxyDirect(w http.ResponseWriter, incoming *http.Request, route Route) {
	leaseID, _ := randomID("req_")
	lease, ok, err := s.limiter.Acquire(incoming.Context(), route, newLeaseID(leaseID, route.ID))
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "concurrency_backend_unavailable", "concurrency service is unavailable")
		return
	}
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusTooManyRequests, "route_concurrency_exceeded", "proxy route concurrency is full")
		return
	}
	defer lease.Release(context.Background())
	source := incoming.Body
	if source == nil {
		source = http.NoBody
	}
	replay, err := newReplayBody(source, s.maxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes", s.maxRequestBytes))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	defer replay.Close()

	var lastStatus int
	for attempt := 0; attempt < directMaxAttempts; attempt++ {
		response, retryable, attemptErr := s.performAttempt(incoming, route, replay.Open)
		if attemptErr == nil && !retryable {
			s.writeUpstreamResponse(w, response)
			return
		}
		if response != nil {
			lastStatus = response.StatusCode
			response.Body.Close()
		}
		if attempt+1 < directMaxAttempts && waitForProxyRetry(incoming.Context(), attempt) {
			continue
		}
		break
	}
	message := fmt.Sprintf("upstream remained unavailable after %d automatic attempts", directMaxAttempts)
	if lastStatus != 0 {
		message += fmt.Sprintf(" (last status %d)", lastStatus)
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable", message)
}

func waitForProxyRetry(ctx context.Context, attempt int) bool {
	delay := 250 * time.Millisecond * time.Duration(1<<attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) proxySmartGroup(w http.ResponseWriter, incoming *http.Request, group SmartGroup) {
	replay, err := newReplayBody(incoming.Body, s.maxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes", s.maxRequestBytes))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	defer replay.Close()
	modelID, err := replay.ModelID()
	if err != nil || strings.TrimSpace(modelID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	candidates, err := s.repository.ListGroupRoutes(incoming.Context(), group.ID, true, modelID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "failed to load smart group members")
		return
	}
	if len(candidates) == 0 {
		s.refreshGroupModels(incoming.Context(), group.ID)
		candidates, err = s.repository.ListGroupRoutes(incoming.Context(), group.ID, true, modelID)
	}
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "no enabled smart group member supports this model")
		return
	}
	requestID, _ := randomID("req_")
	remaining := append([]Route(nil), candidates...)
	var lastErr error
	for len(remaining) > 0 {
		lease, route, acquireErr := s.limiter.AcquireBest(incoming.Context(), remaining, requestID)
		if acquireErr != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "concurrency_backend_unavailable", "concurrency service is unavailable")
			return
		}
		if route == nil {
			break
		}
		remaining = withoutRoute(remaining, route.ID)
		response, retryable, attemptErr := s.performAttempt(incoming, *route, replay.Open)
		if attemptErr != nil {
			lastErr = attemptErr
			lease.Release(context.Background())
			continue
		}
		if retryable {
			lastErr = fmt.Errorf("upstream returned %d", response.StatusCode)
			response.Body.Close()
			lease.Release(context.Background())
			continue
		}
		s.writeUpstreamResponse(w, response)
		lease.Release(context.Background())
		return
	}
	if lastErr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "all_upstreams_failed", lastErr.Error())
		return
	}
	w.Header().Set("Retry-After", "1")
	writeOpenAIError(w, http.StatusTooManyRequests, "smart_group_concurrency_exceeded", "all eligible smart group members are at capacity")
}

func (s *Service) performAttempt(incoming *http.Request, route Route, bodyFactory func() (io.ReadCloser, error)) (*http.Response, bool, error) {
	client, err := s.dataClientForRoute(incoming.Context(), route)
	if err != nil {
		return nil, true, err
	}
	secret, cleanupJob, err := s.createRemoteKey(incoming.Context(), route)
	if err != nil {
		return nil, true, err
	}
	body, err := bodyFactory()
	if err != nil {
		s.beginDelete(cleanupJob)
		return nil, true, err
	}
	baseURL := s.mustSiteBaseURL(incoming.Context(), route.SiteID)
	if baseURL == "" {
		body.Close()
		s.beginDelete(cleanupJob)
		return nil, true, errors.New("upstream site is unavailable")
	}
	request, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, strings.TrimRight(baseURL, "/")+incoming.URL.RequestURI(), body)
	if err != nil {
		body.Close()
		s.beginDelete(cleanupJob)
		return nil, true, err
	}
	copyRequestHeaders(request.Header, incoming.Header)
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("User-Agent", upstream.BrowserUserAgent)
	response, err := client.Do(request)
	if err != nil {
		s.beginDelete(cleanupJob)
		return nil, true, err
	}
	retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	// http.Client.Do returns after response headers are available. For a successful
	// SSE response, defer cleanup until the first complete event has arrived, then
	// delete synchronously before the event is exposed to the downstream client.
	// This removes the timing dependency on downstream/TCP buffering when testing
	// an in-flight streaming request. Retryable and non-streaming responses retain
	// the asynchronous cleanup path.
	if !retryable && isEventStream(response.Header) {
		response.Body = newCleanupGateBody(response.Body, func() error {
			return s.deleteCleanupJobNow(cleanupJob)
		})
	} else {
		s.beginDelete(cleanupJob)
	}
	return response, retryable, nil
}

func isEventStream(header http.Header) bool {
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

var errSSEEventTooLarge = errors.New("first SSE event exceeds limit")

// cleanupGateBody holds the first complete SSE event in the proxy until the
// cleanup callback succeeds. The response headers may already be visible to
// the client, but no response body bytes are written before the callback.
type cleanupGateBody struct {
	source      *bufio.Reader
	close       io.Closer
	onGate      func() error
	once        sync.Once
	gateErr     error
	pending     []byte
	terminalErr error
}

func newCleanupGateBody(source io.ReadCloser, onGate func() error) io.ReadCloser {
	return &cleanupGateBody{
		source: bufio.NewReaderSize(source, 32*1024),
		close:  source,
		onGate: onGate,
	}
}

func (b *cleanupGateBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.pending, b.terminalErr = readFirstSSEEvent(b.source)
		b.gateErr = b.onGate()
	})
	if b.gateErr != nil {
		return 0, b.gateErr
	}
	if b.terminalErr != nil && !errors.Is(b.terminalErr, io.EOF) {
		return 0, b.terminalErr
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	if b.terminalErr != nil {
		err := b.terminalErr
		b.terminalErr = nil
		return 0, err
	}
	return b.source.Read(p)
}

func (b *cleanupGateBody) Close() error {
	return b.close.Close()
}

func readFirstSSEEvent(source *bufio.Reader) ([]byte, error) {
	var event []byte
	for len(event) < firstSSEEventLimit {
		line, err := source.ReadBytes('\n')
		event = append(event, line...)
		if bytes.HasSuffix(event, []byte("\n\n")) || bytes.HasSuffix(event, []byte("\r\n\r\n")) {
			return event, nil
		}
		if err != nil {
			return event, err
		}
	}
	return event, errSSEEventTooLarge
}

func (s *Service) writeUpstreamResponse(w http.ResponseWriter, response *http.Response) {
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func withoutRoute(routes []Route, id string) []Route {
	result := routes[:0]
	for _, route := range routes {
		if route.ID != id {
			result = append(result, route)
		}
	}
	return result
}

var hopHeaders = map[string]struct{}{
	"Connection": {}, "Proxy-Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {},
	"Proxy-Authorization": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {},
}

func copyRequestHeaders(destination, source http.Header) {
	for key, values := range source {
		if _, skip := hopHeaders[http.CanonicalHeaderKey(key)]; skip || strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if _, skip := hopHeaders[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
}

var errBodyTooLarge = errors.New("request body too large")

type replayBody struct {
	memory []byte
	file   *os.File
	size   int64
}

func newReplayBody(source io.ReadCloser, maxBytes int64) (*replayBody, error) {
	defer source.Close()
	result := &replayBody{}
	var memory bytes.Buffer
	buffer := make([]byte, 32*1024)
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			result.size += int64(n)
			if result.size > maxBytes {
				result.Close()
				return nil, errBodyTooLarge
			}
			if result.file == nil && result.size <= memoryReplayLimit {
				_, _ = memory.Write(buffer[:n])
			} else {
				if result.file == nil {
					result.file, err = os.CreateTemp("", "transithub-proxy-body-*")
					if err != nil {
						return nil, err
					}
					if _, err = result.file.Write(memory.Bytes()); err != nil {
						result.Close()
						return nil, err
					}
					memory.Reset()
				}
				if _, err = result.file.Write(buffer[:n]); err != nil {
					result.Close()
					return nil, err
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			result.Close()
			return nil, err
		}
	}
	if result.file == nil {
		result.memory = append([]byte(nil), memory.Bytes()...)
	}
	return result, nil
}

func (r *replayBody) Open() (io.ReadCloser, error) {
	if r.file == nil {
		return io.NopCloser(bytes.NewReader(r.memory)), nil
	}
	return os.Open(r.file.Name())
}

func (r *replayBody) ModelID() (string, error) {
	reader, err := r.Open()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	var value struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(reader).Decode(&value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value.Model), nil
}

func (r *replayBody) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	name := r.file.Name()
	_ = r.file.Close()
	r.file = nil
	return os.Remove(name)
}
