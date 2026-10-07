package model_proxy

import "time"

const (
	OwnerRoute      = "route"
	OwnerSmartGroup = "smart_group"
)

type Route struct {
	ID                string `json:"id"`
	UserID            string `json:"-"`
	AdminAccountID    string `json:"-"`
	Name              string `json:"name"`
	SiteID            string `json:"siteId"`
	SiteName          string `json:"siteName"`
	GroupID           string `json:"groupId"`
	GroupName         string `json:"groupName"`
	ConcurrencyLimit  int    `json:"concurrencyLimit"`
	ActiveConcurrency int64  `json:"activeConcurrency"`
	Enabled           bool   `json:"enabled"`
	ProxyID           string `json:"proxyId"`
	ProxyName         string `json:"proxyName"`
	KeyPreview        string `json:"keyPreview"`
	ModelCount        int    `json:"modelCount"`
	CleanupPending    int    `json:"cleanupPending"`
	// Member policies are populated only when this route is loaded through a
	// smart-group membership. They are intentionally not route-level settings.
	StreamOnly        bool       `json:"streamOnly,omitempty"`
	MinInputTokens    int        `json:"minInputTokens,omitempty"`
	RequestsPerMinute int        `json:"requestsPerMinute,omitempty"`
	ModelSyncedAt     *time.Time `json:"modelSyncedAt"`
	ModelSyncError    string     `json:"modelSyncError"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type EgressProxy struct {
	ID                string     `json:"id"`
	UserID            string     `json:"-"`
	AdminAccountID    string     `json:"-"`
	Name              string     `json:"name"`
	Protocol          string     `json:"protocol"`
	Address           string     `json:"address"`
	URLCiphertext     string     `json:"-"`
	Enabled           bool       `json:"enabled"`
	RouteCount        int        `json:"routeCount"`
	LastTestStatus    string     `json:"lastTestStatus"`
	LastTestLatencyMS *int64     `json:"lastTestLatencyMs"`
	LastTestExitIP    string     `json:"lastTestExitIp"`
	LastTestError     string     `json:"lastTestError"`
	LastTestedAt      *time.Time `json:"lastTestedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type SmartGroup struct {
	ID               string    `json:"id"`
	UserID           string    `json:"-"`
	AdminAccountID   string    `json:"-"`
	Name             string    `json:"name"`
	Enabled          bool      `json:"enabled"`
	KeyPreview       string    `json:"keyPreview"`
	TotalConcurrency int       `json:"totalConcurrency"`
	Members          []Route   `json:"members"`
	Models           []Model   `json:"models"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type Model struct {
	ID                   string         `json:"id"`
	Object               string         `json:"object,omitempty"`
	OwnedBy              string         `json:"owned_by,omitempty"`
	EffectiveConcurrency int            `json:"effectiveConcurrency,omitempty"`
	Raw                  map[string]any `json:"-"`
}

type Credential struct {
	OwnerType      string
	OwnerID        string
	UserID         string
	AdminAccountID string
}

type CleanupJob struct {
	ID             string
	UserID         string
	AdminAccountID string
	RouteID        string
	SiteID         string
	RemoteKeyID    string
	RemoteKeyName  string
	Status         string
	Attempts       int
	LastError      string
}

type CreateRouteRequest struct {
	Name             string `json:"name"`
	SiteID           string `json:"siteId"`
	GroupID          string `json:"groupId"`
	GroupName        string `json:"groupName"`
	ConcurrencyLimit int    `json:"concurrencyLimit"`
	ProxyID          string `json:"proxyId"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

type UpdateRouteRequest struct {
	Name             *string `json:"name,omitempty"`
	SiteID           *string `json:"siteId,omitempty"`
	GroupID          *string `json:"groupId,omitempty"`
	GroupName        *string `json:"groupName,omitempty"`
	ConcurrencyLimit *int    `json:"concurrencyLimit,omitempty"`
	ProxyID          *string `json:"proxyId,omitempty"`
	Enabled          *bool   `json:"enabled,omitempty"`
}

type CreateEgressProxyRequest struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type UpdateEgressProxyRequest struct {
	Name    *string `json:"name,omitempty"`
	URL     *string `json:"url,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

type EgressProxyTestResult struct {
	Success   bool   `json:"success"`
	LatencyMS int64  `json:"latencyMs"`
	ExitIP    string `json:"exitIp"`
	Message   string `json:"message"`
}

type CreateSmartGroupRequest struct {
	Name       string   `json:"name"`
	MemberKeys []string `json:"memberKeys"`
	Enabled    *bool    `json:"enabled,omitempty"`
}

type UpdateSmartGroupRequest struct {
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

type AddMemberRequest struct {
	EntryKey string `json:"entryKey"`
}

type UpdateMemberPolicyRequest struct {
	StreamOnly        *bool `json:"streamOnly,omitempty"`
	MinInputTokens    *int  `json:"minInputTokens,omitempty"`
	RequestsPerMinute *int  `json:"requestsPerMinute,omitempty"`
}

type KeyResponse struct {
	Key     string `json:"key"`
	Preview string `json:"preview"`
}

type ModelsResponse struct {
	Object string           `json:"object"`
	Data   []map[string]any `json:"data"`
}

type publicTarget struct {
	Credential Credential
	Route      *Route
	Group      *SmartGroup
}

type requestError struct {
	Status  int
	Message string
}

func (e *requestError) Error() string { return e.Message }
