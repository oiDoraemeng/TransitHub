package model_proxy

import (
	"context"
	"math"
	"net/http"
)

type usageCorrectionContextKey struct{}
type inputUsagePolicy struct {
	Estimate, Limit  int64
	Min, Max         int64
	AllowancePercent int
	Method, Model    string
	Features         correctionInputFeatures
}

func usageCorrectionAllowance(value *int) (int, error) {
	if value == nil {
		return 40, nil
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
		if _, _, ok := nativeGeminiModelAction(r.URL.Path); !ok {
			return r
		}
	}
	percent := group.UsageCorrectionAllowancePercent
	if percent < 0 || percent > 1000 {
		return r
	}
	input, err := replay.Object()
	if err != nil {
		return r
	}
	features, known := correctionFeatures(input)
	if !known {
		return r
	}
	model, _ := input["model"].(string)
	if pathModel, _, ok := nativeGeminiModelAction(r.URL.Path); ok {
		model = pathModel
	}
	// Tokenize only after a member is selected, so a mapped public alias does
	// not trigger an unnecessary count with the wrong tokenizer.
	policy := inputUsagePolicy{AllowancePercent: percent, Features: features, Model: model}
	return r.WithContext(context.WithValue(r.Context(), usageCorrectionContextKey{}, policy))
}

func (p inputUsagePolicy) forModel(model string) inputUsagePolicy {
	range_ := p.Features.tokenRange(model)
	p.Min, p.Max, p.Estimate = range_.Min, range_.Max, range_.Max
	p.Method, p.Model = range_.Method, model
	p.Limit = correctionRangeLimit(p.Max, p.AllowancePercent)
	return p
}

// Ceil(maximum * (1 + percent/100)), saturating instead of wrapping on overflow.
func correctionRangeLimit(maximum int64, percent int) int64 {
	if maximum <= 0 || percent < 0 || percent > 1000 {
		return 0
	}
	factor := int64(100 + percent)
	whole, remainder := maximum/100, maximum%100
	extra := (remainder*factor + 99) / 100
	if whole > (math.MaxInt64-extra)/factor {
		return math.MaxInt64
	}
	return whole*factor + extra
}
