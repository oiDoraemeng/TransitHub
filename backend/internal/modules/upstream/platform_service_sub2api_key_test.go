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
