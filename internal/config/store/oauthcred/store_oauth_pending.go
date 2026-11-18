package oauthcred

import (
	"reflect"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

func (s *Store) oauthTokenProviderConfig(providerID string, pc cfgmodel.ProviderConfig, token *oauth.Token) cfgmodel.ProviderConfig {
	pc.OAuthToken = token
	if s.rotatingOAuthProvider(providerID) {
		pc.APIKey = ""
	} else {
		pc.APIKey = token.AccessToken
	}
	if providerID == string(catwalk.InferenceProviderCopilot) {
		pc.SetupGitHubCopilot()
	}
	return pc
}

func (s *Store) recordPendingOAuthLocked(providerID string, predecessor, token *oauth.Token, records ...*docread.OAuthJournalRecord) {
	if s.pendingOAuth == nil {
		s.pendingOAuth = make(map[string]PendingRotation)
	}
	record := docread.OAuthJournalRecord{Token: token, PredecessorRefreshTokenSHA256: oauthjournal.RotationDigest(predecessor, token)}
	if len(records) > 0 && records[0] != nil {
		record = *records[0]
	}
	var digests []string
	if previous, ok := s.pendingOAuth[providerID]; ok {
		digests = append(digests, previous.Record.PredecessorRefreshTokenSHA256)
		digests = append(digests, previous.Record.BurnedRefreshTokenSHA256...)
	}
	s.port.OAuthJournal().OutboxEachLocked(func(key oauthjournal.Key, owned *oauthjournal.Entry) {
		if key.Provider == providerID {
			digests = append(digests, owned.Record.PredecessorRefreshTokenSHA256)
			digests = append(digests, owned.Record.BurnedRefreshTokenSHA256...)
		}
	})
	digests = append(digests, oauthjournal.RotationDigest(predecessor, token))
	record = oauthjournal.WithBurned(&record, digests...)
	copyToken := *token
	record.Token = &copyToken
	s.pendingOAuth[providerID] = PendingRotation{Predecessor: *predecessor, Token: copyToken, Record: record}
}

func (s *Store) clearPendingOAuth(providerID string, predecessor, token *oauth.Token) {
	s.port.LockPublish()
	defer s.port.UnlockPublish()
	if entry, ok := s.pendingOAuth[providerID]; ok && reflect.DeepEqual(&entry.Predecessor, predecessor) && reflect.DeepEqual(&entry.Token, token) {
		delete(s.pendingOAuth, providerID)
	}
}

func (s *Store) pendingOAuthCurrent(providerID string, predecessor, token *oauth.Token) bool {
	s.port.LockPublish()
	defer s.port.UnlockPublish()
	entry, ok := s.pendingOAuth[providerID]
	if !ok || !reflect.DeepEqual(&entry.Predecessor, predecessor) || !reflect.DeepEqual(&entry.Token, token) {
		return false
	}
	cur := s.port.LoadSnapshot()
	if cur.Config() == nil || cur.Config().Providers == nil {
		return false
	}
	pc, ok := cur.Config().Providers.Get(providerID)
	return ok && reflect.DeepEqual(pc.OAuthToken, token)
}

// Predecessors and earlier-expiring replacements retain the pending rotation.
// The caller holds publishMu (root overlayPendingOAuthLocked contract); cfg is
// the candidate configuration and the (possibly replaced) candidate is
// returned.
func (s *Store) OverlayPendingRotation(cfg *cfgmodel.Config) *cfgmodel.Config {
	for providerID, entry := range s.pendingOAuth {
		var pc cfgmodel.ProviderConfig
		var ok bool
		if cfg != nil && cfg.Providers != nil {
			pc, ok = cfg.Providers.Get(providerID)
		}
		decision := oauthjournal.OAuthJournalLogout
		if ok {
			record := entry.Record
			record.Token = &entry.Token
			decision = oauthjournal.Arbitrate(&record, &entry.Predecessor, pc.OAuthToken, nil)
		}
		if decision != oauthjournal.OAuthJournalPredecessor && decision != oauthjournal.OAuthJournalLater && decision != oauthjournal.OAuthJournalEqual {
			s.port.OAuthJournal().OutboxEachLocked(func(key oauthjournal.Key, owned *oauthjournal.Entry) {
				if key.Provider == providerID && reflect.DeepEqual(owned.Record.Token, &entry.Token) {
					owned.Obsolete = true
				}
			})
			delete(s.pendingOAuth, providerID)
			continue
		}
		cur := s.port.LoadSnapshot()
		if cur.Config() == nil || cur.Config().Providers == nil {
			delete(s.pendingOAuth, providerID)
			continue
		}
		live, exists := cur.Config().Providers.Get(providerID)
		if !exists || !reflect.DeepEqual(live.OAuthToken, &entry.Token) {
			delete(s.pendingOAuth, providerID)
			continue
		}
		token := entry.Token
		cfgCopy := *cfg
		providersCopy := cfg.Providers.Copy()
		providersCopy[providerID] = s.oauthTokenProviderConfig(providerID, pc, &token)
		cfgCopy.Providers = csync.NewMapFrom(providersCopy)
		cfg = &cfgCopy
	}
	return cfg
}
