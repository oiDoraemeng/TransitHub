package model_proxy

import "time"

const (
	OwnerRoute      = "route"
	OwnerSmartGroup = "smart_group"

	KeywordMatchModeDisabled     = "disabled"
	KeywordMatchModeSkipOnMatch  = "skip_on_match"
	KeywordMatchModeRequireMatch = "require_match"
)

type Route struct {
	ID                  string                       `json:"id"`
	UserID              string                       `json:"-"`
	AdminAccountID      string                       `json:"-"`
	Name                string                       `json:"name"`
	SiteID              string                       `json:"siteId"`
	SiteName            string                       `json:"siteName"`
	GroupID             string                       `json:"groupId"`
	GroupName           string                       `json:"groupName"`
	ConcurrencyLimit    int                          `json:"concurrencyLimit"`
	ActiveConcurrency   int64                        `json:"activeConcurrency"`
	TodayRequests       int64                        `json:"todayRequests"`
	Enabled             bool                         `json:"enabled"`
	ProxyID             string                       `json:"proxyId"`
	ProxyName           string                       `json:"proxyName"`
	KeyPreview          string                       `json:"keyPreview"`
	ModelCount          int                          `json:"modelCount"`
	CleanupPending      int                          `json:"cleanupPending"`
	PausedUntil         *time.Time                   `json:"pausedUntil,omitempty"`
	NextProbeAt         *time.Time                   `json:"nextProbeAt,omitempty"`
	ConsecutiveFailures int64                        `json:"consecutiveFailures,omitempty"`
	ModelHealth         []ModelHealth                `json:"modelHealth,omitempty"`
	RemovedModels       map[string]RemovedRouteModel `json:"-"`
	// Member policies are populated only when this route is loaded through a
	// smart-group membership. They are intentionally not route-level settings.
	StreamOnly            bool              `json:"streamOnly,omitempty"`
	SchedulingPaused      bool              `json:"schedulingPaused,omitempty"`
	NeverAutoPause        bool              `json:"neverAutoPause,omitempty"`
	MemberPolicyRevision  int64             `json:"-"`
	MinInputTokens        int               `json:"minInputTokens,omitempty"`
	RequestsPerMinute     int               `json:"requestsPerMinute,omitempty"`
	Priority              int               `json:"priority,omitempty"`
	UseProvidedKey        bool              `json:"useProvidedKey,omitempty"`
	RouteProvidedKey      bool              `json:"routeProvidedKey,omitempty"`
	MemberProvidedKey     bool              `json:"memberProvidedKey,omitempty"`
	UpstreamKeyPreview    string            `json:"upstreamKeyPreview,omitempty"`
	UpstreamKeyCiphertext string            `json:"-"`
	KeywordMatchMode      string            `json:"keywordMatchMode,omitempty"`
	ModelMappingEnabled   bool              `json:"modelMappingEnabled,omitempty"`
	ModelMapping          map[string]string `json:"modelMapping,omitempty"`
	SyncKeyDeleteEnabled  bool              `json:"syncKeyDeleteEnabled,omitempty"`
	SyncKeyDeleteDelayMS  int               `json:"syncKeyDeleteDelayMs,omitempty"`
	ModelSyncedAt         *time.Time        `json:"modelSyncedAt"`
	ModelSyncError        string            `json:"modelSyncError"`
	CreatedAt             time.Time         `json:"createdAt"`
	UpdatedAt             time.Time         `json:"updatedAt"`
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
	ID                        string           `json:"id"`
	UserID                    string           `json:"-"`
	AdminAccountID            string           `json:"-"`
	Name                      string           `json:"name"`
	Enabled                   bool             `json:"enabled"`
	LocalReplyEnabled         bool             `json:"localReplyEnabled"`
	LocalReplyMinTokens       int              `json:"localReplyMinTokens"`
	LocalReplyMaxOutputTokens int              `json:"localReplyMaxOutputTokens"`
	LocalReplyText            string           `json:"localReplyText"`
	Keywords                  []string         `json:"keywords"`
	KeywordMatchCounts        map[string]int64 `json:"keywordMatchCounts"`
	CustomKeys                []CustomKey      `json:"customKeys"`
	KeyPreview                string           `json:"keyPreview"`
	TotalConcurrency          int              `json:"totalConcurrency"`
	Members                   []Route          `json:"members"`
	Models                    []Model          `json:"models"`
	CreatedAt                 time.Time        `json:"createdAt"`
	UpdatedAt                 time.Time        `json:"updatedAt"`
}

type CustomKey struct {
	ID         string    `json:"id"`
	Preview    string    `json:"preview"`
	CreatedAt  time.Time `json:"createdAt"`
	Hash       string    `json:"-"`
	Ciphertext string    `json:"-"`
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
	UpstreamKey      string `json:"upstreamKey,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

type UpdateRouteRequest struct {
	Name             *string `json:"name,omitempty"`
	SiteID           *string `json:"siteId,omitempty"`
	GroupID          *string `json:"groupId,omitempty"`
	GroupName        *string `json:"groupName,omitempty"`
	ConcurrencyLimit *int    `json:"concurrencyLimit,omitempty"`
	ProxyID          *string `json:"proxyId,omitempty"`
	UpstreamKey      string  `json:"upstreamKey,omitempty"`
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
	Name                      string   `json:"name"`
	MemberKeys                []string `json:"memberKeys,omitempty"`
	RouteIDs                  []string `json:"routeIds,omitempty"`
	Keywords                  []string `json:"keywords,omitempty"`
	Enabled                   *bool    `json:"enabled,omitempty"`
	LocalReplyEnabled         bool     `json:"localReplyEnabled,omitempty"`
	LocalReplyMinTokens       *int     `json:"localReplyMinTokens,omitempty"`
	LocalReplyMaxOutputTokens *int     `json:"localReplyMaxOutputTokens,omitempty"`
	LocalReplyText            *string  `json:"localReplyText,omitempty"`
}

type UpdateSmartGroupRequest struct {
	LocalReplyEnabled         *bool     `json:"localReplyEnabled,omitempty"`
	LocalReplyMinTokens       *int      `json:"localReplyMinTokens,omitempty"`
	LocalReplyMaxOutputTokens *int      `json:"localReplyMaxOutputTokens,omitempty"`
	LocalReplyText            *string   `json:"localReplyText,omitempty"`
	Name                      *string   `json:"name,omitempty"`
	Keywords                  *[]string `json:"keywords,omitempty"`
	Enabled                   *bool     `json:"enabled,omitempty"`
}

type UpdateSmartGroupCustomKeysRequest struct {
	AddKeys      []string `json:"addKeys,omitempty"`
	RemoveKeyIDs []string `json:"removeKeyIds,omitempty"`
}

type AddMemberRequest struct {
	RouteID  string `json:"routeId,omitempty"`
	EntryKey string `json:"entryKey,omitempty"`
}

type UpdateMemberPolicyRequest struct {
	SchedulingPaused     *bool              `json:"schedulingPaused,omitempty"`
	NeverAutoPause       *bool              `json:"neverAutoPause,omitempty"`
	StreamOnly           *bool              `json:"streamOnly,omitempty"`
	MinInputTokens       *int               `json:"minInputTokens,omitempty"`
	RequestsPerMinute    *int               `json:"requestsPerMinute,omitempty"`
	Priority             *int               `json:"priority,omitempty"`
	ModelMappingEnabled  *bool              `json:"modelMappingEnabled,omitempty"`
	ModelMapping         *map[string]string `json:"modelMapping,omitempty"`
	UseProvidedKey       *bool              `json:"useProvidedKey,omitempty"`
	UpstreamKey          string             `json:"upstreamKey,omitempty"`
	KeywordMatchMode     *string            `json:"keywordMatchMode,omitempty"`
	SyncKeyDeleteEnabled *bool              `json:"syncKeyDeleteEnabled,omitempty"`
	SyncKeyDeleteDelayMS *int               `json:"syncKeyDeleteDelayMs,omitempty"`
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
