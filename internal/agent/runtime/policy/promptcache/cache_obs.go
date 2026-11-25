package promptcache

import (
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
)

// A plain fantasy.Usage loses field presence. Only the native Codex decoder
// currently forwards typed evidence that this response actually reported zero.
// Profiles/capability and metadata belonging to another provider prove nothing.
func ObservedCacheReadTokens(usage fantasy.Usage, estimated bool, provider string, metadata fantasy.ProviderMetadata) *int64 {
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

// A request whose provider-reported cache read is below cacheMissRatioPercent of
// its prompt, on a prompt of at least cacheMissMinPromptTokens, is logged once
// as a miss together with what changed since the previous request of the same
// session. The log line answers the one question usage counters cannot: did we
// send a different prefix, or did the provider drop a prefix we kept stable.
const (
	CacheMissMinPromptTokens = 20_000
	CacheMissRatioPercent    = 30
	CacheObsMaxSessions      = 512
)

// CacheRequestObs is what the next request of a session is compared with.
type CacheRequestObs struct {
	Prefix  string
	Tools   string
	Model   string
	Digests []string
	At      time.Time
}

var CacheObs = struct {
	mu   sync.Mutex
	last map[string]*CacheRequestObs
}{last: map[string]*CacheRequestObs{}}

// Causes reported by ClassifyCacheMiss.
const (
	CacheMissFirstRequest   = "first_request_in_process"
	CacheMissModelChanged   = "model_changed"
	CacheMissPrefixChanged  = "system_prefix_changed"
	CacheMissToolsChanged   = "tools_changed"
	CacheMissHistoryChanged = "history_rewritten"
	CacheMissProviderSide   = "prefix_and_history_unchanged_provider_expiry_or_eviction"
)

// CommonPrefixLen is the number of leading equal elements of a and b.
func CommonPrefixLen(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// ClassifyCacheMiss names the first reason the request cur could not reuse the
// cache of prev, and the number of leading messages the two share.
func ClassifyCacheMiss(prev, cur *CacheRequestObs) (cause string, common int) {
	if prev == nil {
		return CacheMissFirstRequest, 0
	}
	common = CommonPrefixLen(prev.Digests, cur.Digests)
	switch {
	case prev.Model != cur.Model:
		return CacheMissModelChanged, common
	case prev.Prefix != cur.Prefix:
		return CacheMissPrefixChanged, common
	case prev.Tools != cur.Tools:
		return CacheMissToolsChanged, common
	case common < len(prev.Digests):
		return CacheMissHistoryChanged, common
	}
	return CacheMissProviderSide, common
}

// SwapCacheObs stores cur for the session and returns the previous one. The map
// is bounded: past CacheObsMaxSessions the oldest entry goes.
func SwapCacheObs(sessionHash string, cur *CacheRequestObs) *CacheRequestObs {
	CacheObs.mu.Lock()
	defer CacheObs.mu.Unlock()
	prev := CacheObs.last[sessionHash]
	CacheObs.last[sessionHash] = cur
	if len(CacheObs.last) > CacheObsMaxSessions {
		oldestKey := ""
		var oldest time.Time
		for k, v := range CacheObs.last {
			if k == sessionHash {
				continue
			}
			if oldestKey == "" || v.At.Before(oldest) {
				oldestKey, oldest = k, v.At
			}
		}
		delete(CacheObs.last, oldestKey)
	}
	return prev
}

// ResetCacheObsForTest clears the observation map (test seam).
func ResetCacheObsForTest() {
	CacheObs.mu.Lock()
	CacheObs.last = map[string]*CacheRequestObs{}
	CacheObs.mu.Unlock()
}

// CacheObsSessions reports the number of tracked sessions (test seam).
func CacheObsSessions() int {
	CacheObs.mu.Lock()
	defer CacheObs.mu.Unlock()
	return len(CacheObs.last)
}
