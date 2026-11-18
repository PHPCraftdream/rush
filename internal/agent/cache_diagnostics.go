package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	session "github.com/PHPCraftdream/rush/internal/session/service"
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
	ts.noteCacheRequest(usage, estimated, metadata)
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

// A request whose provider-reported cache read is below cacheMissRatioPercent of
// its prompt, on a prompt of at least cacheMissMinPromptTokens, is logged once
// as a miss together with what changed since the previous request of the same
// session. The log line answers the one question usage counters cannot: did we
// send a different prefix, or did the provider drop a prefix we kept stable.
const (
	cacheMissMinPromptTokens = 20_000
	cacheMissRatioPercent    = 30
	cacheObsMaxSessions      = 512
)

// cacheRequestObs is what the next request of a session is compared with.
type cacheRequestObs struct {
	prefix  string
	tools   string
	model   string
	digests []string
	at      time.Time
}

var cacheObs = struct {
	mu   sync.Mutex
	last map[string]*cacheRequestObs
}{last: map[string]*cacheRequestObs{}}

// Causes reported by classifyCacheMiss.
const (
	cacheMissFirstRequest   = "first_request_in_process"
	cacheMissModelChanged   = "model_changed"
	cacheMissPrefixChanged  = "system_prefix_changed"
	cacheMissToolsChanged   = "tools_changed"
	cacheMissHistoryChanged = "history_rewritten"
	cacheMissProviderSide   = "prefix_and_history_unchanged_provider_expiry_or_eviction"
)

// commonPrefixLen is the number of leading equal elements of a and b.
func commonPrefixLen(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// classifyCacheMiss names the first reason the request cur could not reuse the
// cache of prev, and the number of leading messages the two share.
func classifyCacheMiss(prev, cur *cacheRequestObs) (cause string, common int) {
	if prev == nil {
		return cacheMissFirstRequest, 0
	}
	common = commonPrefixLen(prev.digests, cur.digests)
	switch {
	case prev.model != cur.model:
		return cacheMissModelChanged, common
	case prev.prefix != cur.prefix:
		return cacheMissPrefixChanged, common
	case prev.tools != cur.tools:
		return cacheMissToolsChanged, common
	case common < len(prev.digests):
		return cacheMissHistoryChanged, common
	}
	return cacheMissProviderSide, common
}

// swapCacheObs stores cur for the session and returns the previous one. The map
// is bounded: past cacheObsMaxSessions the oldest entry goes.
func swapCacheObs(sessionHash string, cur *cacheRequestObs) *cacheRequestObs {
	cacheObs.mu.Lock()
	defer cacheObs.mu.Unlock()
	prev := cacheObs.last[sessionHash]
	cacheObs.last[sessionHash] = cur
	if len(cacheObs.last) > cacheObsMaxSessions {
		oldestKey := ""
		var oldest time.Time
		for k, v := range cacheObs.last {
			if k == sessionHash {
				continue
			}
			if oldestKey == "" || v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		delete(cacheObs.last, oldestKey)
	}
	return prev
}

// noteCacheRequest records this request for the next comparison and logs it when
// the provider reports that it missed the cache.
func (ts *turnStream) noteCacheRequest(usage fantasy.Usage, estimated bool, metadata fantasy.ProviderMetadata) {
	d := ts.cacheDiagnostic
	if d.PrefixFingerprint == "" && len(d.MessageDigests) == 0 {
		return
	}
	model := ts.smartModel.ModelCfg.Provider + "/" + ts.smartModel.ModelCfg.Model
	cur := &cacheRequestObs{prefix: d.PrefixFingerprint, tools: d.ToolsFingerprint, model: model, digests: d.MessageDigests, at: time.Now()}
	sessionHash := session.HashID(ts.call.SessionID)
	prev := swapCacheObs(sessionHash, cur)

	provider := ""
	if ts.smartModel.Model != nil {
		provider = ts.smartModel.Model.Provider()
	}
	observed := observedCacheReadTokens(usage, estimated, provider, metadata)
	if observed == nil {
		return
	}
	prompt := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if prompt < cacheMissMinPromptTokens || *observed*100 >= prompt*cacheMissRatioPercent {
		return
	}
	cause, common := classifyCacheMiss(prev, cur)
	attrs := []any{
		"session_hash", sessionHash,
		"model", model,
		"prompt_tokens", prompt,
		"cached_tokens", *observed,
		"cause", cause,
		"messages", len(cur.digests),
		"common_prefix_messages", common,
	}
	if prev != nil {
		attrs = append(attrs, "idle_s", int64(cur.at.Sub(prev.at).Seconds()), "prev_messages", len(prev.digests))
	}
	slog.Info("agent: prompt cache miss", attrs...)
}
