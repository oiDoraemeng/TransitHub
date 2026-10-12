package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateSub2APIKeyUsesCustomKeyAndSetsQuota(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/keys" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Idempotency-Key"); got != "" {
			t.Errorf("ordinary site should not receive Idempotency-Key, got %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-**********"}})
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	id, key, err := service.CreateSub2APIKey(Session{
		Platform:    PlatformSub2API,
		BaseURL:     server.URL,
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}, "Transit-vip", 7)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if id != "11" {
		t.Fatalf("unexpected create result: id=%q key=%q", id, key)
	}
	if requestBody["name"] != "Transit-vip" || requestBody["group_id"] != float64(7) {
		t.Fatalf("unexpected key identity fields: %#v", requestBody)
	}
	if requestBody["quota"] != float64(100) {
		t.Fatalf("quota = %#v, want 100", requestBody["quota"])
	}
	customKey, ok := requestBody["custom_key"].(string)
	if !ok || len(customKey) < 16 || !strings.HasPrefix(customKey, "th_") {
		t.Fatalf("invalid custom key in request: %#v", requestBody["custom_key"])
	}
	if key != customKey {
		t.Fatalf("returned key does not match submitted custom key")
	}
}

func TestCreateSub2APIKeyRetriesWhenIdempotencyKeyIsRequired(t *testing.T) {
	requestCount := 0
	idempotencyKeys := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("first request must preserve legacy behavior, got Idempotency-Key %q", got)
			}
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{
				"code":   http.StatusBadRequest,
				"reason": "IDEMPOTENCY_KEY_REQUIRED",
			})
			return
		}
		if got := r.Header.Get("Idempotency-Key"); !strings.HasPrefix(got, "transithub-") {
			t.Fatalf("retry must include generated Idempotency-Key, got %q", got)
		} else {
			idempotencyKeys = append(idempotencyKeys, got)
		}
		var requestBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode retry request body: %v", err)
		}
		if requestBody["custom_key"] == nil || requestBody["quota"] != float64(100) {
			t.Fatalf("retry changed create payload: %#v", requestBody)
		}
		writeJSON(w, map[string]any{"data": map[string]any{"id": 12 + requestCount, "key": "sk-**********"}})
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	id, key, err := service.CreateSub2APIKey(Session{
		Platform:    PlatformSub2API,
		BaseURL:     server.URL,
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}, "Transit-idempotent", 32)
	if err != nil {
		t.Fatalf("create key with idempotency compatibility: %v", err)
	}
	if requestCount != 2 || id != "14" || !strings.HasPrefix(key, "th_") {
		t.Fatalf("unexpected retry result: requests=%d id=%q key=%q", requestCount, id, key)
	}

	secondID, _, err := service.CreateSub2APIKey(Session{
		Platform:    PlatformSub2API,
		BaseURL:     server.URL,
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}, "Transit-idempotent-second", 32)
	if err != nil {
		t.Fatalf("create second key with learned compatibility: %v", err)
	}
	if requestCount != 3 || secondID != "15" {
		t.Fatalf("learned compatibility should avoid another failed request: requests=%d id=%q", requestCount, secondID)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == idempotencyKeys[1] {
		t.Fatalf("each create must use a unique idempotency key: %#v", idempotencyKeys)
	}
}

func TestCreateSub2APIKeyPreservesLegacyFullResponseKey(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeJSON(w, map[string]any{"data": map[string]any{"id": 12, "key": "sk-legacy-created"}})
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	id, key, err := service.CreateSub2APIKey(Session{
		Platform:    PlatformSub2API,
		BaseURL:     server.URL,
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}, "Transit-legacy", 8)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if id != "12" || key != "sk-legacy-created" {
		t.Fatalf("unexpected create result: id=%q key=%q", id, key)
	}
	if requestBody["custom_key"] == key {
		t.Fatal("legacy response key should differ from submitted custom key")
	}
}

func TestCreateSub2APIKeyFallsBackWhenCustomKeyIsRejected(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var requestBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if requestCount == 1 {
			if requestBody["custom_key"] == nil {
				t.Fatal("first request must include custom_key")
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeJSON(w, map[string]any{"message": "unknown field custom_key"})
			return
		}
		if requestBody["custom_key"] != nil {
			t.Fatal("fallback request must omit custom_key")
		}
		writeJSON(w, map[string]any{"data": map[string]any{"id": 13, "key": "sk-old-server"}})
	}))
	defer server.Close()

	service := NewPlatformService(NewHTTPClient(server.Client()))
	id, key, err := service.CreateSub2APIKey(Session{
		Platform:    PlatformSub2API,
		BaseURL:     server.URL,
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}, "Transit-old", 9)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if requestCount != 2 || id != "13" || key != "sk-old-server" {
		t.Fatalf("unexpected fallback result: requests=%d id=%q key=%q", requestCount, id, key)
	}
}
