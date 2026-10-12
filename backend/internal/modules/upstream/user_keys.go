package upstream

import (
	"fmt"
	"strconv"
	"strings"
)

// CreateUserKey creates a user credential in the platform's native group.
// Sub2API uses numeric group IDs; New-API uses group names.
func (s *PlatformService) CreateUserKey(session Session, name, group string) (string, string, error) {
	switch session.Platform {
	case PlatformSub2API:
		groupID, err := strconv.Atoi(group)
		if err != nil {
			return "", "", fmt.Errorf("invalid Sub2API group id: %w", err)
		}
		return s.CreateSub2APIKey(session, name, groupID)
	case PlatformNewAPI:
		return s.CreateNewAPIToken(session, name, group)
	default:
		return "", "", newRequestError(ErrorRequest, session.Platform)
	}
}

func (s *PlatformService) DeleteUserKey(session Session, keyID string) error {
	switch session.Platform {
	case PlatformSub2API:
		return s.DeleteSub2APIKey(session, keyID)
	case PlatformNewAPI:
		return s.DeleteNewAPIToken(session, keyID)
	default:
		return newRequestError(ErrorRequest, session.Platform)
	}
}

func (s *PlatformService) FindUserKeyByName(session Session, name string) (*Sub2APIKeyItem, error) {
	keys, err := s.FindUserKeysByNames(session, []string{name})
	if err != nil {
		return nil, err
	}
	if key, ok := keys[name]; ok {
		return &key, nil
	}
	return nil, nil
}

// FindUserKeysByNames recovers temporary credentials whose creation response
// was lost. A cleanup batch shares one complete pagination scan per site.
func (s *PlatformService) FindUserKeysByNames(session Session, names []string) (map[string]Sub2APIKeyItem, error) {
	if session.Platform == PlatformSub2API {
		return s.FindSub2APIKeysByNames(session, names)
	}
	if session.Platform != PlatformNewAPI || !session.IsAuthenticated() {
		return nil, newRequestError(ErrorAuth, session.Platform)
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = struct{}{}
		}
	}
	found := make(map[string]Sub2APIKeyItem, len(wanted))
	if len(wanted) == 0 {
		return found, nil
	}
	for page := 1; ; page++ {
		keys, more, err := s.listNewAPITokensPage(session, page, 100)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if _, ok := wanted[key.Name]; ok {
				found[key.Name] = key
			}
		}
		if len(found) == len(wanted) || !more {
			return found, nil
		}
	}
}

// RecoverUserKey returns a credential after a session refresh, without creating
// a second key when the original create succeeded but lookup/reveal failed.
func (s *PlatformService) RecoverUserKey(session Session, name string) (string, string, error) {
	key, err := s.FindUserKeyByName(session, name)
	if err != nil || key == nil {
		return "", "", err
	}
	if session.Platform == PlatformNewAPI {
		secret, err := s.FetchNewAPITokenKey(session, key.ID)
		return key.ID, secret, err
	}
	if key.Key == "" || isMaskedSub2APIKey(key.Key) {
		return key.ID, "", newRequestError(ErrorInvalidResponse, session.Platform)
	}
	return key.ID, key.Key, nil
}
