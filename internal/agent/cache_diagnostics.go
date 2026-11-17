package agent

import (
	"context"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/session"
)

// A plain fantasy.Usage loses field presence. Only the native Codex decoder
// currently forwards typed evidence that this response actually reported zero.
// Profiles/capability and metadata belonging to another provider prove nothing.
func observedCacheReadTokens(usage fantasy.Usage, estimated bool, provider string, metadata fantasy.ProviderMetadata) *int64 {
	if estimated || usage.CacheReadTokens < 0 {
		return nil
	}
	if usage.CacheReadTokens > 0 {
		value := usage.CacheReadTokens
		return &value
	}
	if provider == codexprovider.Name {
		if observed, ok := metadata[codexprovider.Name].(*codexprovider.UsageMetadata); ok && observed != nil && observed.CachedTokens != nil && *observed.CachedTokens == 0 {
			zero := int64(0)
			return &zero
		}
	}
	return nil
}

func (ts *turnStream) logCacheStepUsage(usage fantasy.Usage, estimated bool, metadata fantasy.ProviderMetadata) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	d := ts.cacheDiagnostic
	provider := ""
	if ts.smartModel.Model != nil {
		provider = ts.smartModel.Model.Provider()
	}
	var cachedTokens any
	if observed := observedCacheReadTokens(usage, estimated, provider, metadata); observed != nil {
		cachedTokens = *observed
	}
	slog.Debug("agent: prompt cache observation",
		"session_hash", session.HashID(ts.call.SessionID),
		"provider", ts.smartModel.ModelCfg.Provider,
		"model", ts.smartModel.ModelCfg.Model,
		"prefix_fingerprint", d.PrefixFingerprint,
		"tools_fingerprint", d.ToolsFingerprint,
		"prefix_logical_bytes", d.PrefixBytes,
		"tools_logical_bytes", d.ToolsBytes,
		"message_logical_bytes", d.MessageBytes,
		"tool_count", d.ToolCount,
		"prepare_ms", ts.cachePrepareDuration.Milliseconds(),
		"step_elapsed_ms", time.Since(ts.cachePreparedAt).Milliseconds(),
		"cached_tokens", cachedTokens,
		"estimated", estimated,
		"semantic_invalidation_expectation", "changed prefix or tool definitions may invalidate reuse; counters do not identify the cause",
		"byte_measurement", "logical JSON, not wire bytes or cache savings")
}
