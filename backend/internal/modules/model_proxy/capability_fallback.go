package model_proxy

import (
	"context"
	"encoding/json"
	"log"
	"strings"
)

func mappedFallbackOnly(route Route, facts requestCapabilities, model string) bool {
	if route.RequestMatchMode != RequestMatchModeMatchOrFallback {
		return false
	}
	effective, simulated := effectiveRequestCapabilities(route, facts, model)
	return !simulated && !effective.matches(route.RequestMatchCapabilities)
}

// With no keyword match, a failed ordinary member yields to an explicitly
// configured mapped backup before any other native backup. Each pool keeps the
// atomic scheduler's priority, capacity and health checks. Before a failure,
// fallback-only members cannot displace an eligible ordinary member of equal or
// higher priority. Capability-matched mapped members remain primary candidates.
func (l *Limiter) acquireWithMappedFallback(ctx context.Context, routes []Route, requestID string, spec routeProbeSpec, facts requestCapabilities, keywordMatched, afterFailure bool, configured ...bool) (*Lease, *Route, string, error) {
	fallbackConfigured := len(configured) > 0 && configured[0]
	for _, route := range routes {
		fallbackConfigured = fallbackConfigured || route.RequestMatchMode == RequestMatchModeMatchOrFallback
	}
	if !fallbackConfigured {
		lease, route, err := l.AcquireBest(ctx, routes, requestID, spec)
		return lease, route, "priority", err
	}
	if spec.Path == compactPath {
		lease, route, err := l.AcquireBest(ctx, routes, requestID, spec)
		return lease, route, "priority", err
	}
	if keywordMatched {
		if afterFailure {
			native := make([]Route, 0, len(routes))
			for _, route := range routes {
				if route.ModelMappingEnabled && strings.HasPrefix(routeTargetModel(route, spec.ModelID), "gpt-") {
					continue
				}
				native = append(native, route)
			}
			lease, route, err := l.AcquireBest(ctx, native, requestID, spec)
			return lease, route, "keyword_native_backup", err
		}
		lease, route, err := l.AcquireBest(ctx, routes, requestID, spec)
		return lease, route, "priority", err
	}
	var mapped, primary, backups []Route
	highestFallback := -1
	for _, route := range routes {
		if route.RequestMatchMode == RequestMatchModeMatchOrFallback && requestPolicyAllows(route, facts, spec.ModelID) {
			mapped = append(mapped, route)
			if route.Priority > highestFallback {
				highestFallback = route.Priority
			}
		}
	}
	if len(mapped) == 0 {
		lease, route, err := l.AcquireBest(ctx, routes, requestID, spec)
		return lease, route, "priority", err
	}
	for _, route := range routes {
		if route.RequestMatchMode == RequestMatchModeMatchOrFallback {
			if !afterFailure && !mappedFallbackOnly(route, facts, spec.ModelID) {
				primary = append(primary, route)
			}
			continue
		}
		if !afterFailure && route.Priority >= highestFallback {
			primary = append(primary, route)
		} else {
			backups = append(backups, route)
		}
	}
	pools := []struct {
		routes []Route
		stage  string
	}{{primary, "primary"}, {mapped, "mapped_fallback"}, {backups, "native_backup"}}
	var lastState error
	for _, pool := range pools {
		if len(pool.routes) == 0 {
			continue
		}
		lease, route, err := l.AcquireBest(ctx, pool.routes, requestID, spec)
		if err == errRoutePaused || err == errModelPaused {
			lastState = err
			continue
		}
		if err != nil || route != nil {
			return lease, route, pool.stage, err
		}
	}
	return nil, nil, "unavailable", lastState
}

type memberSelectionDecision struct {
	RouteID string `json:"route_id"`
	Reason  string `json:"reason"`
}

// Log only structural facts and indexes into the configured keyword list.
// Neither input text, key material, thinking text nor signatures enter this log.
func logMemberSelection(requestID, groupID, model, path string, facts requestCapabilities, keywordIndexes []int, decisions []memberSelectionDecision) {
	capabilities := []string{}
	for _, capability := range []string{"pdf", "code_execution", "json_schema", "web_search", "native_claude"} {
		if facts.has(capability) {
			capabilities = append(capabilities, capability)
		}
	}
	data, _ := json.Marshal(struct {
		RequestID      string                    `json:"request_id"`
		GroupID        string                    `json:"group_id"`
		Model          string                    `json:"model"`
		Path           string                    `json:"path"`
		Capabilities   []string                  `json:"capabilities"`
		Thinking       bool                      `json:"thinking"`
		NativeState    bool                      `json:"native_state"`
		KeywordIndexes []int                     `json:"keyword_indexes"`
		Members        []memberSelectionDecision `json:"members"`
	}{requestID, groupID, model, path, capabilities, facts.Thinking, facts.NativeState, keywordIndexes, decisions})
	log.Printf("[model-proxy] selection %s", data)
}

func appendSelectionExclusions(decisions []memberSelectionDecision, before, after []Route, reason func(Route) string) []memberSelectionDecision {
	kept := make(map[string]bool, len(after))
	for _, route := range after {
		kept[route.ID] = true
	}
	for _, route := range before {
		if !kept[route.ID] {
			decisions = append(decisions, memberSelectionDecision{route.ID, reason(route)})
		}
	}
	return decisions
}

func requestExclusionReason(route Route, facts requestCapabilities, model string) string {
	if facts.Thinking && !route.thinkingAllowed() {
		return "thinking_disabled"
	}
	effective, _ := effectiveRequestCapabilities(route, facts, model)
	if effective.NativeClaude && strings.HasPrefix(routeTargetModel(route, model), "gpt-") {
		return "native_state_requires_native"
	}
	if route.RequestMatchMode == KeywordMatchModeSkipOnMatch {
		return "capability_skip_on_match"
	}
	if effective.present() && !effective.coveredBy(route.RequestMatchCapabilities) {
		return "capabilities_not_covered"
	}
	return "capability_required"
}
