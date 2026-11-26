package model_proxy

import (
	"context"
	"net/http"
)

type usageCorrectionContextKey struct{}
type inputUsagePolicy struct {
	Estimate, Limit  int64
	AllowancePercent int
}

func usageCorrectionAllowance(value *int) (int, error) {
	if value == nil {
		return 10, nil
	}
	if *value < 0 || *value > 1000 {
		return 0, &requestError{Status: 400, Message: "usageCorrectionAllowancePercent must be between 0 and 1000"}
	}
	return *value, nil
}

// The group config is loaded afresh for each public request. A saved change
// applies to the next request and never affects other groups or direct routes.
func (s *Service) withUsageCorrection(r *http.Request, replay *replayBody, group SmartGroup) *http.Request {
	if !group.UsageCorrectionEnabled || r.Method != http.MethodPost {
		return r
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses":
	default:
		return r
	}
	percent := group.UsageCorrectionAllowancePercent
	if percent < 0 || percent > 1000 {
		return r
	}
	input, err := replay.Object()
	if err != nil {
		return r
	}
	estimate, known := estimateCorrectionInput(input, replay)
	if !known || estimate <= 0 {
		return r
	}
	policy := inputUsagePolicy{Estimate: estimate, Limit: (estimate*int64(100+percent) + 99) / 100, AllowancePercent: percent}
	return r.WithContext(context.WithValue(r.Context(), usageCorrectionContextKey{}, policy))
}
