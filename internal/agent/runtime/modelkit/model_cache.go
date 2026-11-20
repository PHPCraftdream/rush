package modelkit

import (
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
)

// CachedModelPair holds a resolved (smart, fast) agentcall.Model pair as built
// together by a single buildModelsFromCfg call.
type CachedModelPair struct {
	Smart agentcall.Model
	Fast  agentcall.Model
}

// ModelPairCache is the small cache surface used by model resolution. Keeping
// it narrow preserves compatibility with existing csync.Map fixtures.
type ModelPairCache interface {
	Len() int
}

// ModelPairCacheKey keeps every cache dimension separate. In particular, a
// provider/model name may contain punctuation used by human-readable keys.
type ModelPairCacheKey struct {
	generation     uint64
	smartProvider  string
	smartModel     string
	smartReasoning string
	smartTier      string
	fastProvider   string
	fastModel      string
	fastReasoning  string
	fastTier       string
}

// NewModelPairCacheKey captures the model selection dimensions from one config snapshot.
func NewModelPairCacheKey(generation uint64, smart, fast config.SelectedModel) ModelPairCacheKey {
	return ModelPairCacheKey{
		generation:     generation,
		smartProvider:  smart.Provider,
		smartModel:     smart.Model,
		smartReasoning: smart.ReasoningEffort,
		smartTier:      smart.ServiceTier,
		fastProvider:   fast.Provider,
		fastModel:      fast.Model,
		fastReasoning:  fast.ReasoningEffort,
		fastTier:       fast.ServiceTier,
	}
}

type boundedModelPairCacheEntry struct {
	pair     CachedModelPair
	lastUsed uint64
}

// BoundedModelPairCache is an LRU cache. Its mutex only protects cache data;
// model construction remains outside it so unrelated misses do not serialize.
type BoundedModelPairCache struct {
	mu       sync.Mutex
	capacity int
	clock    uint64
	entries  map[ModelPairCacheKey]boundedModelPairCacheEntry
}

func NewBoundedModelPairCache(capacity int) *BoundedModelPairCache {
	return &BoundedModelPairCache{
		capacity: capacity,
		entries:  make(map[ModelPairCacheKey]boundedModelPairCacheEntry, capacity),
	}
}

func (c *BoundedModelPairCache) Get(key ModelPairCacheKey) (CachedModelPair, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return CachedModelPair{}, false
	}
	c.clock++
	entry.lastUsed = c.clock
	c.entries[key] = entry
	return entry.pair, true
}

func (c *BoundedModelPairCache) Set(key ModelPairCacheKey, pair CachedModelPair) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clock++
	if c.capacity <= 0 {
		return
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.capacity {
		var oldestKey ModelPairCacheKey
		var oldest uint64
		for candidateKey, candidate := range c.entries {
			if oldest == 0 || candidate.lastUsed < oldest {
				oldestKey = candidateKey
				oldest = candidate.lastUsed
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = boundedModelPairCacheEntry{pair: pair, lastUsed: c.clock}
}

func (c *BoundedModelPairCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[ModelPairCacheKey]boundedModelPairCacheEntry, c.capacity)
}

func (c *BoundedModelPairCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// LegacyModelPairCacheKey lets old csync.Map test fixtures participate while
// production uses ModelPairCacheKey directly. JSON is a canonical structural
// representation, not delimiter-based concatenation.
func LegacyModelPairCacheKey(key ModelPairCacheKey) string {
	encoded, err := json.Marshal(struct {
		Generation     uint64 `json:"generation"`
		SmartProvider  string `json:"smart_provider"`
		SmartModel     string `json:"smart_model"`
		SmartReasoning string `json:"smart_reasoning"`
		FastProvider   string `json:"fast_provider"`
		FastModel      string `json:"fast_model"`
		FastReasoning  string `json:"fast_reasoning"`
	}{
		Generation:     key.generation,
		SmartProvider:  key.smartProvider,
		SmartModel:     key.smartModel,
		SmartReasoning: key.smartReasoning,
		FastProvider:   key.fastProvider,
		FastModel:      key.fastModel,
		FastReasoning:  key.fastReasoning,
	})
	if err != nil {
		panic("model pair cache key cannot be encoded")
	}
	return string(encoded)
}
