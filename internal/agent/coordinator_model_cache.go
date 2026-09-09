package agent

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/config"
)

// modelPairCacheKey keeps every cache dimension separate. In particular, a
// provider/model name may contain punctuation used by human-readable keys.
type modelPairCacheKey struct {
	generation     uint64
	smartProvider  string
	smartModel     string
	smartReasoning string
	fastProvider   string
	fastModel      string
	fastReasoning  string
}

type boundedModelPairCacheEntry struct {
	pair     cachedModelPair
	lastUsed uint64
}

// boundedModelPairCache is an LRU cache. Its mutex only protects cache data;
// model construction remains outside it so unrelated misses do not serialize.
type boundedModelPairCache struct {
	mu       sync.Mutex
	capacity int
	clock    uint64
	entries  map[modelPairCacheKey]boundedModelPairCacheEntry
}

func newBoundedModelPairCache(capacity int) *boundedModelPairCache {
	return &boundedModelPairCache{
		capacity: capacity,
		entries:  make(map[modelPairCacheKey]boundedModelPairCacheEntry, capacity),
	}
}

func (c *coordinator) buildCachedModelPair(ctx context.Context, cfg *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error) {
	if c.modelPairBuilder != nil {
		return c.modelPairBuilder(ctx, cfg, smartCfg, fastCfg)
	}
	return c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
}

func (c *boundedModelPairCache) get(key modelPairCacheKey) (cachedModelPair, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return cachedModelPair{}, false
	}
	c.clock++
	entry.lastUsed = c.clock
	c.entries[key] = entry
	return entry.pair, true
}

func (c *boundedModelPairCache) set(key modelPairCacheKey, pair cachedModelPair) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clock++
	if c.capacity <= 0 {
		return
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.capacity {
		var oldestKey modelPairCacheKey
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

func (c *boundedModelPairCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[modelPairCacheKey]boundedModelPairCacheEntry, c.capacity)
}

func (c *boundedModelPairCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// legacyModelPairCacheKey lets old csync.Map test fixtures participate while
// production uses modelPairCacheKey directly. JSON is a canonical structural
// representation, not delimiter-based concatenation.
func legacyModelPairCacheKey(key modelPairCacheKey) string {
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

func (c *coordinator) getCachedModelPair(key modelPairCacheKey) (cachedModelPair, bool, uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	epoch := c.modelCacheEpoch
	if c.modelCache == nil {
		return cachedModelPair{}, false, epoch
	}
	if cache, ok := c.modelCache.(*boundedModelPairCache); ok {
		pair, found := cache.get(key)
		return pair, found, epoch
	}
	if cache, ok := c.modelCache.(interface {
		Get(string) (cachedModelPair, bool)
	}); ok {
		pair, found := cache.Get(legacyModelPairCacheKey(key))
		return pair, found, epoch
	}
	return cachedModelPair{}, false, epoch
}

func (c *coordinator) setCachedModelPairIfCurrent(key modelPairCacheKey, pair cachedModelPair, epoch uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	if c.modelCache == nil || c.modelCacheEpoch != epoch {
		return
	}
	if cache, ok := c.modelCache.(*boundedModelPairCache); ok {
		cache.set(key, pair)
		return
	}
	if cache, ok := c.modelCache.(interface {
		Set(string, cachedModelPair)
	}); ok {
		cache.Set(legacyModelPairCacheKey(key), pair)
	}
}
