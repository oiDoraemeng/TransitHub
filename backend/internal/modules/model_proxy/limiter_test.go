package model_proxy

import (
	"sort"
	"testing"
)

func TestRankAcquireCandidatesUsesSoftTiersAndLatency(t *testing.T) {
	routes := []availableRoute{
		availableTestRoute("temp-unknown", 2, false, 0, 50, 0, false),
		availableTestRoute("temp-fast", 2, false, 4, 50, 20, true),
		availableTestRoute("temp-slow", 2, false, 0, 50, 100, true),
		availableTestRoute("provided-fast", 2, true, 9, 50, 30, true),
		availableTestRoute("provided-slow", 2, true, 0, 50, 90, true),
	}

	candidates := rankAcquireCandidates(routes, 1)
	assertCandidate(t, candidates[0], "temp-unknown", temporaryKeySoftConcurrency)
	assertCandidate(t, candidates[1], "temp-fast", temporaryKeySoftConcurrency)
	assertCandidate(t, candidates[2], "temp-slow", temporaryKeySoftConcurrency)
	assertCandidate(t, candidates[3], "provided-fast", providedKeySoftConcurrency)
	assertCandidate(t, candidates[4], "provided-slow", providedKeySoftConcurrency)
}

func TestRankAcquireCandidatesReturnsToTemporaryKeysAfterSoftTiersFill(t *testing.T) {
	routes := []availableRoute{
		availableTestRoute("temporary", 2, false, 5, 50, 100, true),
		availableTestRoute("provided", 2, true, 10, 50, 20, true),
	}

	candidates := rankAcquireCandidates(routes, 1)
	assertCandidate(t, candidates[0], "temporary", 50)
	assertCandidate(t, candidates[1], "provided", 50)
}

func TestRankAcquireCandidatesKeepsPriorityAsHardBoundary(t *testing.T) {
	routes := []availableRoute{
		availableTestRoute("high-priority-overflow", 2, true, 10, 50, 100, true),
		availableTestRoute("low-priority-temporary", 1, false, 0, 50, 10, true),
	}

	candidates := rankAcquireCandidates(routes, 1)
	assertCandidate(t, candidates[0], "high-priority-overflow", 50)
	assertCandidate(t, candidates[1], "low-priority-temporary", temporaryKeySoftConcurrency)
}

func TestRankAcquireCandidatesHonorsRouteLimitBelowSoftLimit(t *testing.T) {
	routes := []availableRoute{
		availableTestRoute("small-temporary", 2, false, 0, 3, 10, true),
		availableTestRoute("small-provided", 2, true, 0, 7, 10, true),
	}

	candidates := rankAcquireCandidates(routes, 1)
	assertCandidate(t, candidates[0], "small-temporary", 3)
	assertCandidate(t, candidates[1], "small-provided", 7)
}

func TestRotateLeadingCapacityTieUsesCursor(t *testing.T) {
	routes := []availableRoute{
		availableTestRoute("a", 2, false, 5, 50, 0, false),
		availableTestRoute("b", 2, false, 5, 50, 0, false),
		availableTestRoute("c", 2, false, 10, 50, 0, false),
	}
	sortAvailableRoutes(routes)
	rotateLeadingCapacityTie(routes, 2)
	if routes[0].route.ID != "b" {
		t.Fatalf("first route=%q want b", routes[0].route.ID)
	}
}

func availableTestRoute(id string, priority int, provided bool, active int64, limit int, latencyMS int64, latencyKnown bool) availableRoute {
	return availableRoute{
		route: Route{
			ID: id, Priority: priority, UseProvidedKey: provided, ConcurrencyLimit: limit,
		},
		active: active, available: int64(limit) - active, latencyMS: latencyMS, latencyKnown: latencyKnown,
	}
}

func assertCandidate(t *testing.T, candidate acquireCandidate, routeID string, limit int) {
	t.Helper()
	if candidate.route.ID != routeID || candidate.limit != limit {
		t.Fatalf("candidate=(%s,%d) want (%s,%d)", candidate.route.ID, candidate.limit, routeID, limit)
	}
}

// Pure reference ordering for the scheduler policy. Redis tests exercise the
// production atomic scheduler under real contention.
type availableRoute struct {
	route        Route
	active       int64
	available    int64
	latencyMS    int64
	latencyKnown bool
}

type acquireCandidate struct {
	availableRoute
	limit int
}

func sortAvailableRoutes(routes []availableRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].route.Priority == routes[j].route.Priority {
			if routes[i].available == routes[j].available {
				return routes[i].route.ID < routes[j].route.ID
			}
			return routes[i].available > routes[j].available
		}
		return routes[i].route.Priority > routes[j].route.Priority
	})
}

func rankAcquireCandidates(routes []availableRoute, cursor int64) []acquireCandidate {
	priorities := make([]int, 0, len(routes))
	seen := make(map[int]struct{}, len(routes))
	for _, route := range routes {
		if route.available <= 0 {
			continue
		}
		if _, ok := seen[route.route.Priority]; !ok {
			seen[route.route.Priority] = struct{}{}
			priorities = append(priorities, route.route.Priority)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	result := make([]acquireCandidate, 0, len(routes)*2)
	for _, priority := range priorities {
		priorityRoutes := make([]availableRoute, 0, len(routes))
		for _, route := range routes {
			if route.route.Priority == priority && route.available > 0 {
				priorityRoutes = append(priorityRoutes, route)
			}
		}
		result = appendSoftTier(result, priorityRoutes, false, temporaryKeySoftConcurrency)
		result = appendSoftTier(result, priorityRoutes, true, providedKeySoftConcurrency)
		sortAvailableRoutes(priorityRoutes)
		rotateLeadingCapacityTie(priorityRoutes, cursor)
		for _, route := range priorityRoutes {
			result = append(result, acquireCandidate{availableRoute: route, limit: route.route.ConcurrencyLimit})
		}
	}
	return result
}

func rotateLeadingCapacityTie(routes []availableRoute, cursor int64) {
	if len(routes) < 2 || routes[0].available <= 0 {
		return
	}
	tied := 1
	for tied < len(routes) && routes[tied].available == routes[0].available {
		tied++
	}
	if tied > 1 {
		selected := int((cursor - 1) % int64(tied))
		if selected < 0 {
			selected = 0
		}
		routes[0], routes[selected] = routes[selected], routes[0]
	}
}

func appendSoftTier(result []acquireCandidate, routes []availableRoute, providedKey bool, softLimit int) []acquireCandidate {
	tier := make([]availableRoute, 0, len(routes))
	for _, route := range routes {
		limit := softLimit
		if route.route.ConcurrencyLimit < limit {
			limit = route.route.ConcurrencyLimit
		}
		if route.route.UseProvidedKey == providedKey && route.active < int64(limit) {
			tier = append(tier, route)
		}
	}
	sort.SliceStable(tier, func(i, j int) bool {
		if tier[i].latencyKnown != tier[j].latencyKnown {
			return !tier[i].latencyKnown
		}
		if tier[i].latencyKnown && tier[i].latencyMS != tier[j].latencyMS {
			return tier[i].latencyMS < tier[j].latencyMS
		}
		if tier[i].active != tier[j].active {
			return tier[i].active < tier[j].active
		}
		return tier[i].route.ID < tier[j].route.ID
	})
	for _, route := range tier {
		limit := softLimit
		if route.route.ConcurrencyLimit < limit {
			limit = route.route.ConcurrencyLimit
		}
		result = append(result, acquireCandidate{availableRoute: route, limit: limit})
	}
	return result
}
