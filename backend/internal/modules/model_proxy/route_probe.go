package model_proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

func writeRoutePaused(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "180")
	writeOpenAIError(w, http.StatusServiceUnavailable, "route_temporarily_paused", "eligible upstream routes are temporarily paused; recovery probes run every 3 minutes")
}

func writeModelPaused(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeOpenAIError(w, http.StatusServiceUnavailable, "model_temporarily_paused", "eligible upstream routes for this model are isolated; recovery probes are scheduled at minutes 1, 3, 5 and 10 after isolation, then every 10 minutes; other models remain available")
}

// Client cancellation and invalid request parameters say nothing about route
// health. Other failures (including authentication, quota and transport errors)
// count once per completed route request, after all its retries have finished.
func routeRequestResult(ctx context.Context, response *http.Response, err error) (success, observe bool) {
	if ctx.Err() != nil {
		return false, false
	}
	if response != nil && (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnprocessableEntity) && !responseHealth(response).modelMissing {
		return false, false
	}
	return err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 && !responseHealth(response).failed, true
}

func (s *Service) observeRouteRequest(ctx context.Context, lease *Lease, spec routeProbeSpec, response *http.Response, requestErr error, route Route) bool {
	if s.limiter == nil || lease == nil {
		return false
	}
	success, observe := routeRequestResult(ctx, response, requestErr)
	if !observe {
		return false
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var paused bool
	var err error
	health := responseHealth(response)
	// An HTTP response proves the route is reachable. 5xx/429/403 can be
	// specific to the requested model; only credential-wide failures promote
	// them to a route circuit. Transport errors retain the route circuit.
	modelScoped := lease.modelHealthID != "" && response != nil &&
		(health.modelMissing || response.StatusCode != http.StatusUnauthorized && !health.credentialError)
	if modelScoped {
		paused, err = s.limiter.ObserveModelResult(stateCtx, lease, success, health.modelMissing, route, spec)
	}
	if !modelScoped {
		paused, err = s.limiter.ObserveRouteResult(stateCtx, lease, success, spec)
	}
	if err != nil {
		log.Printf("[model-proxy] route health update route_id=%s err=%v", lease.routeID, err)
	}
	return paused
}

func (s *Service) routeProbeLoop(ctx context.Context) {
	defer s.wg.Done()
	if s.limiter == nil {
		return
	}
	type probeJob struct {
		id    string
		model bool
	}
	jobs := make(chan probeJob, routeProbeWorkers)
	var workers sync.WaitGroup
	for index := 0; index < routeProbeWorkers; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					if job.model {
						s.probeIsolatedModel(ctx, job.id)
					} else {
						s.probePausedRoute(ctx, job.id)
					}
				}
			}
		}()
	}
	defer workers.Wait()
	defer close(jobs)
	ticker := time.NewTicker(routeProbeTick)
	defer ticker.Stop()
	for {
		stateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		due, err := s.limiter.dueRouteProbes(stateCtx)
		modelDue, modelErr := s.limiter.dueModelProbes(stateCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("[model-proxy] list recovery probes: %v", err)
		}
		if modelErr != nil && ctx.Err() == nil {
			log.Printf("[model-proxy] list model recovery probes: %v", modelErr)
		}
		// Interleave both queues so a backlog of route failures does not starve
		// isolated models waiting for their first recovery probe.
		for i := 0; i < len(due) || i < len(modelDue); i++ {
			batch := make([]probeJob, 0, 2)
			if i < len(modelDue) {
				batch = append(batch, probeJob{id: modelDue[i], model: true})
			}
			if i < len(due) {
				batch = append(batch, probeJob{id: due[i]})
			}
			for _, job := range batch {
				select {
				case jobs <- job:
				case <-ctx.Done():
					return
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) probePausedRoute(parent context.Context, routeID string) {
	ctx, cancel := context.WithTimeout(parent, routeProbeTimeout)
	defer cancel()
	claim, err := s.limiter.claimRouteProbe(ctx, routeID)
	if err != nil || claim == nil {
		return
	}
	success := false
	defer func() {
		stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
		defer stateCancel()
		if _, err := s.limiter.finishRouteProbe(stateCtx, claim, success); err != nil {
			log.Printf("[model-proxy] finish recovery probe route_id=%s err=%v", routeID, err)
		}
	}()
	route, err := s.loadProbeRoute(ctx, claim)
	if err != nil || route == nil || !route.Enabled {
		return
	}
	lease, acquired, err := s.limiter.acquireRouteLease(ctx, *route, claim.token, route.ConcurrencyLimit, claim.token)
	if err != nil || !acquired {
		return
	}
	defer lease.Release(context.WithoutCancel(parent))
	success = s.performRouteProbe(ctx, *route, claim.spec)
}

func (s *Service) loadProbeRoute(ctx context.Context, claim *routeProbeClaim) (*Route, error) {
	if claim.spec.GroupID == "" {
		return s.repository.GetRoute(ctx, claim.routeID)
	}
	group, err := s.repository.GetSmartGroup(ctx, claim.spec.GroupID)
	if err != nil || group == nil || !group.Enabled {
		return nil, err
	}
	// Reload the exact membership, including its current prepared key and mapping.
	routes, err := s.repository.ListGroupRoutes(ctx, claim.spec.GroupID, true, claim.spec.ModelID)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.ID == claim.routeID {
			return &route, nil
		}
	}
	return nil, nil
}

func newRouteProbeRequest(ctx context.Context, spec routeProbeSpec) (*http.Request, []byte, error) {
	path := spec.Path
	method := http.MethodPost
	var payload map[string]any
	switch {
	case path == "/v1/models" || path == "/v1beta/models" || (strings.HasPrefix(path, "/v1beta/models/") && !strings.Contains(path, ":")):
		method = http.MethodGet
	case path == "/v1/embeddings":
		payload = map[string]any{"model": spec.ModelID, "input": "ping"}
	case path == "/v1/chat/completions" || path == "/v1/messages":
		payload = map[string]any{"model": spec.ModelID, "messages": []map[string]string{{"role": "user", "content": "Reply OK."}}, "max_tokens": 16, "stream": false}
	case path == "/v1/responses":
		payload = map[string]any{"model": spec.ModelID, "input": "Reply OK.", "max_output_tokens": 16, "stream": false}
	case path == "/v1/responses/compact":
		payload = map[string]any{"model": spec.ModelID, "input": []map[string]string{{"role": "user", "content": "Reply OK."}}}
	case strings.HasPrefix(path, "/v1beta/models/"):
		if _, _, ok := nativeGeminiModelAction(path); !ok {
			return nil, nil, errors.New("unsupported recovery probe endpoint")
		}
		path = strings.Replace(path, ":streamGenerateContent", ":generateContent", 1)
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []map[string]string{{"text": "Reply OK."}}}}, "generationConfig": map[string]int{"maxOutputTokens": 16}}
	default:
		return nil, nil, errors.New("unsupported recovery probe endpoint")
	}
	if method == http.MethodPost && strings.TrimSpace(spec.ModelID) == "" {
		return nil, nil, errors.New("recovery probe model is required")
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://recovery.local"+path, bytes.NewReader(body))
	if err == nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, body, err
}

func (s *Service) performRouteProbe(ctx context.Context, route Route, spec routeProbeSpec) bool {
	request, body, streaming, err := newStreamingRouteProbeRequest(ctx, spec)
	if err != nil {
		return false
	}
	// Catalog, embedding and compact endpoints have no streaming protocol.
	if !streaming {
		return s.executeRouteProbe(ctx, route, spec).Success
	}
	response, _, err := s.performAttemptWithResponseHeaderTimeout(request, route,
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
		route.ModelMapping, mappedResponseContext{}, false, smartGroupFirstResponseHeaderTimeout)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || !isEventStream(response.Header) {
		return false
	}
	return validateRouteProbeStream(response.Body, spec.Path)
}

func (s *Service) executeRouteProbe(ctx context.Context, route Route, spec routeProbeSpec) (result RouteTestResult) {
	started := time.Now()
	result.Message = "admin.modelProxy.manualTest.failed"
	defer func() { result.LatencyMS = time.Since(started).Milliseconds() }()
	request, body, err := newRouteProbeRequest(ctx, spec)
	if err != nil {
		return result
	}
	response, _, err := s.performAttemptWithResponseHeaderTimeout(request, route,
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
		route.ModelMapping, mappedResponseContext{}, false, smartGroupFirstResponseHeaderTimeout)
	if response != nil {
		defer response.Body.Close()
		result.StatusCode = response.StatusCode
	}
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return result
	}
	// A models list alone is insufficient to restore an inference route. Validate
	// the actual minimal request and its body, including errors hidden behind 200.
	encoded, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(encoded) > 1<<20 {
		return result
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return result
	}
	if len(payload) == 0 {
		return result
	}
	if raw, ok := payload["error"]; ok && string(raw) != "null" {
		return result
	}
	expected := "output"
	switch {
	case spec.Path == "/v1/models" || spec.Path == "/v1/embeddings":
		expected = "data"
	case spec.Path == "/v1/chat/completions":
		expected = "choices"
	case spec.Path == "/v1/messages":
		expected = "content"
	case spec.Path == "/v1beta/models":
		expected = "models"
	case strings.HasPrefix(spec.Path, "/v1beta/models/"):
		if strings.Contains(spec.Path, ":") {
			expected = "candidates"
		} else {
			expected = "name"
		}
	}
	raw, ok := payload[expected]
	result.Success = ok && len(raw) > 0 && string(raw) != "null"
	if result.Success {
		result.Message = "admin.modelProxy.manualTest.succeeded"
	}
	return result
}
