package model_proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"transithub/backend/internal/modules/upstream"
)

const (
	cleanupBatchSize      = 50
	cleanupWorkerCount    = 2
	cleanupRequestTimeout = 60 * time.Second
	cleanupStateTimeout   = 5 * time.Second
	cleanupSiteInterval   = 500 * time.Millisecond
	cleanup429BaseDelay   = 30 * time.Second
	cleanup429MaxDelay    = 30 * time.Minute
	forceRefreshCooldown  = 30 * time.Second
)

type AccountResolver interface {
	RequireCurrentID(ctx context.Context, userID string) (string, error)
}

type UpstreamSites interface {
	GetSite(ctx context.Context, siteID string) (*upstream.Site, error)
	FreshSiteSession(ctx context.Context, siteID string) (upstream.Session, error)
	ForceRefreshSiteSession(ctx context.Context, siteID string) (upstream.Session, error)
	List(ctx context.Context, userID string) []upstream.Response
}

type Service struct {
	repository        *Repository
	accounts          AccountResolver
	sites             UpstreamSites
	platform          *upstream.PlatformService
	cipher            *KeyCipher
	limiter           *Limiter
	dataClient        *http.Client
	egressClientsMu   sync.Mutex
	egressClients     map[string]cachedEgressClient
	refreshInterval   time.Duration
	maxRequestBytes   int64
	sessionRefresh    singleflight.Group
	modelRefresh      singleflight.Group
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	cleanupRateMu     sync.Mutex
	cleanupNext       map[string]time.Time
	forceRefreshMu    sync.Mutex
	forceRefreshUntil map[string]time.Time
}

func NewService(repository *Repository, accounts AccountResolver, sites UpstreamSites, platform *upstream.PlatformService, cipher *KeyCipher, limiter *Limiter, maxRequestBytes int64, refreshInterval time.Duration) *Service {
	if maxRequestBytes <= 0 {
		maxRequestBytes = 64 << 20
	}
	if refreshInterval <= 0 {
		refreshInterval = 5 * time.Minute
	}
	dataClient, err := newModelHTTPClient("")
	if err != nil {
		panic(err)
	}
	return &Service{
		repository: repository, accounts: accounts, sites: sites, platform: platform,
		cipher: cipher, limiter: limiter, maxRequestBytes: maxRequestBytes, refreshInterval: refreshInterval,
		dataClient: dataClient, egressClients: make(map[string]cachedEgressClient),
		cleanupNext: make(map[string]time.Time), forceRefreshUntil: make(map[string]time.Time),
	}
}

func (s *Service) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(2)
	go s.cleanupLoop(ctx)
	go s.modelRefreshLoop(ctx)
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.dataClient != nil {
		s.dataClient.CloseIdleConnections()
	}
	s.egressClientsMu.Lock()
	defer s.egressClientsMu.Unlock()
	for _, cached := range s.egressClients {
		if cached.client != nil {
			cached.client.CloseIdleConnections()
		}
	}
}

func (s *Service) currentWorkspace(ctx context.Context, userID string) (string, error) {
	if s.accounts == nil {
		return "", &requestError{Status: 409, Message: "admin.adminAccounts.errors.noCurrentAccount"}
	}
	return s.accounts.RequireCurrentID(ctx, userID)
}

func (s *Service) ListSites(ctx context.Context, userID string) []upstream.Response {
	all := s.sites.List(ctx, userID)
	result := make([]upstream.Response, 0, len(all))
	for _, site := range all {
		if site.Platform == upstream.PlatformSub2API && site.Status == upstream.StatusConnected {
			result = append(result, site)
		}
	}
	return result
}

func (s *Service) ListGroups(ctx context.Context, userID, siteID string) ([]upstream.GroupInfo, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return nil, err
	}
	site, err := s.sites.GetSite(ctx, siteID)
	if err != nil || site == nil || site.UserID != userID || site.AdminAccountID != accountID || site.Platform != upstream.PlatformSub2API {
		return nil, &requestError{Status: 404, Message: "upstream site not found"}
	}
	session, err := s.freshSession(ctx, siteID)
	if err != nil {
		return nil, err
	}
	groups, err := s.platform.FetchAdminGroups(session)
	if err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *Service) ListRoutes(ctx context.Context, userID string) ([]Route, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return nil, err
	}
	routes, err := s.repository.ListRoutes(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	for index := range routes {
		routes[index].ActiveConcurrency, _ = s.limiter.Active(ctx, routes[index].ID)
	}
	return routes, nil
}

func (s *Service) CreateRoute(ctx context.Context, userID string, input CreateRouteRequest) (Route, string, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return Route{}, "", err
	}
	input.Name = strings.TrimSpace(input.Name)
	input.SiteID = strings.TrimSpace(input.SiteID)
	input.GroupID = strings.TrimSpace(input.GroupID)
	input.GroupName = strings.TrimSpace(input.GroupName)
	input.ProxyID = strings.TrimSpace(input.ProxyID)
	if input.Name == "" || input.SiteID == "" || input.GroupID == "" {
		return Route{}, "", &requestError{Status: 400, Message: "name, siteId and groupId are required"}
	}
	if input.ConcurrencyLimit == 0 {
		input.ConcurrencyLimit = 50
	}
	if input.ConcurrencyLimit < 1 || input.ConcurrencyLimit > 100000 {
		return Route{}, "", &requestError{Status: 400, Message: "concurrencyLimit must be between 1 and 100000"}
	}
	site, err := s.sites.GetSite(ctx, input.SiteID)
	if err != nil || site == nil || site.UserID != userID || site.AdminAccountID != accountID || site.Platform != upstream.PlatformSub2API || site.Session == nil {
		return Route{}, "", &requestError{Status: 400, Message: "a connected Sub2API site is required"}
	}
	if err := s.validateEgressProxy(ctx, userID, accountID, input.ProxyID); err != nil {
		return Route{}, "", err
	}
	if input.GroupName == "" {
		for _, group := range site.Metrics.Groups {
			if group.ID == input.GroupID {
				input.GroupName = group.Name
				break
			}
		}
	}
	if input.GroupName == "" {
		input.GroupName = input.GroupID
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	routeID, err := randomID("proute_")
	if err != nil {
		return Route{}, "", err
	}
	keyID, key, hash, ciphertext, preview, err := s.newAccessKey(OwnerRoute)
	if err != nil {
		return Route{}, "", err
	}
	route := Route{ID: routeID, UserID: userID, AdminAccountID: accountID, Name: input.Name, SiteID: input.SiteID, SiteName: site.Name, GroupID: input.GroupID, GroupName: input.GroupName, ConcurrencyLimit: input.ConcurrencyLimit, Enabled: enabled, ProxyID: input.ProxyID, KeyPreview: preview}
	if err := s.repository.CreateRoute(ctx, route, keyID, hash, ciphertext, preview); err != nil {
		return Route{}, "", err
	}
	go s.refreshRouteModels(context.Background(), route.ID)
	created, err := s.repository.GetRoute(ctx, route.ID)
	if err == nil && created != nil {
		route = *created
	}
	return route, key, err
}

func (s *Service) UpdateRoute(ctx context.Context, userID, routeID string, input UpdateRouteRequest) (Route, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return Route{}, err
	}
	route, err := s.repository.GetRoute(ctx, routeID)
	if err != nil || route == nil || route.UserID != userID || route.AdminAccountID != accountID {
		return Route{}, &requestError{Status: 404, Message: "proxy route not found"}
	}
	refreshModels := false
	if input.Name != nil {
		route.Name = strings.TrimSpace(*input.Name)
	}
	if input.SiteID != nil && strings.TrimSpace(*input.SiteID) != route.SiteID {
		route.SiteID = strings.TrimSpace(*input.SiteID)
		refreshModels = true
	}
	if input.GroupID != nil && strings.TrimSpace(*input.GroupID) != route.GroupID {
		route.GroupID = strings.TrimSpace(*input.GroupID)
		refreshModels = true
	}
	if input.GroupName != nil {
		route.GroupName = strings.TrimSpace(*input.GroupName)
	}
	if input.ConcurrencyLimit != nil {
		route.ConcurrencyLimit = *input.ConcurrencyLimit
	}
	if input.Enabled != nil {
		route.Enabled = *input.Enabled
	}
	if input.ProxyID != nil {
		proxyID := strings.TrimSpace(*input.ProxyID)
		if proxyID != route.ProxyID {
			route.ProxyID = proxyID
			refreshModels = true
		}
	}
	if route.Name == "" || route.SiteID == "" || route.GroupID == "" || route.ConcurrencyLimit < 1 || route.ConcurrencyLimit > 100000 {
		return Route{}, &requestError{Status: 400, Message: "invalid proxy route"}
	}
	site, siteErr := s.sites.GetSite(ctx, route.SiteID)
	if siteErr != nil || site == nil || site.UserID != userID || site.AdminAccountID != accountID || site.Platform != upstream.PlatformSub2API || site.Session == nil {
		return Route{}, &requestError{Status: 400, Message: "a connected Sub2API site is required"}
	}
	if err := s.validateEgressProxy(ctx, userID, accountID, route.ProxyID); err != nil {
		return Route{}, err
	}
	if err := s.repository.UpdateRoute(ctx, *route); err != nil {
		return Route{}, err
	}
	if refreshModels || route.Enabled {
		go s.refreshRouteModels(context.Background(), route.ID)
	}
	updated, err := s.repository.GetRoute(ctx, route.ID)
	if updated == nil {
		return *route, err
	}
	return *updated, err
}

func (s *Service) DeleteRoute(ctx context.Context, userID, routeID string) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	return s.repository.DeleteRoute(ctx, userID, accountID, routeID)
}

func (s *Service) ListSmartGroups(ctx context.Context, userID string) ([]SmartGroup, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return nil, err
	}
	groups, err := s.repository.ListSmartGroups(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	for groupIndex := range groups {
		for memberIndex := range groups[groupIndex].Members {
			groups[groupIndex].Members[memberIndex].ActiveConcurrency, _ = s.limiter.Active(ctx, groups[groupIndex].Members[memberIndex].ID)
		}
	}
	return groups, nil
}

func (s *Service) CreateSmartGroup(ctx context.Context, userID string, input CreateSmartGroupRequest) (SmartGroup, string, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return SmartGroup{}, "", err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return SmartGroup{}, "", &requestError{Status: 400, Message: "name is required"}
	}
	routeIDs := make([]string, 0, len(input.RouteIDs)+len(input.MemberKeys))
	seen := make(map[string]struct{})
	for _, routeID := range input.RouteIDs {
		routeID = strings.TrimSpace(routeID)
		if routeID == "" {
			continue
		}
		route, routeErr := s.repository.GetRoute(ctx, routeID)
		if routeErr != nil || route == nil || route.UserID != userID || route.AdminAccountID != accountID {
			return SmartGroup{}, "", &requestError{Status: 400, Message: "one or more member routes are invalid"}
		}
		if _, exists := seen[routeID]; exists {
			continue
		}
		seen[routeID] = struct{}{}
		routeIDs = append(routeIDs, routeID)
	}
	for _, entryKey := range input.MemberKeys {
		entryKey = strings.TrimSpace(entryKey)
		if entryKey == "" {
			continue
		}
		routeID, resolveErr := s.repository.ResolveRouteKey(ctx, hashKey(entryKey), userID, accountID)
		if resolveErr != nil {
			return SmartGroup{}, "", &requestError{Status: 400, Message: "one or more member keys are invalid"}
		}
		if _, exists := seen[routeID]; exists {
			continue
		}
		seen[routeID] = struct{}{}
		routeIDs = append(routeIDs, routeID)
	}
	if len(routeIDs) == 0 {
		return SmartGroup{}, "", &requestError{Status: 400, Message: "at least one member route is required"}
	}
	modelMapping, err := normalizeModelMapping(input.ModelMapping)
	if err != nil {
		return SmartGroup{}, "", err
	}
	groupID, err := randomID("pgroup_")
	if err != nil {
		return SmartGroup{}, "", err
	}
	keyID, key, hash, ciphertext, preview, err := s.newAccessKey(OwnerSmartGroup)
	if err != nil {
		return SmartGroup{}, "", err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	group := SmartGroup{ID: groupID, UserID: userID, AdminAccountID: accountID, Name: input.Name, Enabled: enabled, KeyPreview: preview, ModelMapping: modelMapping}
	if err := s.repository.CreateSmartGroup(ctx, group, routeIDs, keyID, hash, ciphertext, preview); err != nil {
		return SmartGroup{}, "", err
	}
	for _, routeID := range routeIDs {
		go s.refreshRouteModels(context.Background(), routeID)
	}
	created, err := s.repository.GetSmartGroup(ctx, group.ID)
	if err == nil && created != nil {
		group = *created
	}
	return group, key, err
}

func (s *Service) UpdateSmartGroup(ctx context.Context, userID, groupID string, input UpdateSmartGroupRequest) (SmartGroup, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return SmartGroup{}, err
	}
	group, err := s.repository.GetSmartGroup(ctx, groupID)
	if err != nil || group == nil || group.UserID != userID || group.AdminAccountID != accountID {
		return SmartGroup{}, &requestError{Status: 404, Message: "smart group not found"}
	}
	if input.Name != nil {
		group.Name = strings.TrimSpace(*input.Name)
	}
	if input.Enabled != nil {
		group.Enabled = *input.Enabled
	}
	if input.ModelMapping != nil {
		modelMapping, mappingErr := normalizeModelMapping(input.ModelMapping)
		if mappingErr != nil {
			return SmartGroup{}, mappingErr
		}
		group.ModelMapping = modelMapping
	}
	if group.Name == "" {
		return SmartGroup{}, &requestError{Status: 400, Message: "name is required"}
	}
	if err := s.repository.UpdateSmartGroup(ctx, *group); err != nil {
		return SmartGroup{}, err
	}
	return *group, nil
}

func (s *Service) DeleteSmartGroup(ctx context.Context, userID, groupID string) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	return s.repository.DeleteSmartGroup(ctx, userID, accountID, groupID)
}

func (s *Service) AddMember(ctx context.Context, userID, groupID, routeID, entryKey string) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		entryKey = strings.TrimSpace(entryKey)
		routeID, err = s.repository.ResolveRouteKey(ctx, hashKey(entryKey), userID, accountID)
		if err != nil {
			return &requestError{Status: 400, Message: "entry key is not a route key in this workspace"}
		}
	} else {
		route, routeErr := s.repository.GetRoute(ctx, routeID)
		if routeErr != nil || route == nil || route.UserID != userID || route.AdminAccountID != accountID {
			return &requestError{Status: 400, Message: "route is not available in this workspace"}
		}
	}
	if err := s.repository.AddMember(ctx, userID, accountID, groupID, routeID); err != nil {
		return err
	}
	go s.refreshRouteModels(context.Background(), routeID)
	return nil
}

func (s *Service) RemoveMember(ctx context.Context, userID, groupID, routeID string) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	return s.repository.RemoveMember(ctx, userID, accountID, groupID, routeID)
}

func (s *Service) UpdateMemberPolicy(ctx context.Context, userID, groupID, routeID string, input UpdateMemberPolicyRequest) error {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return err
	}
	if input.MinInputTokens != nil && *input.MinInputTokens != 0 && *input.MinInputTokens < 2000 {
		return &requestError{Status: 400, Message: "minInputTokens must be 0 or at least 2000"}
	}
	if input.MinInputTokens != nil && *input.MinInputTokens < 0 {
		return &requestError{Status: 400, Message: "minInputTokens cannot be negative"}
	}
	if input.RequestsPerMinute != nil && *input.RequestsPerMinute < 0 {
		return &requestError{Status: 400, Message: "requestsPerMinute cannot be negative"}
	}
	if input.Priority != nil && *input.Priority < 0 {
		return &requestError{Status: 400, Message: "priority cannot be negative"}
	}
	input.UpstreamKey = strings.TrimSpace(input.UpstreamKey)
	keywordsProvided := input.ExcludedKeywords != nil
	if keywordsProvided {
		keywords, normalizeErr := normalizeExcludedKeywords(input.ExcludedKeywords)
		if normalizeErr != nil {
			return normalizeErr
		}
		input.ExcludedKeywords = keywords
		if input.KeywordCheckEnabled != nil && *input.KeywordCheckEnabled && len(keywords) == 0 {
			return &requestError{Status: 400, Message: "at least one excluded keyword is required when keyword checking is enabled"}
		}
	}
	if input.StreamOnly == nil && input.MinInputTokens == nil && input.RequestsPerMinute == nil && input.Priority == nil && input.ModelMappingEnabled == nil && input.UseProvidedKey == nil && input.UpstreamKey == "" && input.KeywordCheckEnabled == nil && !keywordsProvided {
		return &requestError{Status: 400, Message: "at least one member policy field is required"}
	}
	var keyCiphertext, keyPreviewValue *string
	if input.UpstreamKey != "" {
		if input.UseProvidedKey != nil && !*input.UseProvidedKey {
			return &requestError{Status: 400, Message: "useProvidedKey must be enabled when upstreamKey is provided"}
		}
		if s.cipher == nil || !s.cipher.Available() {
			return errEncryptionKeyUnavailable
		}
		ciphertext, encryptErr := s.cipher.Encrypt(input.UpstreamKey)
		if encryptErr != nil {
			return encryptErr
		}
		preview := keyPreview(input.UpstreamKey)
		keyCiphertext = &ciphertext
		keyPreviewValue = &preview
		if input.UseProvidedKey == nil {
			enabled := true
			input.UseProvidedKey = &enabled
		}
	}
	if input.UseProvidedKey != nil && *input.UseProvidedKey && keyCiphertext == nil {
		hasKey, lookupErr := s.repository.MemberHasProvidedKey(ctx, userID, accountID, groupID, routeID)
		if lookupErr != nil {
			return lookupErr
		}
		if !hasKey {
			return &requestError{Status: 400, Message: "upstreamKey is required when useProvidedKey is enabled"}
		}
	}
	if input.UseProvidedKey != nil && !*input.UseProvidedKey {
		empty := ""
		keyCiphertext = &empty
		keyPreviewValue = &empty
	}
	if input.KeywordCheckEnabled != nil && *input.KeywordCheckEnabled && !keywordsProvided {
		hasKeywords, lookupErr := s.repository.MemberHasExcludedKeywords(ctx, userID, accountID, groupID, routeID)
		if lookupErr != nil {
			return lookupErr
		}
		if !hasKeywords {
			return &requestError{Status: 400, Message: "excludedKeywords is required when keyword checking is enabled"}
		}
	}
	var excludedKeywords []string
	if keywordsProvided {
		excludedKeywords = input.ExcludedKeywords
	}
	return s.repository.UpdateMemberPolicy(ctx, userID, accountID, groupID, routeID,
		input.StreamOnly, input.MinInputTokens, input.RequestsPerMinute, input.Priority,
		input.ModelMappingEnabled, input.UseProvidedKey, keyCiphertext, keyPreviewValue,
		input.KeywordCheckEnabled, excludedKeywords)
}

const (
	maxModelMappings        = 128
	maxModelMappingPartSize = 256
	maxExcludedKeywords     = 64
	maxExcludedKeywordSize  = 256
)

func normalizeModelMapping(values map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for source, target := range values {
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if source == "" || target == "" {
			return nil, &requestError{Status: 400, Message: "model mapping source and target are required"}
		}
		if len([]rune(source)) > maxModelMappingPartSize || len([]rune(target)) > maxModelMappingPartSize {
			return nil, &requestError{Status: 400, Message: "model mapping source or target is too long"}
		}
		result[source] = target
		if len(result) > maxModelMappings {
			return nil, &requestError{Status: 400, Message: "too many model mappings"}
		}
	}
	return result, nil
}

func normalizeExcludedKeywords(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len([]rune(value)) > maxExcludedKeywordSize {
			return nil, &requestError{Status: 400, Message: "excluded keyword is too long"}
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
		if len(result) > maxExcludedKeywords {
			return nil, &requestError{Status: 400, Message: "too many excluded keywords"}
		}
	}
	return result, nil
}

func (s *Service) RevealKey(ctx context.Context, userID, ownerType, ownerID string) (KeyResponse, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return KeyResponse{}, err
	}
	ciphertext, preview, err := s.repository.RevealKey(ctx, userID, accountID, ownerType, ownerID)
	if err != nil {
		return KeyResponse{}, err
	}
	key, err := s.cipher.Decrypt(ciphertext)
	return KeyResponse{Key: key, Preview: preview}, err
}

func (s *Service) RotateKey(ctx context.Context, userID, ownerType, ownerID string) (KeyResponse, error) {
	accountID, err := s.currentWorkspace(ctx, userID)
	if err != nil {
		return KeyResponse{}, err
	}
	if err := s.repository.ValidateOwner(ctx, userID, accountID, ownerType, ownerID); err != nil {
		return KeyResponse{}, err
	}
	_, key, hash, ciphertext, preview, err := s.newAccessKey(ownerType)
	if err != nil {
		return KeyResponse{}, err
	}
	if err := s.repository.RotateKey(ctx, userID, accountID, ownerType, ownerID, hash, ciphertext, preview); err != nil {
		return KeyResponse{}, err
	}
	return KeyResponse{Key: key, Preview: preview}, nil
}

func (s *Service) ResolvePublicTarget(ctx context.Context, entryKey string) (*publicTarget, error) {
	credential, err := s.repository.FindCredential(ctx, hashKey(entryKey))
	if err != nil || credential == nil {
		return nil, &requestError{Status: 401, Message: "invalid API key"}
	}
	target := &publicTarget{Credential: *credential}
	switch credential.OwnerType {
	case OwnerRoute:
		route, routeErr := s.repository.GetRoute(ctx, credential.OwnerID)
		if routeErr != nil || route == nil || !route.Enabled {
			return nil, &requestError{Status: 403, Message: "proxy route is disabled"}
		}
		target.Route = route
	case OwnerSmartGroup:
		group, groupErr := s.repository.GetSmartGroup(ctx, credential.OwnerID)
		if groupErr != nil || group == nil || !group.Enabled {
			return nil, &requestError{Status: 403, Message: "smart group is disabled"}
		}
		target.Group = group
	default:
		return nil, &requestError{Status: 401, Message: "invalid API key"}
	}
	return target, nil
}

func (s *Service) newAccessKey(ownerType string) (id, key, hash, ciphertext, preview string, err error) {
	if s.cipher == nil || !s.cipher.Available() {
		err = errEncryptionKeyUnavailable
		return
	}
	id, err = randomID("pkey_")
	if err != nil {
		return
	}
	key, err = generateEntryKey(ownerType)
	if err != nil {
		return
	}
	hash = hashKey(key)
	preview = keyPreview(key)
	ciphertext, err = s.cipher.Encrypt(key)
	return
}

func (s *Service) freshSession(ctx context.Context, siteID string) (upstream.Session, error) {
	value, err, _ := s.sessionRefresh.Do(siteID, func() (any, error) {
		return s.sites.FreshSiteSession(ctx, siteID)
	})
	if err != nil {
		return upstream.Session{}, err
	}
	return value.(upstream.Session), nil
}

func (s *Service) createRemoteKey(ctx context.Context, route Route) (string, CleanupJob, error) {
	jobID, err := randomID("pcleanup_")
	if err != nil {
		return "", CleanupJob{}, err
	}
	job := CleanupJob{ID: jobID, UserID: route.UserID, AdminAccountID: route.AdminAccountID, RouteID: route.ID, SiteID: route.SiteID, RemoteKeyName: "transithub-" + jobID}
	if err := s.repository.CreateCleanupJob(ctx, job); err != nil {
		return "", CleanupJob{}, err
	}
	session, err := s.freshSession(ctx, route.SiteID)
	if err != nil {
		return "", job, err
	}
	groupID, err := strconv.Atoi(route.GroupID)
	if err != nil {
		return "", job, fmt.Errorf("invalid Sub2API group id: %w", err)
	}
	remoteID, secret, err := s.platform.CreateSub2APIKey(session, job.RemoteKeyName, groupID)
	if isUpstreamUnauthorized(err) {
		session, err = s.forceSession(ctx, route.SiteID)
		if err == nil {
			remoteID, secret, err = s.platform.CreateSub2APIKey(session, job.RemoteKeyName, groupID)
		}
	}
	if err != nil {
		return "", job, err
	}
	job.RemoteKeyID = remoteID
	job.Status = "active"
	if err := s.repository.ActivateCleanupJob(ctx, job.ID, remoteID); err != nil {
		return "", job, err
	}
	return secret, job, nil
}

func (s *Service) beginDelete(job CleanupJob) {
	if err := s.repository.MarkCleanupPending(context.Background(), job.ID); err != nil {
		log.Printf("[model-proxy] mark cleanup pending job_id=%s err=%v", job.ID, err)
	}
}

func (s *Service) processCleanupJob(ctx context.Context, job CleanupJob) {
	if err := s.waitCleanupSlot(ctx, job.SiteID); err != nil {
		stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
		s.retryCleanup(stateCtx, job, err)
		cancel()
		return
	}
	if err := s.deleteCleanupJob(ctx, job); err != nil {
		stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
		s.retryCleanup(stateCtx, job, err)
		cancel()
		return
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
	_ = s.repository.CompleteCleanupJob(stateCtx, job.ID)
	cancel()
}

// deleteCleanupJob performs one cleanup attempt and leaves retry scheduling to
// the caller. A missing remote key is already in the desired terminal state.
func (s *Service) deleteCleanupJob(ctx context.Context, job CleanupJob) error {
	if job.RemoteKeyID == "" {
		session, err := s.freshSession(ctx, job.SiteID)
		if err != nil {
			return err
		}
		key, err := s.platform.FindSub2APIKeyByName(session, job.RemoteKeyName)
		if isUpstreamUnauthorized(err) {
			session, err = s.forceSession(ctx, job.SiteID)
			if err == nil {
				key, err = s.platform.FindSub2APIKeyByName(session, job.RemoteKeyName)
			}
		}
		if err != nil {
			return err
		}
		if key != nil {
			job.RemoteKeyID = key.ID
			if err := s.repository.SetCleanupRemoteKey(ctx, job.ID, key.ID); err != nil {
				return err
			}
		}
		if job.RemoteKeyID == "" {
			return nil
		}
	}
	session, err := s.freshSession(ctx, job.SiteID)
	if err == nil {
		err = s.platform.DeleteSub2APIKey(session, job.RemoteKeyID)
	}
	if isUpstreamUnauthorized(err) {
		session, err = s.forceSession(ctx, job.SiteID)
		if err == nil {
			err = s.platform.DeleteSub2APIKey(session, job.RemoteKeyID)
		}
	}
	if err == nil || isUpstreamNotFound(err) {
		return nil
	}
	return err
}

// deleteCleanupJobNow is used by the SSE gate. It completes the remote-key
// deletion before the first response event is released downstream.
func (s *Service) deleteCleanupJobNow(job CleanupJob) error {
	_ = s.repository.MarkCleanupPending(context.Background(), job.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.deleteCleanupJob(ctx, job); err != nil {
		s.retryCleanup(context.Background(), job, err)
		return err
	}
	return s.repository.CompleteCleanupJob(context.Background(), job.ID)
}

func (s *Service) retryCleanup(ctx context.Context, job CleanupJob, cleanupErr error) {
	job.Attempts++
	if requestErr, ok := cleanupRequestError(cleanupErr); ok && isCleanupUpstreamThrottle(requestErr.StatusCode) {
		exponent := math.Min(float64(job.Attempts-1), 5)
		delay := time.Duration(math.Pow(2, exponent)) * cleanup429BaseDelay
		if delay > cleanup429MaxDelay {
			delay = cleanup429MaxDelay
		}
		delay += time.Duration(mathrand.Int64N(int64(cleanup429BaseDelay)))
		_ = s.repository.RetryCleanupJob(ctx, job.ID, job.Attempts, time.Now().Add(delay), cleanupError(cleanupErr))
		return
	}
	exponent := math.Min(float64(job.Attempts-1), 9)
	delay := time.Duration(math.Pow(2, exponent)) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	delay += time.Duration(mathrand.Int64N(int64(time.Second)))
	_ = s.repository.RetryCleanupJob(ctx, job.ID, job.Attempts, time.Now().Add(delay), cleanupError(cleanupErr))
}

func isCleanupUpstreamThrottle(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func cleanupRequestError(err error) (*upstream.RequestError, bool) {
	var requestErr *upstream.RequestError
	return requestErr, errors.As(err, &requestErr)
}

func cleanupError(err error) error {
	if err == nil {
		return nil
	}
	var requestErr *upstream.RequestError
	if errors.As(err, &requestErr) && requestErr.StatusCode > 0 {
		return fmt.Errorf("%w (status=%d)", err, requestErr.StatusCode)
	}
	return err
}

func isUpstreamNotFound(err error) bool {
	var requestErr *upstream.RequestError
	return errors.As(err, &requestErr) && requestErr.StatusCode == http.StatusNotFound
}

func isUpstreamUnauthorized(err error) bool {
	var requestErr *upstream.RequestError
	return errors.As(err, &requestErr) && requestErr.StatusCode == http.StatusUnauthorized
}

func (s *Service) forceSession(ctx context.Context, siteID string) (upstream.Session, error) {
	s.forceRefreshMu.Lock()
	if until := s.forceRefreshUntil[siteID]; until.After(time.Now()) {
		s.forceRefreshMu.Unlock()
		return upstream.Session{}, fmt.Errorf("upstream session refresh is rate limited until %s", until.Format(time.RFC3339))
	}
	s.forceRefreshMu.Unlock()
	value, err, _ := s.sessionRefresh.Do("force:"+siteID, func() (any, error) {
		return s.sites.ForceRefreshSiteSession(ctx, siteID)
	})
	if err != nil {
		if requestErr, ok := cleanupRequestError(err); ok && (requestErr.StatusCode == http.StatusUnauthorized || requestErr.StatusCode == http.StatusTooManyRequests) {
			s.forceRefreshMu.Lock()
			s.forceRefreshUntil[siteID] = time.Now().Add(forceRefreshCooldown)
			s.forceRefreshMu.Unlock()
		}
		return upstream.Session{}, err
	}
	s.forceRefreshMu.Lock()
	delete(s.forceRefreshUntil, siteID)
	s.forceRefreshMu.Unlock()
	return value.(upstream.Session), nil
}

func (s *Service) cleanupLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.processDueCleanupJobs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) waitCleanupSlot(ctx context.Context, siteID string) error {
	for {
		now := time.Now()
		s.cleanupRateMu.Lock()
		next := s.cleanupNext[siteID]
		if !next.After(now) {
			s.cleanupNext[siteID] = now.Add(cleanupSiteInterval)
			s.cleanupRateMu.Unlock()
			return nil
		}
		wait := time.Until(next)
		s.cleanupRateMu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// prepareCleanupJobs resolves missing remote IDs once per claimed batch. Older
// jobs were created before remote IDs were persisted, so resolving each job in
// a worker would repeatedly scan the entire upstream key list and trigger 429s.
func (s *Service) prepareCleanupJobs(ctx context.Context, jobs []CleanupJob) []CleanupJob {
	prepared := make([]CleanupJob, 0, len(jobs))
	bySite := make(map[string][]int)
	for index := range jobs {
		if jobs[index].RemoteKeyID == "" {
			bySite[jobs[index].SiteID] = append(bySite[jobs[index].SiteID], index)
			continue
		}
		prepared = append(prepared, jobs[index])
	}
	for siteID, indexes := range bySite {
		if err := s.waitCleanupSlot(ctx, siteID); err != nil {
			s.retryCleanupBatch(jobs, indexes, err)
			continue
		}
		session, err := s.freshSession(ctx, siteID)
		if isUpstreamUnauthorized(err) {
			session, err = s.forceSession(ctx, siteID)
		}
		if err == nil {
			names := make([]string, 0, len(indexes))
			for _, index := range indexes {
				names = append(names, jobs[index].RemoteKeyName)
			}
			var found map[string]upstream.Sub2APIKeyItem
			found, err = s.platform.FindSub2APIKeysByNames(session, names)
			if isUpstreamUnauthorized(err) {
				session, err = s.forceSession(ctx, siteID)
				if err == nil {
					found, err = s.platform.FindSub2APIKeysByNames(session, names)
				}
			}
			if err == nil {
				for _, index := range indexes {
					key, ok := found[jobs[index].RemoteKeyName]
					if !ok || strings.TrimSpace(key.ID) == "" {
						stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
						_ = s.repository.CompleteCleanupJob(stateCtx, jobs[index].ID)
						cancel()
						continue
					}
					jobs[index].RemoteKeyID = key.ID
					stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
					if saveErr := s.repository.SetCleanupRemoteKey(stateCtx, jobs[index].ID, key.ID); saveErr != nil {
						cancel()
						retryCtx, retryCancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
						s.retryCleanup(retryCtx, jobs[index], saveErr)
						retryCancel()
						continue
					}
					cancel()
					prepared = append(prepared, jobs[index])
				}
				continue
			}
		}
		s.retryCleanupBatch(jobs, indexes, err)
	}
	return prepared
}

func (s *Service) retryCleanupBatch(jobs []CleanupJob, indexes []int, cleanupErr error) {
	if cleanupErr == nil {
		cleanupErr = errors.New("cleanup key lookup failed")
	}
	for _, index := range indexes {
		stateCtx, cancel := context.WithTimeout(context.Background(), cleanupStateTimeout)
		s.retryCleanup(stateCtx, jobs[index], cleanupErr)
		cancel()
	}
}

func (s *Service) processDueCleanupJobs(ctx context.Context) {
	jobs, err := s.repository.ClaimCleanupJobs(ctx, cleanupBatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[model-proxy] claim cleanup jobs: %v", err)
		}
		return
	}
	jobs = s.prepareCleanupJobs(ctx, jobs)
	if len(jobs) == 0 {
		return
	}
	if len(jobs) > 0 {
		workers := cleanupWorkerCount
		if len(jobs) < workers {
			workers = len(jobs)
		}
		jobQueue := make(chan CleanupJob)
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				for job := range jobQueue {
					jobCtx, cancel := context.WithTimeout(ctx, cleanupRequestTimeout)
					s.processCleanupJob(jobCtx, job)
					cancel()
				}
			}()
		}
		for _, job := range jobs {
			jobQueue <- job
		}
		close(jobQueue)
		wg.Wait()
	}
	_ = s.repository.PurgeCompletedCleanupJobs(ctx)
}

func (s *Service) modelRefreshLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.refreshStaleModels(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) refreshStaleModels(ctx context.Context) {
	routes, err := s.repository.RoutesNeedingRefresh(ctx, time.Now().Add(-s.refreshInterval))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[model-proxy] list stale models: %v", err)
		}
		return
	}
	for _, route := range routes {
		_ = s.refreshRouteModels(ctx, route.ID)
	}
}

func (s *Service) refreshRouteModels(ctx context.Context, routeID string) error {
	_, err, _ := s.modelRefresh.Do(routeID, func() (any, error) {
		route, err := s.repository.GetRoute(ctx, routeID)
		if err != nil || route == nil || !route.Enabled {
			return nil, err
		}
		client, err := s.dataClientForRoute(ctx, *route)
		if err != nil {
			_ = s.repository.SetModelSyncError(ctx, routeID, err)
			return nil, err
		}
		secret, job, err := s.createRemoteKey(ctx, *route)
		if err != nil {
			_ = s.repository.SetModelSyncError(ctx, routeID, err)
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.mustSiteBaseURL(ctx, route.SiteID), "/")+"/v1/models", nil)
		if err != nil {
			s.beginDelete(job)
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := client.Do(request)
		if err != nil {
			s.beginDelete(job)
			_ = s.repository.SetModelSyncError(ctx, routeID, err)
			return nil, err
		}
		s.beginDelete(job)
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = fmt.Errorf("models endpoint returned %d", response.StatusCode)
			_ = s.repository.SetModelSyncError(ctx, routeID, err)
			return nil, err
		}
		var payload struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			_ = s.repository.SetModelSyncError(ctx, routeID, err)
			return nil, err
		}
		models := make([]Model, 0, len(payload.Data))
		for _, raw := range payload.Data {
			id, _ := raw["id"].(string)
			if strings.TrimSpace(id) == "" {
				continue
			}
			object, _ := raw["object"].(string)
			ownedBy, _ := raw["owned_by"].(string)
			models = append(models, Model{ID: id, Object: object, OwnedBy: ownedBy, Raw: raw})
		}
		if err := s.repository.ReplaceRouteModels(ctx, routeID, models); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return err
}

func (s *Service) refreshGroupModels(ctx context.Context, groupID string) {
	routes, err := s.repository.ListGroupRoutes(ctx, groupID, true, "")
	if err != nil {
		return
	}
	for _, route := range routes {
		_ = s.refreshRouteModels(ctx, route.ID)
	}
}

func (s *Service) mustSiteBaseURL(ctx context.Context, siteID string) string {
	site, err := s.sites.GetSite(ctx, siteID)
	if err != nil || site == nil {
		return ""
	}
	return site.BaseURL
}

func randomID(prefix string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

func isNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
