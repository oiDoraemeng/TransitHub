package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForcedRefreshSharesRotationAcrossEgressClients(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		writeJSON(w, map[string]any{"data": map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
		}})
	}))
	defer server.Close()
	service := NewPlatformService(NewHTTPClient(server.Client()))
	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	stale := Session{Platform: PlatformSub2API, BaseURL: server.URL,
		AccessToken: "rejected-access", RefreshToken: "old-refresh", ExpiresAt: &expiresAt}
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			client := service.WithHTTPClient(NewHTTPClient(server.Client()))
			fresh, err := client.ForceRefreshSession(stale)
			if err != nil || fresh.AccessToken != "new-access" || fresh.RefreshToken != "new-refresh" {
				t.Errorf("forced refresh = %+v, err=%v", fresh, err)
			}
		}()
	}
	workers.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("same refresh token exchanged %d times, want 1", got)
	}
	if _, err := service.ForceRefreshSession(stale); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("stale session exchanged again after proxy client closed: %d", got)
	}
}
