package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestNewAPIUserKeyLifecycle(t *testing.T) {
	for _, mode := range []string{"cookie", "token"} {
		for _, envelope := range []string{"paged", "legacy"} {
			t.Run(mode+"/"+envelope, func(t *testing.T) {
				created, deleted := false, false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("New-Api-User") != "42" {
						t.Error("missing New-API user header")
					}
					if mode == "cookie" && r.Header.Get("Cookie") != "session=test" {
						t.Error("missing session cookie")
					}
					if mode == "token" && r.Header.Get("Authorization") != "Bearer system-token" {
						t.Error("missing system access token")
					}
					var response any
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body["group"] != "01-按量" || body["name"] != "transithub-test" || body["unlimited_quota"] != true || body["expired_time"] != float64(-1) {
							t.Errorf("invalid token configuration: %#v", body)
						}
						created = true
						response = map[string]any{"success": true}
					case r.Method == http.MethodGet && r.URL.Path == "/api/token/":
						items := []map[string]any{}
						if created && !deleted {
							items = append(items, map[string]any{"id": 7, "name": "transithub-test", "key": "sk-****", "group": "01-按量", "status": 1})
						}
						var data any = items
						if envelope == "paged" {
							data = map[string]any{"items": items, "total": len(items), "page_size": 100}
						}
						response = map[string]any{"success": true, "data": data}
					case r.Method == http.MethodPost && r.URL.Path == "/api/token/7/key":
						response = map[string]any{"success": true, "data": map[string]any{"key": "sk-full-secret"}}
					case r.Method == http.MethodDelete && r.URL.Path == "/api/token/7":
						deleted = true
						response = map[string]any{"success": true}
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusNotFound)
					}
					_ = json.NewEncoder(w).Encode(response)
				}))
				defer server.Close()
				session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, UserID: "42"}
				if mode == "cookie" {
					session.Cookie = "session=test"
				} else {
					session.AccessToken = "system-token"
				}
				service := NewPlatformService(NewHTTPClient(server.Client()))
				id, secret, err := service.CreateUserKey(session, "transithub-test", "01-按量")
				if err != nil || id != "7" || secret != "sk-full-secret" {
					t.Fatalf("create: id=%q secret=%q err=%v", id, secret, err)
				}
				if err := service.DeleteUserKey(session, id); err != nil {
					t.Fatal(err)
				}
				key, err := service.FindUserKeyByName(session, "transithub-test")
				if err != nil || key != nil || !deleted {
					t.Fatalf("key still present after deletion: key=%v err=%v", key, err)
				}
			})
		}
	}
}

func TestNewAPIRecoveryPagesEntireBatchBeyondTenPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("p"))
		// The upstream clamps pages to two items, with a match on page 11.
		items := []map[string]any{}
		for id := (page-1)*2 + 1; id <= page*2 && id <= 22; id++ {
			items = append(items, map[string]any{"id": id, "name": fmt.Sprintf("temporary-%d", id)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items, "page_size": 2, "total": 22}})
	}))
	defer server.Close()
	service := NewPlatformService(NewHTTPClient(server.Client()))
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, Cookie: "session=test", UserID: "42"}
	keys, err := service.FindUserKeysByNames(session, []string{"temporary-1", "temporary-22", "absent"})
	if err != nil || len(keys) != 2 || keys["temporary-22"].ID != "22" || requests != 11 {
		t.Fatalf("recovery: keys=%v requests=%d err=%v", keys, requests, err)
	}
}

func TestNewAPICreationPreservesIDAndRecoversAfterRevealUnauthorized(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
			creates++
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/token/":
			_, _ = w.Write([]byte(`{"success":true,"data":[{"id":7,"name":"temporary","key":"masked***"}]}`))
		case r.URL.Path == "/api/token/7/key":
			if r.Header.Get("Cookie") == "session=old" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false}`))
			} else {
				_, _ = w.Write([]byte(`{"success":true,"data":{"key":"sk-secret"}}`))
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	service := NewPlatformService(NewHTTPClient(server.Client()))
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, UserID: "42", Cookie: "session=old"}
	id, _, err := service.CreateUserKey(session, "temporary", "default")
	if id != "7" || err == nil {
		t.Fatalf("expected known ID after failed reveal: id=%q err=%v", id, err)
	}
	session.Cookie = "session=new"
	id, key, err := service.RecoverUserKey(session, "temporary")
	if err != nil || id != "7" || key != "sk-secret" || creates != 1 {
		t.Fatalf("recovery duplicated token: id=%q key=%q creates=%d err=%v", id, key, creates, err)
	}
}

func TestNewAPIRecoveryRejectsMalformedList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"unexpected":[]}}`))
	}))
	defer server.Close()
	service := NewPlatformService(NewHTTPClient(server.Client()))
	session := Session{Platform: PlatformNewAPI, BaseURL: server.URL, UserID: "42", Cookie: "session=test"}
	if _, err := service.FindUserKeyByName(session, "temporary"); err == nil {
		t.Fatal("malformed response must not be treated as a missing key")
	}
}
