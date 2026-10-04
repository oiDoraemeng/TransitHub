package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateSub2APIKeySetsQuota(t *testing.T) {
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
		writeJSON(w, map[string]any{"data": map[string]any{"id": 11, "key": "sk-created"}})
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
	if id != "11" || key != "sk-created" {
		t.Fatalf("unexpected create result: id=%q key=%q", id, key)
	}
	if requestBody["name"] != "Transit-vip" || requestBody["group_id"] != float64(7) {
		t.Fatalf("unexpected key identity fields: %#v", requestBody)
	}
	if requestBody["quota"] != float64(100) {
		t.Fatalf("quota = %#v, want 100", requestBody["quota"])
	}
}
