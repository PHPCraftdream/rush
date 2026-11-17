package promptcache

import (
	"time"
)

// cacheReporterProviderIDs lists CONFIGURED provider IDs (the `provider` key
// in rush.json, config.SelectedModel.Provider) confirmed to emit cache
// counters even though their fantasy provider TYPE (openai-compat) cannot be
// trusted: the same wire protocol serves both cache-reporting endpoints
// (e.g. zai's GLM) and local endpoints that never report caching. Adding
// openaicompat.Name to cacheProfiles would fabricate 0% for the silent ones,
// so the classification falls back to this ID list instead. A provider ID not
// listed here keeps the honest CacheSupportNone on zero-counter turns.
var cacheReporterProviderIDs = map[string]bool{
	"zai": true,
}

// IsCacheReporterProviderID reports whether the configured provider ID is
// confirmed to emit cache counters even though its fantasy provider TYPE
// cannot be trusted — see cacheReporterProviderIDs.
func IsCacheReporterProviderID(providerID string) bool {
	return cacheReporterProviderIDs[providerID]
}

// PromptCacheTTL reports the prompt-cache TTL the cache profiles assume
// for provider — the same figure the keep-alive scheduler derives its
// replay interval from (cacheKeepAliveIntervalFor). Zero means the
// provider has no explicit TTL here (implicit-cache or unknown providers):
// callers must keep expiry unknown, not invent a TTL or read zero as
// "never expires".
//
// The ttlFor lookup is injected by the owner of the per-provider cache
// profiles and is evaluated at this function's read point, exactly where
// the profiles used to be read directly.
func PromptCacheTTL(provider string, ttlFor func(string) time.Duration) time.Duration {
	return ttlFor(provider)
}
