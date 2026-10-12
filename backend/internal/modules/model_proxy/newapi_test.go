package model_proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

type proxyTestSites struct {
	site      upstream.Site
	responses []upstream.Response
}

func (s proxyTestSites) GetSite(context.Context, string) (*upstream.Site, error) { return &s.site, nil }
func (s proxyTestSites) FreshSiteSession(context.Context, string) (upstream.Session, error) {
	return *s.site.Session, nil
}
func (s proxyTestSites) ForceRefreshSiteSession(context.Context, string) (upstream.Session, error) {
	return *s.site.Session, nil
}
func (s proxyTestSites) List(context.Context, string) []upstream.Response { return s.responses }

type proxyTestAccount struct{}

func (proxyTestAccount) RequireCurrentID(context.Context, string) (string, error) {
	return "workspace", nil
}

func TestProxyListsNewAPISitesAndNativeGroups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self/groups":
			_, _ = w.Write([]byte(`{"success":true,"data":{"01-按量":{"ratio":1}}}`))
		case "/api/pricing":
			_, _ = w.Write([]byte(`{"success":true,"data":[],"group_ratio":{"01-按量":1}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	}))
	defer server.Close()
	session := upstream.Session{Platform: upstream.PlatformNewAPI, BaseURL: server.URL, UserID: "42", Cookie: "session=test"}
	sites := proxyTestSites{
		site: upstream.Site{UserID: "user", AdminAccountID: "workspace", Platform: upstream.PlatformNewAPI, Session: &session},
		responses: []upstream.Response{
			{ID: "new", Platform: upstream.PlatformNewAPI, Status: upstream.StatusConnected},
			{ID: "sub", Platform: upstream.PlatformSub2API, Status: upstream.StatusConnected},
			{ID: "failed", Platform: upstream.PlatformNewAPI, Status: upstream.StatusError},
			{ID: "unsupported", Platform: upstream.PlatformAuto, Status: upstream.StatusConnected},
		},
	}
	service := &Service{sites: sites, accounts: proxyTestAccount{}, platform: upstream.NewPlatformService(upstream.NewHTTPClient(server.Client()))}
	if found := service.ListSites(context.Background(), "user"); len(found) != 2 || found[0].ID != "new" {
		t.Fatalf("New-API missing from route selector: %v", found)
	}
	groups, err := service.ListGroups(context.Background(), "user", "new")
	if err != nil || len(groups) != 1 || groups[0].ID != "01-按量" {
		t.Fatalf("New-API groups: %v err=%v", groups, err)
	}
	if _, err := service.ListGroups(context.Background(), "other-user", "new"); err == nil {
		t.Fatal("another user must not access site groups")
	}
}

func TestProxyCleanupDeletesNewAPIToken(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/token/7" || r.Header.Get("Cookie") != "session=test" {
			t.Errorf("wrong cleanup request: %s %s", r.Method, r.URL)
		}
		deleted = true
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	session := upstream.Session{Platform: upstream.PlatformNewAPI, BaseURL: server.URL, Cookie: "session=test", UserID: "42"}
	service := &Service{sites: proxyTestSites{site: upstream.Site{Session: &session}}, platform: upstream.NewPlatformService(upstream.NewHTTPClient(server.Client()))}
	if err := service.deleteCleanupJob(context.Background(), CleanupJob{SiteID: "new", RemoteKeyID: "7"}); err != nil || !deleted {
		t.Fatalf("New-API cleanup failed: deleted=%v err=%v", deleted, err)
	}
}

func TestTemporaryKeyNamesFitNewAPILimit(t *testing.T) {
	jobID, err := randomID("pcleanup_")
	if err != nil {
		t.Fatal(err)
	}
	name := temporaryKeyName(jobID)
	if len(name) > 50 || !strings.HasSuffix(name, strings.TrimPrefix(jobID, "pcleanup_")) {
		t.Fatalf("temporary key name exceeds upstream limit or loses uniqueness: %q", name)
	}
}
