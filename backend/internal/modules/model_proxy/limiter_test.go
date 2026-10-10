package model_proxy

import "testing"

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
