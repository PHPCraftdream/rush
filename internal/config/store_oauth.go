// Provider credential management: setting and removing API keys, runtime
// (in-memory-only) provider config updates, OAuth token refresh with
// cross-session disk recovery, and the GitHub Copilot import path.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/oauth/hyper"
	"github.com/tidwall/gjson"
)

// SetProviderAPIKey sets the API key for a provider and persists it.
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	var providerConfig ProviderConfig
	var exists bool
	var setKeyOrToken func()

	switch v := apiKey.(type) {
	case string:
		if err := s.SetConfigField(scope, fmt.Sprintf("providers.%s.api_key", providerID), v); err != nil {
			return fmt.Errorf("failed to save api key to config file: %w", err)
		}
		setKeyOrToken = func() { providerConfig.APIKey = v }
	case *oauth.Token:
		if err := s.SetConfigFields(scope, map[string]any{
			fmt.Sprintf("providers.%s.api_key", providerID): v.AccessToken,
			fmt.Sprintf("providers.%s.oauth", providerID):   v,
		}); err != nil {
			return err
		}
		setKeyOrToken = func() {
			providerConfig.APIKey = v.AccessToken
			providerConfig.OAuthToken = v
			switch providerID {
			case string(catwalk.InferenceProviderCopilot):
				providerConfig.SetupGitHubCopilot()
			}
		}
	}

	// publishMu is held for the whole read-modify-publish cycle below, and
	// the update is published as a genuinely new *Config/generation (via
	// publishLocked) rather than mutated in place on the currently-published
	// snapshot's Providers map. Mutating sn.config.Providers directly (the
	// old behavior here) is memory-safe on its own (Providers has its own
	// RWMutex) but breaks the immutable-snapshot contract: it silently
	// changes what any snapshot captured earlier (e.g. via Snapshot(), or a
	// *Config a concurrent reader is mid-read on) sees for providerID, with
	// no generation bump and no cache invalidation — exactly the torn-read
	// hazard Snapshot() exists to prevent. This mirrors the bug fixed in
	// SetProviderRuntimeConfig (task #341, P1-3) and RemoveProviderAPIKey
	// (task #437, P1-2); SetProviderAPIKey itself was missed by that pass.
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	sn := s.loadSnapshot()
	providerConfig, exists = sn.config.Providers.Get(providerID)
	if !exists {
		var foundProvider *catwalk.Provider
		for _, p := range sn.knownProviders {
			if string(p.ID) == providerID {
				foundProvider = &p
				break
			}
		}
		if foundProvider == nil {
			return fmt.Errorf("provider with ID %s not found in known providers", providerID)
		}
		providerConfig = ProviderConfig{
			ID:           providerID,
			Name:         foundProvider.Name,
			BaseURL:      foundProvider.APIEndpoint,
			Type:         foundProvider.Type,
			Disable:      false,
			ExtraHeaders: make(map[string]string),
			ExtraParams:  make(map[string]string),
			Models:       foundProvider.Models,
		}
	}
	setKeyOrToken()

	next := sn.clone()
	var cfgCopy Config
	if sn.config != nil {
		cfgCopy = *sn.config
	}
	var providersCopy map[string]ProviderConfig
	if sn.config != nil && sn.config.Providers != nil {
		providersCopy = sn.config.Providers.Copy()
	} else {
		providersCopy = make(map[string]ProviderConfig)
	}
	newProviders := csync.NewMapFrom(providersCopy)
	newProviders.Set(providerID, providerConfig)
	cfgCopy.Providers = newProviders
	next.config = &cfgCopy

	s.publishLocked(next)
	return nil
}

// SetProviderRuntimeConfig updates a provider's config and increments the
// generation, publishing a new snapshot. It is for in-memory-only provider
// updates that are NOT persisted to disk — e.g. re-resolving an API key
// template after a 401 error in the coordinator. A subsequent reload will
// rebuild Providers from disk (re-resolving the template), discarding this
// change by design.
//
// publishMu is held so no concurrent reload can swap the Providers
// *csync.Map between loadSnapshot() and .Set(). Each reload creates a brand
// new *csync.Map (confirmed: loadFromBytes → json.Unmarshal calls
// Map.UnmarshalJSON which allocates a fresh inner map; setDefaults creates a
// fresh NewMap if unmarshal left it nil), so without this lock the .Set()
// could land in an already-orphaned map that no reader sees.
//
// Unlike the old implementation that mutated in-place without incrementing
// generation, this now publishes a new snapshot with an incremented generation,
// ensuring the cache key contract is honored (task #341, P1-3).
func (s *ConfigStore) SetProviderRuntimeConfig(providerID string, pc ProviderConfig) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	next := cur.clone()

	// clone() is a SHALLOW copy of the snapshot struct — next.config is
	// still the SAME *Config pointer cur.config is, so mutating its
	// Providers map in place (the old behavior here) would also mutate
	// what any older, already-loaded snapshot sees, defeating the whole
	// point of a "new snapshot, new generation". Build a genuinely
	// independent *Config with its own Providers map instead: copy the
	// struct by value, then replace Providers with a fresh csync.Map
	// seeded from the old one's contents plus this update, so the old
	// snapshot's Providers map is never touched.
	var cfgCopy Config
	if cur.config != nil {
		cfgCopy = *cur.config
	}
	var providersCopy map[string]ProviderConfig
	if cur.config != nil && cur.config.Providers != nil {
		providersCopy = cur.config.Providers.Copy()
	} else {
		providersCopy = make(map[string]ProviderConfig)
	}
	newProviders := csync.NewMapFrom(providersCopy)
	newProviders.Set(providerID, pc)
	cfgCopy.Providers = newProviders
	next.config = &cfgCopy

	s.publishLocked(next)
}

// copilotRefreshTokenFn and hyperExchangeTokenFn indirect the two external
// OAuth refresh calls used by RefreshOAuthToken below. They default to the
// real network-calling implementations; tests override them (package-private,
// restored via t.Cleanup) to simulate a slow refresh call — e.g. one that
// blocks until a concurrent ReloadFromDisk has published a new generation —
// without making a real network call or depending on hyper.BaseURL()'s
// process-wide sync.OnceValue memoization (see the caveat on
// TestProviders_ConcurrentErrorCollection_NotLost in provider_test.go for why
// that value cannot be safely redirected per-test).
var (
	copilotRefreshTokenFn = copilot.RefreshToken
	hyperExchangeTokenFn  = hyper.ExchangeToken
)

// RefreshOAuthToken refreshes the OAuth token for the given provider.
// Before making an external refresh request, it checks the config file on
// disk to see if another Rush session has already refreshed the token. If
// a newer, unexpired token is found, it is used instead of refreshing. If
// the exchange fails (e.g. because another session already rotated the
// refresh token), the disk is re-checked to recover the other session's
// token.
func (s *ConfigStore) RefreshOAuthToken(ctx context.Context, scope Scope, providerID string) error {
	providers := s.loadSnapshot().config.Providers
	providerConfig, exists := providers.Get(providerID)
	if !exists {
		return fmt.Errorf("provider %s not found", providerID)
	}

	if providerConfig.OAuthToken == nil {
		return fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}

	// Check if another session refreshed the token recently by reading
	// the current token from the config file on disk.
	newToken, err := s.loadTokenFromDisk(scope, providerID)
	if err != nil {
		slog.Warn("Failed to read token from config file, proceeding with refresh", "provider", providerID, "error", err)
	} else if newToken != nil && !newToken.IsExpired() && newToken.AccessToken != providerConfig.OAuthToken.AccessToken {
		slog.Info("Using token refreshed by another session", "provider", providerID)
		return s.applyToken(providerConfig, newToken, providerID)
	}

	var refreshedToken *oauth.Token
	var refreshErr error
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		refreshedToken, refreshErr = copilotRefreshTokenFn(ctx, providerConfig.OAuthToken.RefreshToken)
	case hyperp.Name:
		refreshedToken, refreshErr = hyperExchangeTokenFn(ctx, providerConfig.OAuthToken.RefreshToken)
	default:
		return fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
	if refreshErr != nil {
		// The exchange may have failed because another session already
		// rotated the refresh token. Re-read the config file and use the
		// other session's token if available.
		if diskToken, diskErr := s.loadTokenFromDisk(scope, providerID); diskErr == nil &&
			diskToken != nil &&
			!diskToken.IsExpired() &&
			diskToken.AccessToken != providerConfig.OAuthToken.AccessToken {
			slog.Info("Using token refreshed by another session after exchange failure", "provider", providerID)
			return s.applyToken(providerConfig, diskToken, providerID)
		}
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}

	slog.Info("Successfully refreshed OAuth token", "provider", providerID)
	providerConfig.OAuthToken = refreshedToken
	providerConfig.APIKey = refreshedToken.AccessToken

	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		providerConfig.SetupGitHubCopilot()
	}

	// Use SetProviderRuntimeConfig to publish a new snapshot with incremented
	// generation, ensuring cache invalidation works correctly (task #341, P1-3).
	s.SetProviderRuntimeConfig(providerID, providerConfig)

	if err := s.SetConfigFields(scope, map[string]any{
		fmt.Sprintf("providers.%s.api_key", providerID): refreshedToken.AccessToken,
		fmt.Sprintf("providers.%s.oauth", providerID):   refreshedToken,
	}); err != nil {
		return fmt.Errorf("failed to persist refreshed token: %w", err)
	}

	return nil
}

// applyToken updates the in-memory provider config with the given token
// and publishes a new snapshot with incremented generation (task #341, P1-3).
func (s *ConfigStore) applyToken(providerConfig ProviderConfig, token *oauth.Token, providerID string) error {
	providerConfig.OAuthToken = token
	providerConfig.APIKey = token.AccessToken
	if providerID == string(catwalk.InferenceProviderCopilot) {
		providerConfig.SetupGitHubCopilot()
	}
	s.SetProviderRuntimeConfig(providerID, providerConfig)
	return nil
}

// loadTokenFromDisk reads the OAuth token for the given provider from the
// config file on disk. Returns nil if the token is not found or matches the
// current in-memory token.
func (s *ConfigStore) loadTokenFromDisk(scope Scope, providerID string) (*oauth.Token, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	oauthKey := fmt.Sprintf("providers.%s.oauth", providerID)
	oauthResult := gjson.Get(string(data), oauthKey)
	if !oauthResult.Exists() {
		return nil, nil
	}

	var token oauth.Token
	if err := json.Unmarshal([]byte(oauthResult.Raw), &token); err != nil {
		return nil, err
	}

	if token.AccessToken == "" {
		return nil, nil
	}

	return &token, nil
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	if s.HasConfigField(ScopeGlobal, "providers.copilot.api_key") || s.HasConfigField(ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	if !hasDiskToken {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := copilot.RefreshToken(refreshCtx, diskToken)
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}

	if err := s.SetProviderAPIKey(ScopeGlobal, string(catwalk.InferenceProviderCopilot), token); err != nil {
		return token, false
	}

	if err := s.SetConfigFields(ScopeGlobal, map[string]any{
		"providers.copilot.api_key": token.AccessToken,
		"providers.copilot.oauth":   token,
	}); err != nil {
		slog.Error("Unable to save GitHub Copilot token to disk", "error", err)
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}

// RemoveProviderAPIKey removes the API key for the given provider from disk
// and removes it from the in-memory enabled providers list, publishing a new
// snapshot with an incremented generation.
//
// Providers being a *csync.Map (its own internal RWMutex) only makes a
// direct Del call MEMORY-safe — it does nothing to preserve the logical
// immutability of an already-published snapshot. RemoveConfigField above
// reloads from disk and publishes its own new snapshot; calling
// loadSnapshot().config.Providers.Del(providerID) straight afterwards would
// mutate that freshly published snapshot's Providers map in place, with no
// generation bump and no cache invalidation, and would also retroactively
// change what any snapshot captured earlier (e.g. via Snapshot()) sees for
// providerID — exactly the torn-read hazard Snapshot() exists to prevent.
// This mirrors the bug fixed in SetProviderRuntimeConfig (task #341, P1-3):
// build an independent *Config with its own fresh Providers map (via
// csync.NewMapFrom + Copy, NOT storeSnapshot.clone() alone — clone() is a
// SHALLOW copy whose .config still points at the SAME *Config the current
// snapshot uses) and publish it under publishMu so the old snapshot's
// Providers map is never touched.
func (s *ConfigStore) RemoveProviderAPIKey(scope Scope, providerID string) error {
	if err := s.RemoveConfigField(scope, fmt.Sprintf("providers.%s.api_key", providerID)); err != nil {
		return fmt.Errorf("failed to remove provider API key: %w", err)
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	next := cur.clone()

	var cfgCopy Config
	if cur.config != nil {
		cfgCopy = *cur.config
	}
	var providersCopy map[string]ProviderConfig
	if cur.config != nil && cur.config.Providers != nil {
		providersCopy = cur.config.Providers.Copy()
	} else {
		providersCopy = make(map[string]ProviderConfig)
	}
	newProviders := csync.NewMapFrom(providersCopy)
	newProviders.Del(providerID)
	cfgCopy.Providers = newProviders
	next.config = &cfgCopy

	s.publishLocked(next)
	return nil
}
