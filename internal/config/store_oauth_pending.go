package config

import (
	"reflect"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type pendingOAuthRotation struct {
	predecessor oauth.Token
	token       oauth.Token
	record      oauthJournalRecord
}

func oauthTokenProviderConfig(providerID string, pc ProviderConfig, token *oauth.Token) ProviderConfig {
	pc.OAuthToken = token
	if providerID == "openai-codex" {
		pc.APIKey = ""
	} else {
		pc.APIKey = token.AccessToken
	}
	if providerID == string(catwalk.InferenceProviderCopilot) {
		pc.SetupGitHubCopilot()
	}
	return pc
}

func (s *ConfigStore) recordPendingOAuthLocked(providerID string, predecessor, token *oauth.Token, records ...*oauthJournalRecord) {
	if s.pendingOAuth == nil {
		s.pendingOAuth = make(map[string]pendingOAuthRotation)
	}
	record := oauthJournalRecord{Token: token, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(predecessor)}
	if len(records) > 0 && records[0] != nil {
		record = *records[0]
	}
	var digests []string
	if previous, ok := s.pendingOAuth[providerID]; ok {
		digests = append(digests, previous.record.PredecessorRefreshTokenSHA256)
		digests = append(digests, previous.record.BurnedRefreshTokenSHA256...)
	}
	for key, owned := range s.oauthOutbox {
		if key.provider == providerID {
			digests = append(digests, owned.record.PredecessorRefreshTokenSHA256)
			digests = append(digests, owned.record.BurnedRefreshTokenSHA256...)
		}
	}
	digests = append(digests, oauthRefreshTokenSHA256(predecessor))
	record = oauthJournalWithBurned(&record, digests...)
	copyToken := *token
	record.Token = &copyToken
	s.pendingOAuth[providerID] = pendingOAuthRotation{predecessor: *predecessor, token: copyToken, record: record}
}

func (s *ConfigStore) clearPendingOAuth(providerID string, predecessor, token *oauth.Token) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if entry, ok := s.pendingOAuth[providerID]; ok && reflect.DeepEqual(&entry.predecessor, predecessor) && reflect.DeepEqual(&entry.token, token) {
		delete(s.pendingOAuth, providerID)
	}
}

func (s *ConfigStore) pendingOAuthCurrent(providerID string, predecessor, token *oauth.Token) bool {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	entry, ok := s.pendingOAuth[providerID]
	if !ok || !reflect.DeepEqual(&entry.predecessor, predecessor) || !reflect.DeepEqual(&entry.token, token) {
		return false
	}
	cur := s.loadSnapshot()
	if cur.config == nil || cur.config.Providers == nil {
		return false
	}
	pc, ok := cur.config.Providers.Get(providerID)
	return ok && reflect.DeepEqual(pc.OAuthToken, token)
}

// Predecessors and earlier-expiring replacements retain the pending rotation.
func (s *ConfigStore) overlayPendingOAuthLocked(candidate *storeSnapshot) {
	for providerID, entry := range s.pendingOAuth {
		var pc ProviderConfig
		var ok bool
		if candidate.config != nil && candidate.config.Providers != nil {
			pc, ok = candidate.config.Providers.Get(providerID)
		}
		decision := oauthJournalLogout
		if ok {
			record := entry.record
			record.Token = &entry.token
			decision = arbitrateOAuthJournal(&record, &entry.predecessor, pc.OAuthToken, nil)
		}
		if decision != oauthJournalPredecessor && decision != oauthJournalLater && decision != oauthJournalEqual {
			for key, owned := range s.oauthOutbox {
				if key.provider == providerID && reflect.DeepEqual(owned.record.Token, &entry.token) {
					owned.obsolete = true
				}
			}
			delete(s.pendingOAuth, providerID)
			continue
		}
		cur := s.loadSnapshot()
		if cur.config == nil || cur.config.Providers == nil {
			delete(s.pendingOAuth, providerID)
			continue
		}
		live, exists := cur.config.Providers.Get(providerID)
		if !exists || !reflect.DeepEqual(live.OAuthToken, &entry.token) {
			delete(s.pendingOAuth, providerID)
			continue
		}
		token := entry.token
		cfgCopy := *candidate.config
		providersCopy := candidate.config.Providers.Copy()
		providersCopy[providerID] = oauthTokenProviderConfig(providerID, pc, &token)
		cfgCopy.Providers = csync.NewMapFrom(providersCopy)
		candidate.config = &cfgCopy
	}
}
