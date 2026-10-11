package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginNewAPIUsesBearerTokenAndNestedUserID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/user/login" {
			if r.Method != http.MethodPost {
				t.Fatalf("login method = %s, want POST", r.Method)
			}
			writeJSON(w, map[string]any{
				"success": true,
				"data": map[string]any{
					"access_token": "dashboard-token",
					"token_type":   "Bearer",
					"user": map[string]any{
						"id":         "3381",
						"quota":      1_000_000,
						"used_quota": 250_000,
						"group":      "default",
					},
				},
			})
			return
		}

		if got := r.Header.Get("Authorization"); got != "Bearer dashboard-token" {
			t.Fatalf("Authorization for %s = %q", r.URL.Path, got)
		}
		if got := r.Header.Get("New-Api-User"); got != "3381" {
			t.Fatalf("New-Api-User for %s = %q", r.URL.Path, got)
		}
		switch r.URL.Path {
		case "/api/status":
			writeJSON(w, map[string]any{"data": map[string]any{"quota_per_unit": 500_000}})
		case "/api/user/self":
			writeJSON(w, map[string]any{"data": map[string]any{
				"id": 3381, "quota": 1_000_000, "used_quota": 250_000, "group": "default",
			}})
		case "/api/log/self/stat":
			writeJSON(w, map[string]any{"data": map[string]any{"quota": 50_000}})
		case "/api/user/self/groups":
			writeJSON(w, map[string]any{"data": map[string]any{"default": 1.0}})
		case "/api/pricing":
			writeJSON(w, map[string]any{"data": []any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	result, err := service.Login(server.URL, PlatformNewAPI, "user@example.com", "password")
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if !result.Session.IsAuthenticated() {
		t.Fatalf("session should be authenticated: %+v", result.Session)
	}
	if result.Session.UserID != "3381" || result.Session.AccessToken != "dashboard-token" || result.Session.TokenType != "Bearer" {
		t.Fatalf("unexpected bearer session: %+v", result.Session)
	}
	if result.Session.Cookie != "" {
		t.Fatalf("token login should not require a cookie, got %q", result.Session.Cookie)
	}
	if result.Metrics.Balance.Value == nil || *result.Metrics.Balance.Value != 2 {
		t.Fatalf("unexpected balance: %+v", result.Metrics.Balance)
	}
}

func TestLoginNewAPIKeepsLegacyCookieSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/user/login" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "legacy-cookie", Path: "/"})
			writeJSON(w, map[string]any{"success": true, "data": map[string]any{
				"id": 42, "quota": 500_000, "used_quota": 0, "group": "default",
			}})
			return
		}

		if got := r.Header.Get("Cookie"); got != "session=legacy-cookie" {
			t.Fatalf("Cookie for %s = %q", r.URL.Path, got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("legacy cookie request must not send Authorization, got %q", got)
		}
		if got := r.Header.Get("New-Api-User"); got != "42" {
			t.Fatalf("New-Api-User for %s = %q", r.URL.Path, got)
		}
		switch r.URL.Path {
		case "/api/status":
			writeJSON(w, map[string]any{"data": map[string]any{"quota_per_unit": 500_000}})
		case "/api/user/self":
			writeJSON(w, map[string]any{"data": map[string]any{
				"id": 42, "quota": 500_000, "used_quota": 0, "group": "default",
			}})
		case "/api/log/self/stat":
			writeJSON(w, map[string]any{"data": map[string]any{"quota": 0}})
		case "/api/user/self/groups":
			writeJSON(w, map[string]any{"data": map[string]any{"default": 1.0}})
		case "/api/pricing":
			writeJSON(w, map[string]any{"data": []any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	result, err := service.Login(server.URL, PlatformNewAPI, "legacy", "password")
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if !result.Session.IsAuthenticated() || result.Session.Cookie != "session=legacy-cookie" || result.Session.UserID != "42" {
		t.Fatalf("unexpected cookie session: %+v", result.Session)
	}
	if result.Session.AccessToken != "" {
		t.Fatalf("legacy cookie session should not contain access token")
	}
}

func TestNewAPIUserIDSupportsNestedNumericAndStringIDs(t *testing.T) {
	for name, input := range map[string]string{
		"top-level string": `{"id":"7"}`,
		"nested number":    `{"user":{"id":3381}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal([]byte(input), &data); err != nil {
				t.Fatal(err)
			}
			if got := newAPIUserID(data); got == "" {
				t.Fatalf("newAPIUserID(%s) returned empty", input)
			}
		})
	}
}
