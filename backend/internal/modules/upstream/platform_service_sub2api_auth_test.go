package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestLoginWithTokenPrefersValidAccessToken(t *testing.T) {
	var refreshCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			writeJSON(w, map[string]any{"data": map[string]any{
				"access_token":  "unexpected-refreshed-token",
				"refresh_token": "unexpected-refresh-token",
				"expires_in":    3600,
			}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("expected the supplied access token, got %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/auth/me":
			writeJSON(w, map[string]any{"data": map[string]any{"balance": 1.25, "total_recharged": 5.0}})
		case "/api/v1/usage/dashboard/stats":
			writeJSON(w, map[string]any{"data": map[string]any{"today_actual_cost": 0.25}})
		case "/api/v1/groups/available":
			writeJSON(w, map[string]any{"data": []any{}})
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	result, err := service.LoginWithToken(server.URL, PlatformSub2API, "", "access-token", "stale-refresh-token", "Bearer")
	if err != nil {
		t.Fatalf("expected valid access token login, got %v", err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("refresh endpoint called %d times, want 0", got)
	}
	if result.Metrics.Balance.Value == nil || *result.Metrics.Balance.Value != 1.25 {
		t.Fatalf("unexpected balance: %+v", result.Metrics.Balance)
	}
}

func TestLoginWithTokenRefreshesAfterAccessTokenUnauthorized(t *testing.T) {
	var refreshCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			atomic.AddInt32(&refreshCalls, 1)
			writeJSON(w, map[string]any{"data": map[string]any{
				"access_token":  "refreshed-access-token",
				"refresh_token": "rotated-refresh-token",
				"expires_in":    3600,
			}})
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") == "Bearer access-token" {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, map[string]any{"message": "expired"})
				return
			}
			writeJSON(w, map[string]any{"data": map[string]any{"balance": 2.5}})
		case "/api/v1/usage/dashboard/stats":
			writeJSON(w, map[string]any{"data": map[string]any{"today_actual_cost": 0.5}})
		case "/api/v1/groups/available":
			writeJSON(w, map[string]any{"data": []any{}})
		case "/api/v1/groups/rates":
			writeJSON(w, map[string]any{"data": map[string]any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	result, err := service.LoginWithToken(server.URL, PlatformSub2API, "", "access-token", "refresh-token", "Bearer")
	if err != nil {
		t.Fatalf("expected refresh fallback to succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", got)
	}
	if result.Session.AccessToken != "refreshed-access-token" || result.Session.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("unexpected refreshed session: %+v", result.Session)
	}
}

func TestSub2APIAccessTokenExpiry(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.signature"
	expiresAt := sub2APIAccessTokenExpiry(token)
	if expiresAt == nil || *expiresAt != 1700000000000 {
		t.Fatalf("unexpected JWT expiry: %v", expiresAt)
	}
	if sub2APIAccessTokenExpiry("opaque-token") != nil {
		t.Fatal("opaque token must not produce a JWT expiry")
	}
}
