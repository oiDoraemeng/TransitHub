package model_proxy

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"
	"transithub/backend/internal/shared/httpjson"
)

type UsageCorrectionSettings struct {
	Enabled          bool `json:"enabled"`
	AllowancePercent int  `json:"allowancePercent"`
}

type usageCorrectionStore interface {
	GetUsageCorrection(context.Context, string, string) (UsageCorrectionSettings, error)
	SaveUsageCorrection(context.Context, string, string, UsageCorrectionSettings) error
}

func (r *Repository) GetUsageCorrection(ctx context.Context, userID, accountID string) (UsageCorrectionSettings, error) {
	result := UsageCorrectionSettings{AllowancePercent: 10}
	err := r.db.QueryRow(ctx, `SELECT enabled,allowance_percent FROM proxy_usage_correction_settings WHERE user_id=$1 AND admin_account_id=$2`, userID, accountID).Scan(&result.Enabled, &result.AllowancePercent)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return result, err
}

func (r *Repository) SaveUsageCorrection(ctx context.Context, userID, accountID string, value UsageCorrectionSettings) error {
	_, err := r.db.Exec(ctx, `INSERT INTO proxy_usage_correction_settings(user_id,admin_account_id,enabled,allowance_percent) VALUES($1,$2,$3,$4)
        ON CONFLICT(user_id,admin_account_id) DO UPDATE SET enabled=EXCLUDED.enabled,allowance_percent=EXCLUDED.allowance_percent,updated_at=now()`, userID, accountID, value.Enabled, value.AllowancePercent)
	return err
}

func (s *Service) correctionStore() usageCorrectionStore {
	if s.usageCorrectionStore != nil {
		return s.usageCorrectionStore
	}
	if s.repository != nil {
		return s.repository
	}
	return nil
}

func (h *Handler) usageCorrectionSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	accountID, err := h.service.currentWorkspace(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	store := h.service.correctionStore()
	if r.Method == http.MethodGet {
		settings, err := store.GetUsageCorrection(r.Context(), userID, accountID)
		if err != nil {
			h.writeError(w, err)
			return
		}
		httpjson.Write(w, http.StatusOK, settings)
		return
	}
	// Require both fields: missing allowance must not silently become strict 0%.
	var input struct {
		Enabled   *bool `json:"enabled"`
		Allowance *int  `json:"allowancePercent"`
	}
	if err := httpjson.Decode(r, &input); err != nil || input.Enabled == nil || input.Allowance == nil || *input.Allowance < 0 || *input.Allowance > 1000 {
		httpjson.WriteError(w, http.StatusBadRequest, "enabled and allowancePercent (0–1000) are required")
		return
	}
	settings := UsageCorrectionSettings{Enabled: *input.Enabled, AllowancePercent: *input.Allowance}
	if err := store.SaveUsageCorrection(r.Context(), userID, accountID, settings); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, settings)
}

type usageCorrectionContextKey struct{}
type inputUsagePolicy struct {
	Estimate, Limit  int64
	AllowancePercent int
}

// Read once per incoming request, without caching: a saved switch/percentage
// applies to the next request, even when several instances share the database.
func (s *Service) withUsageCorrection(r *http.Request, replay *replayBody, userID, accountID string) *http.Request {
	store := s.correctionStore()
	if store == nil || r.Method != http.MethodPost {
		return r
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses":
	default:
		return r
	}
	settings, err := store.GetUsageCorrection(r.Context(), userID, accountID)
	if err != nil {
		log.Printf("[usage-correction] settings unavailable; preserving usage workspace=%s", accountID)
		return r
	}
	if !settings.Enabled || settings.AllowancePercent < 0 || settings.AllowancePercent > 1000 {
		return r
	}
	input, err := replay.Object()
	if err != nil {
		return r
	}
	estimate, known := estimateCorrectionInput(input)
	if !known || estimate <= 0 {
		return r
	}
	policy := inputUsagePolicy{Estimate: estimate, Limit: (estimate*int64(100+settings.AllowancePercent) + 99) / 100, AllowancePercent: settings.AllowancePercent}
	return r.WithContext(context.WithValue(r.Context(), usageCorrectionContextKey{}, policy))
}
