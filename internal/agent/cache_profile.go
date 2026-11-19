package agent

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
)

// PromptCacheTTL reports the prompt-cache TTL the cache profiles assume
// for provider — the same figure the keep-alive scheduler derives its
// replay interval from (cacheKeepAliveIntervalFor). Zero means the
// provider has no explicit TTL here (implicit-cache or unknown providers):
// callers must keep expiry unknown, not invent a TTL or read zero as
// "never expires".
//
// Exported deliberately for diagnostic readers outside this package
// (internal/cmd's `rush sessions cache` report): classifying a warm->cold
// transition as "likely TTL expiry" must use the agent's real per-provider
// TTL so the diagnostic cannot silently drift from the policy it reports
// on. A duplicated local table in the reader was rejected on purpose — any
// copy drifts, and even a drift-check test would need this accessor to
// compare against.
//
// The per-provider table itself lives in runtime/policy/provauth; this
// facade keeps the lookup lazy — no eager snapshot of the table.
func PromptCacheTTL(provider string) time.Duration {
	return provauth.PromptCacheTTL(provider)
}
