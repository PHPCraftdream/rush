package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/discover"
)

var modelVerificationTimeout = 5 * time.Second

// modelVerifyOnlineForTests lets command tests exercise live verification while
// the provider catalog stays cache-only (a real catalog fetch must never run).
var modelVerifyOnlineForTests atomic.Bool

func modelVerifyOffline() bool { return providerCacheOnly() && !modelVerifyOnlineForTests.Load() }

// SetModelVerifyOnlineForTests allows verification requests in cache-only mode;
// it returns the restore function.
func SetModelVerifyOnlineForTests(on bool) func() {
	previous := modelVerifyOnlineForTests.Swap(on)
	return func() { modelVerifyOnlineForTests.Store(previous) }
}

type VerifyResult string

const (
	Listed       VerifyResult = "listed"
	NotListed    VerifyResult = "not-listed"
	Unverifiable VerifyResult = "unverifiable"
)

type ConfirmedModels struct {
	Fingerprint string          `json:"fingerprint"`
	FetchedAt   time.Time       `json:"fetched_at"`
	Models      []catwalk.Model `json:"models"`
}

func VerifyProviderModel(ctx context.Context, cfg *Config, providerID, modelID string, resolvers ...VariableResolver) (VerifyResult, error) {
	if cfg == nil || cfg.Providers == nil {
		return Unverifiable, errors.New("provider is not configured")
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return Unverifiable, errors.New("provider is not configured")
	}
	resolver := IdentityResolver()
	if len(resolvers) > 0 && resolvers[0] != nil {
		resolver = resolvers[0]
	}
	if string(pc.Type) == "" {
		pc.Type = "openai-compatible"
	}
	models, reason, err := RefreshProviderModels(ctx, cfg, pc, providerID, resolver, modelID)
	if err != nil {
		return Unverifiable, err
	}
	if reason != "" {
		return Unverifiable, errors.New(reason)
	}
	for _, model := range models {
		if model.ID == modelID {
			return Listed, nil
		}
	}
	return NotListed, nil
}

// RefreshProviderModels fetches the entire model roster, using a fresh cached roster when possible.
func RefreshProviderModels(ctx context.Context, cfg *Config, pc ProviderConfig, providerID string, resolver VariableResolver, modelID string) ([]catwalk.Model, string, error) {
	if modelVerifyOffline() {
		return nil, "provider cache-only mode is enabled", nil
	}
	cached, fresh := CachedConfirmedModels(pc, providerID, resolver)
	if fresh {
		for _, model := range cached {
			if model.ID == modelID {
				return cached, "", nil
			}
		}
	}
	models, reason, err := FetchProviderModels(ctx, cfg, pc, providerID, resolver)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	key, keyErr := resolver.ResolveValue(pc.APIKey)
	endpoint, endpointErr := resolver.ResolveValue(pc.BaseURL)
	if keyErr != nil || endpointErr != nil {
		return models, "", errors.New("provider credentials could not be resolved")
	}
	if err := PersistConfirmedModels(providerID, endpoint, key, models); err != nil {
		return models, "", err
	}
	return models, "", nil
}

// FetchProviderModels fetches a provider's full roster without writing cache.
func FetchProviderModels(ctx context.Context, cfg *Config, pc ProviderConfig, providerID string, resolver VariableResolver) ([]catwalk.Model, string, error) {
	if modelVerifyOffline() {
		return nil, "provider cache-only mode is enabled", nil
	}
	if pc.OAuthToken != nil {
		return nil, "OAuth-only provider cannot be verified", nil
	}
	if string(pc.Type) == "anthropic" && pc.APIKey == "" {
		return nil, "provider has no API key", nil
	}
	if resolver == nil {
		resolver = IdentityResolver()
	}
	key, err := resolver.ResolveValue(pc.APIKey)
	if err != nil {
		return nil, "provider API key could not be resolved", nil
	}
	if key == "" {
		return nil, "provider has no API key", nil
	}
	endpoint, err := resolver.ResolveValue(pc.BaseURL)
	if err != nil || endpoint == "" {
		return nil, "provider has no resolvable endpoint", nil
	}
	var fetch func(context.Context) ([]catwalk.Model, error)
	if !supportsModelVerification(string(pc.Type)) {
		return nil, fmt.Sprintf("provider type %q does not support model verification", pc.Type), nil
	}
	providerClient, err := cfg.ProviderHTTPClient(pc)
	if err != nil {
		return nil, fmt.Sprintf("provider network settings are invalid: %v", err), nil
	}
	switch string(pc.Type) {
	case "anthropic":
		fetch = func(ctx context.Context) ([]catwalk.Model, error) {
			return discover.DiscoverAnthropicModels(ctx, providerClient, endpoint, key)
		}
	case "openai", "openai-compatible", "openai_compat", "openai-compat":
		fetch = func(ctx context.Context) ([]catwalk.Model, error) {
			return discover.DiscoverProviderCatalog(ctx, discover.Config{ID: providerID, BaseURL: endpoint, APIKey: key, HTTPClient: providerClient}, resolver)
		}
	default:
		return nil, fmt.Sprintf("provider type %q does not support model verification", pc.Type), nil
	}
	ctx, cancel := context.WithTimeout(ctx, modelVerificationTimeout)
	defer cancel()
	models, err := fetch(ctx)
	if err != nil {
		return nil, "", err
	}
	return models, "", nil
}

// PersistConfirmedModels persists a successfully fetched full provider roster.
func PersistConfirmedModels(providerID, endpoint, key string, models []catwalk.Model) error {
	fingerprint := modelCatalogFingerprint(providerID, endpoint, key)
	entry := ConfirmedModels{Fingerprint: fingerprint, FetchedAt: time.Now(), Models: models}
	return writeConfirmedModels(providerID, entry)
}

func confirmedModelsPath(provider string) string { return cachePathFor("confirmed-models-" + provider) }

func writeConfirmedModels(provider string, entry ConfirmedModels) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := confirmedModelsPath(provider)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeCacheDocument(path, data, 0o600, atomicWriteFile)
}

func readConfirmedModels(provider, endpoint, identity string) []catwalk.Model {
	data, err := readCacheDocument(confirmedModelsPath(provider))
	if err != nil {
		return nil
	}
	var entry ConfirmedModels
	if json.Unmarshal(data, &entry) != nil || entry.Fingerprint != modelCatalogFingerprint(provider, endpoint, identity) {
		return nil
	}
	return entry.Models
}

// CachedConfirmedModels reports the matching roster and whether it is fresh.
func CachedConfirmedModels(pc ProviderConfig, providerID string, resolver VariableResolver) ([]catwalk.Model, bool) {
	if resolver == nil {
		resolver = IdentityResolver()
	}
	key, err := resolver.ResolveValue(pc.APIKey)
	if err != nil || key == "" {
		return nil, false
	}
	endpoint, err := resolver.ResolveValue(pc.BaseURL)
	if err != nil || endpoint == "" {
		return nil, false
	}
	data, err := readCacheDocument(confirmedModelsPath(providerID))
	if err != nil {
		return nil, false
	}
	var entry ConfirmedModels
	if json.Unmarshal(data, &entry) != nil || entry.Fingerprint != modelCatalogFingerprint(providerID, endpoint, key) {
		return nil, false
	}
	age := time.Since(entry.FetchedAt)
	return entry.Models, age >= 0 && age < modelCatalogTTL
}

func readFreshConfirmedModels(provider, endpoint, identity string) []catwalk.Model {
	data, err := readCacheDocument(confirmedModelsPath(provider))
	if err != nil {
		return nil
	}
	var entry ConfirmedModels
	if json.Unmarshal(data, &entry) != nil || entry.Fingerprint != modelCatalogFingerprint(provider, endpoint, identity) {
		return nil
	}
	age := time.Since(entry.FetchedAt)
	if age < 0 || age >= modelCatalogTTL {
		return nil
	}
	return entry.Models
}

func confirmedModelIDs(models []catwalk.Model) map[string]struct{} {
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		ids[model.ID] = struct{}{}
	}
	return ids
}

func mergeConfirmedCatalogModels(providers []catwalk.Provider, cfg *Config, resolver VariableResolver) []catwalk.Provider {
	if cfg == nil || cfg.Providers == nil {
		return providers
	}
	if resolver == nil {
		resolver = IdentityResolver()
	}
	for i := range providers {
		id := string(providers[i].ID)
		pc, ok := cfg.Providers.Get(id)
		providerType := string(providers[i].Type)
		if ok && pc.Type != "" {
			providerType = string(pc.Type)
		}
		if ok && !supportsModelVerification(providerType) {
			continue
		}
		keyValue := providers[i].APIKey
		endpointValue := string(providers[i].APIEndpoint)
		if ok {
			if pc.APIKey != "" {
				keyValue = pc.APIKey
			}
			if pc.BaseURL != "" {
				endpointValue = pc.BaseURL
			}
		}
		key, keyErr := resolver.ResolveValue(keyValue)
		endpoint, endpointErr := resolver.ResolveValue(endpointValue)
		if keyErr != nil || endpointErr != nil || key == "" || endpoint == "" {
			continue
		}
		providers[i].Models = mergeConfirmedModels(providers[i].Models,
			readFreshConfirmedModels(id, strings.TrimSpace(endpoint), key))
	}
	return providers
}

func mergeConfirmedModels(catalog, confirmed []catwalk.Model) []catwalk.Model {
	seen := confirmedModelIDs(catalog)
	for _, model := range confirmed {
		if _, exists := seen[model.ID]; exists {
			continue
		}
		catalog = append(catalog, model)
		seen[model.ID] = struct{}{}
	}
	return catalog
}

func supportsModelVerification(providerType string) bool {
	switch providerType {
	case "anthropic", "openai", "openai-compatible", "openai_compat", "openai-compat":
		return true
	default:
		return false
	}
}
