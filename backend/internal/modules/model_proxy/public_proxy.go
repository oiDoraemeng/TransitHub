package model_proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	smartGroupAttempts = 2
	firstSSEEventLimit = 1 << 20
)

func (s *Service) PublicHandler() http.Handler {
	return http.HandlerFunc(s.handlePublic)
}

func (s *Service) handlePublic(w http.ResponseWriter, r *http.Request) {
	if !supportedPublicEndpoint(r.Method, r.URL.Path) {
		writeOpenAIError(w, http.StatusNotFound, "unsupported_endpoint", "only /v1/models, /v1/embeddings, /v1/chat/completions, /v1/responses, /v1/responses/compact, /v1/messages and /v1beta/models/* are supported")
		return
	}
	token := publicAPIKey(r.Header)
	if token == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing API key")
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
	if r.Method == http.MethodGet && r.URL.Path == "/v1beta/models" && target.Group != nil {
		s.writeGroupGeminiModels(w, target.Group)
		return
	}
	if target.Route != nil {
		s.proxyDirect(w, r, *target.Route)
		return
	}
	s.proxySmartGroup(w, r, *target.Group)
}

func supportedPublicEndpoint(method, path string) bool {
	if strings.HasPrefix(path, "/v1beta/") {
		return supportedGeminiEndpoint(method, path)
	}
	if method == http.MethodGet {
		return path == "/v1/models"
	}
	if method == http.MethodPost {
		switch path {
		case "/v1/embeddings", "/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages":
			return true
		}
	}
	return false
}

func supportedGeminiEndpoint(method, path string) bool {
	const modelsPath = "/v1beta/models"
	if path == modelsPath {
		return method == http.MethodGet
	}
	if !strings.HasPrefix(path, modelsPath+"/") {
		return false
	}
	modelAction := strings.TrimPrefix(path, modelsPath+"/")
	if modelAction == "" || strings.Contains(modelAction, "/") {
		return false
	}
	if method == http.MethodGet {
		return !strings.Contains(modelAction, ":")
	}
	model, action, ok := strings.Cut(modelAction, ":")
	if method != http.MethodPost || !ok || strings.TrimSpace(model) == "" {
		return false
	}
	return action == "generateContent" || action == "streamGenerateContent"
}

func (s *Service) writeGroupModels(w http.ResponseWriter, group *SmartGroup) {
	data := make([]map[string]any, 0, len(group.Models))
	for _, model := range groupModelsWithMappings(group) {
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

func (s *Service) writeGroupGeminiModels(w http.ResponseWriter, group *SmartGroup) {
	models := make([]map[string]any, 0, len(group.Models))
	for _, model := range groupModelsWithMappings(group) {
		id := strings.TrimPrefix(model.ID, "models/")
		models = append(models, map[string]any{
			"name":                       "models/" + id,
			"baseModelId":                id,
			"displayName":                id,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func groupModelsWithMappings(group *SmartGroup) []Model {
	return group.Models
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
		response, retryable, attemptErr := s.performAttempt(incoming, route, replay.Open, nil, mappedResponseContext{}, false)
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
	modelID, err := requestModelID(incoming, replay)
	if err != nil || strings.TrimSpace(modelID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	candidates, err := s.smartGroupCandidates(incoming.Context(), group, modelID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "failed to load smart group members")
		return
	}
	if len(candidates) == 0 {
		s.refreshGroupModels(incoming.Context(), group.ID)
		candidates, err = s.smartGroupCandidates(incoming.Context(), group, modelID)
	}
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "no enabled smart group member supports this model")
		return
	}
	keywordFiltering := false
	for _, candidate := range candidates {
		if candidate.KeywordCheckEnabled {
			keywordFiltering = true
			break
		}
	}
	var inputText string
	var policyErr error
	var streaming bool
	var inputTokens int
	if keywordFiltering {
		streaming, inputTokens, inputText, policyErr = requestPolicyFactsWithText(incoming, replay)
	} else {
		streaming, inputTokens, policyErr = requestPolicyFacts(incoming, replay)
	}
	if policyErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "failed to parse request body")
		return
	}
	eligible := make([]Route, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.StreamOnly && !streaming {
			continue
		}
		if candidate.MinInputTokens > 0 && inputTokens < candidate.MinInputTokens {
			continue
		}
		eligible = append(eligible, candidate)
	}
	if keywordFiltering {
		matchedRoutes := matchExcludedKeywordRoutes(strings.ToLower(inputText), eligible)
		filtered := eligible[:0]
		for _, candidate := range eligible {
			if _, matched := matchedRoutes[candidate.ID]; matched {
				continue
			}
			filtered = append(filtered, candidate)
		}
		eligible = filtered
	}
	if len(eligible) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "no_eligible_member", "no smart group member matches the request policy")
		return
	}
	requestID, _ := randomID("req_")
	remaining := append([]Route(nil), eligible...)
	var lastErr error
	rateLimited := false
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
		allowed, rateErr := s.limiter.AllowRequestsPerMinute(incoming.Context(), group.ID, route.ID, route.RequestsPerMinute, requestID)
		if rateErr != nil {
			lease.Release(context.Background())
			writeOpenAIError(w, http.StatusServiceUnavailable, "request_rate_backend_unavailable", "request rate service is unavailable")
			return
		}
		if !allowed {
			rateLimited = true
			lease.Release(context.Background())
			continue
		}
		responseMapping, responseMappingErr := newMappedResponseContext(incoming, replay, modelID, route.ModelMapping)
		if responseMappingErr != nil {
			lastErr = responseMappingErr
			lease.Release(context.Background())
			continue
		}
		response, attemptErr := attemptSmartGroupMember(incoming.Context(), func() (*http.Response, error) {
			response, _, err := s.performAttempt(incoming, *route, replay.Open, route.ModelMapping, responseMapping, !streaming)
			return response, err
		})
		if attemptErr != nil {
			lastErr = fmt.Errorf("member %s failed after %d attempts: %w", route.Name, smartGroupAttempts, attemptErr)
			lease.Release(context.Background())
			if incoming.Context().Err() != nil {
				return
			}
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
	if rateLimited {
		writeOpenAIError(w, http.StatusTooManyRequests, "smart_group_rate_limit_exceeded", "all eligible smart group members reached their per-minute request limit")
		return
	}
	writeOpenAIError(w, http.StatusTooManyRequests, "smart_group_concurrency_exceeded", "all eligible smart group members are at capacity")
}

// attemptSmartGroupMember retries once after the selected member's first failure.
// Any non-2xx response is an upstream failure here, including authentication
// and request errors, because another member may accept the same request.
func attemptSmartGroupMember(ctx context.Context, attempt func() (*http.Response, error)) (*http.Response, error) {
	var lastErr error
	for attemptIndex := 0; attemptIndex < smartGroupAttempts; attemptIndex++ {
		response, err := attempt()
		if err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		if response != nil {
			lastErr = fmt.Errorf("upstream returned %d", response.StatusCode)
			response.Body.Close()
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("upstream returned no response")
		}
		if attemptIndex+1 < smartGroupAttempts && !waitForProxyRetry(ctx, attemptIndex) {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (s *Service) smartGroupCandidates(ctx context.Context, group SmartGroup, modelID string) ([]Route, error) {
	return s.repository.ListGroupRoutes(ctx, group.ID, true, modelID)
}

func requestModelID(incoming *http.Request, replay *replayBody) (string, error) {
	const geminiModelsPrefix = "/v1beta/models/"
	if strings.HasPrefix(incoming.URL.Path, geminiModelsPrefix) {
		modelAction := strings.TrimPrefix(incoming.URL.Path, geminiModelsPrefix)
		if separator := strings.IndexByte(modelAction, ':'); separator >= 0 {
			modelAction = modelAction[:separator]
		}
		if modelID := strings.TrimSpace(strings.Trim(modelAction, "/")); modelID != "" {
			return modelID, nil
		}
	}
	return replay.ModelID()
}

func applyModelMapping(incoming *http.Request, body io.ReadCloser, enabled bool, mapping map[string]string) (*http.Request, io.ReadCloser, error) {
	if !enabled || len(mapping) == 0 {
		return incoming, body, nil
	}
	if source, action, ok := nativeGeminiModelAction(incoming.URL.Path); ok {
		if target := strings.TrimSpace(mapping[source]); target != "" && target != source {
			mapped := incoming.Clone(incoming.Context())
			mapped.URL.Path = "/v1beta/models/" + target + ":" + action
			mapped.URL.RawPath = ""
			return mapped, body, nil
		}
		return incoming, body, nil
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil {
		return nil, nil, err
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return incoming, io.NopCloser(bytes.NewReader(raw)), nil
	}
	var source string
	if err := json.Unmarshal(payload["model"], &source); err != nil {
		return incoming, io.NopCloser(bytes.NewReader(raw)), nil
	}
	target := strings.TrimSpace(mapping[strings.TrimSpace(source)])
	if target == "" || target == source {
		return incoming, io.NopCloser(bytes.NewReader(raw)), nil
	}
	encodedTarget, err := json.Marshal(target)
	if err != nil {
		return nil, nil, err
	}
	payload["model"] = encodedTarget
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	return incoming, io.NopCloser(bytes.NewReader(encoded)), nil
}

func nativeGeminiModelAction(path string) (string, string, bool) {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	modelAction := strings.TrimPrefix(path, prefix)
	separator := strings.LastIndexByte(modelAction, ':')
	if separator <= 0 || separator == len(modelAction)-1 {
		return "", "", false
	}
	model, action := modelAction[:separator], modelAction[separator+1:]
	if strings.Contains(model, "/") || (action != "generateContent" && action != "streamGenerateContent") {
		return "", "", false
	}
	return model, action, true
}

func (s *Service) performAttempt(incoming *http.Request, route Route, bodyFactory func() (io.ReadCloser, error), modelMapping map[string]string, responseMapping mappedResponseContext, synchronousRequest bool) (*http.Response, bool, error) {
	client, err := s.dataClientForRoute(incoming.Context(), route)
	if err != nil {
		return nil, true, err
	}
	secret, cleanupJob, needsCleanup, err := s.acquireRouteSecret(incoming.Context(), route)
	if err != nil {
		return nil, true, err
	}
	cleanup := func() {
		if needsCleanup {
			s.beginDelete(cleanupJob)
		}
	}
	body, err := bodyFactory()
	if err != nil {
		cleanup()
		return nil, true, err
	}
	protocolIncoming, protocolBody, err := applyModelMapping(incoming, body, route.ModelMappingEnabled, modelMapping)
	if err != nil {
		cleanup()
		return nil, true, err
	}
	protocolRequest, err := adaptUpstreamProtocol(protocolIncoming, protocolBody)
	if err != nil {
		cleanup()
		return nil, true, err
	}
	body = protocolRequest.body
	baseURL := s.mustSiteBaseURL(incoming.Context(), route.SiteID)
	if baseURL == "" {
		body.Close()
		cleanup()
		return nil, true, errors.New("upstream site is unavailable")
	}
	request, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, strings.TrimRight(baseURL, "/")+protocolRequest.requestURI, body)
	if err != nil {
		body.Close()
		cleanup()
		return nil, true, err
	}
	copyRequestHeaders(request.Header, incoming.Header)
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("User-Agent", upstream.BrowserUserAgent)
	var deadlineCleanup *headerDeadlineCleanup
	if synchronousRequest && route.SyncKeyDeleteEnabled && needsCleanup {
		delayMS := route.SyncKeyDeleteDelayMS
		if delayMS <= 0 {
			delayMS = defaultSyncKeyDeleteDelayMS
		}
		deadlineCleanup = newHeaderDeadlineCleanup(time.Duration(delayMS)*time.Millisecond, func() error {
			return s.deleteCleanupJobNow(cleanupJob)
		})
	}
	response, err := client.Do(request)
	if deadlineCleanup != nil {
		if cleanupErr := deadlineCleanup.finishAtHeaders(); cleanupErr != nil {
			log.Printf("[model-proxy] synchronous key cleanup job_id=%s err=%v", cleanupJob.ID, cleanupErr)
		}
	}
	if err != nil {
		if deadlineCleanup == nil {
			cleanup()
		}
		return nil, true, err
	}
	retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	if !retryable && responseMapping.Enabled && route.ModelMappingEnabled {
		if err := rewriteMappedResponse(response, responseMapping); err != nil {
			response.Body.Close()
			if deadlineCleanup == nil {
				cleanup()
			}
			return nil, true, err
		}
	}
	if !retryable && protocolRequest.responseAdapter != nil {
		if err := protocolRequest.responseAdapter(response); err != nil {
			response.Body.Close()
			if deadlineCleanup == nil {
				cleanup()
			}
			return nil, true, err
		}
	}
	// http.Client.Do returns after response headers are available. For a successful
	// SSE response, defer cleanup until the first complete event has arrived, then
	// delete synchronously before the event is exposed to the downstream client.
	// This removes the timing dependency on downstream/TCP buffering when testing
	// an in-flight streaming request. Non-streaming smart-group members may use
	// the separate header-deadline cleanup configured above.
	if !retryable && isEventStream(response.Header) && needsCleanup && deadlineCleanup == nil {
		response.Body = newCleanupGateBody(response.Body, func() error {
			return s.deleteCleanupJobNow(cleanupJob)
		})
	} else if deadlineCleanup == nil {
		cleanup()
	}
	return response, retryable, nil
}

// headerDeadlineCleanup starts at the configured deadline unless response
// headers arrive first. finishAtHeaders also waits for the single cleanup call,
// so a fast synchronous response is not exposed before deletion completes.
type headerDeadlineCleanup struct {
	once    sync.Once
	timer   *time.Timer
	done    chan error
	cleanup func() error
}

func newHeaderDeadlineCleanup(delay time.Duration, cleanup func() error) *headerDeadlineCleanup {
	result := &headerDeadlineCleanup{done: make(chan error, 1), cleanup: cleanup}
	result.timer = time.AfterFunc(delay, result.start)
	return result
}

func (c *headerDeadlineCleanup) start() {
	c.once.Do(func() {
		go func() { c.done <- c.cleanup() }()
	})
}

func (c *headerDeadlineCleanup) finishAtHeaders() error {
	c.timer.Stop()
	c.start()
	return <-c.done
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
		cleanupDone := make(chan error, 1)
		go func() { cleanupDone <- b.onGate() }()
		b.pending, b.terminalErr = readFirstSSEEvent(b.source)
		b.gateErr = <-cleanupDone
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
		if _, skip := hopHeaders[http.CanonicalHeaderKey(key)]; skip || isCredentialHeader(key) || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Accept-Encoding") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isCredentialHeader(key string) bool {
	return strings.EqualFold(key, "Authorization") ||
		strings.EqualFold(key, "x-api-key") ||
		strings.EqualFold(key, "x-goog-api-key")
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

func publicAPIKey(header http.Header) string {
	if token := bearerToken(header.Get("Authorization")); token != "" {
		return token
	}
	if token := strings.TrimSpace(header.Get("x-api-key")); token != "" {
		return token
	}
	return strings.TrimSpace(header.Get("x-goog-api-key"))
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

func requestPolicyFacts(incoming *http.Request, replay *replayBody) (bool, int, error) {
	if isNativeGeminiStreamingRequest(incoming) {
		return true, 0, nil
	}
	streaming, inputTokens, _, err := requestPolicyFactsBody(incoming, replay, false)
	return streaming, inputTokens, err
}

func requestPolicyFactsWithText(incoming *http.Request, replay *replayBody) (bool, int, string, error) {
	return requestPolicyFactsBody(incoming, replay, true)
}

func requestPolicyFactsBody(incoming *http.Request, replay *replayBody, collectText bool) (bool, int, string, error) {
	nativeStreaming := isNativeGeminiStreamingRequest(incoming)
	reader, err := replay.Open()
	if err != nil {
		return false, 0, "", err
	}
	defer reader.Close()
	var payload any
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return false, 0, "", err
	}
	streaming := false
	if object, ok := payload.(map[string]any); ok {
		streaming, _ = object["stream"].(bool)
	}
	if nativeStreaming {
		streaming = true
	}
	characters := 0
	var inputText *strings.Builder
	var textBuilder strings.Builder
	if collectText {
		inputText = &textBuilder
	}
	collectInputText(payload, "", &characters, inputText)
	return streaming, (characters + 3) / 4, textBuilder.String(), nil
}

func isNativeGeminiStreamingRequest(incoming *http.Request) bool {
	return strings.HasSuffix(strings.TrimSpace(incoming.URL.Path), ":streamGenerateContent") || incoming.URL.Query().Get("alt") == "sse"
}

func collectInputText(value any, key string, characters *int, inputText *strings.Builder) {
	switch current := value.(type) {
	case string:
		if isInputTextKey(key) {
			*characters += len([]rune(current))
			if inputText != nil {
				inputText.WriteString(current)
				inputText.WriteByte('\n')
			}
		}
	case []any:
		for _, item := range current {
			collectInputText(item, key, characters, inputText)
		}
	case map[string]any:
		for childKey, child := range current {
			collectInputText(child, childKey, characters, inputText)
		}
	}
}

func matchExcludedKeywordRoutes(input string, routes []Route) map[string]struct{} {
	keywordRoutes := make(map[string][]string)
	for _, route := range routes {
		if !route.KeywordCheckEnabled {
			continue
		}
		seen := make(map[string]struct{}, len(route.ExcludedKeywords))
		for _, keyword := range route.ExcludedKeywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword == "" {
				continue
			}
			if _, duplicate := seen[keyword]; duplicate {
				continue
			}
			seen[keyword] = struct{}{}
			keywordRoutes[keyword] = append(keywordRoutes[keyword], route.ID)
		}
	}

	matchedRoutes := make(map[string]struct{})
	for keyword, routeIDs := range keywordRoutes {
		if !strings.Contains(input, keyword) {
			continue
		}
		for _, routeID := range routeIDs {
			matchedRoutes[routeID] = struct{}{}
		}
	}
	return matchedRoutes
}

func isInputTextKey(key string) bool {
	switch strings.ToLower(key) {
	case "content", "text", "input", "prompt", "parts", "messages", "systeminstruction":
		return true
	default:
		return false
	}
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
