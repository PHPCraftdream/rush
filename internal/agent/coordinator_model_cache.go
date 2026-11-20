package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/config"
)

func (c *coordinator) buildCachedModelPair(ctx context.Context, cfg *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error) {
	if c.modelPairBuilder != nil {
		return c.modelPairBuilder(ctx, cfg, smartCfg, fastCfg)
	}
	return c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
}

func (c *coordinator) getCachedModelPair(key modelkit.ModelPairCacheKey) (modelkit.CachedModelPair, bool, uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	epoch := c.modelCacheEpoch
	if c.modelCache == nil {
		return modelkit.CachedModelPair{}, false, epoch
	}
	if cache, ok := c.modelCache.(*modelkit.BoundedModelPairCache); ok {
		pair, found := cache.Get(key)
		return pair, found, epoch
	}
	if cache, ok := c.modelCache.(interface {
		Get(string) (modelkit.CachedModelPair, bool)
	}); ok {
		pair, found := cache.Get(modelkit.LegacyModelPairCacheKey(key))
		return pair, found, epoch
	}
	return modelkit.CachedModelPair{}, false, epoch
}

func (c *coordinator) setCachedModelPairIfCurrent(key modelkit.ModelPairCacheKey, pair modelkit.CachedModelPair, epoch uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	if c.modelCache == nil || c.modelCacheEpoch != epoch {
		return
	}
	if cache, ok := c.modelCache.(*modelkit.BoundedModelPairCache); ok {
		cache.Set(key, pair)
		return
	}
	if cache, ok := c.modelCache.(interface {
		Set(string, modelkit.CachedModelPair)
	}); ok {
		cache.Set(modelkit.LegacyModelPairCacheKey(key), pair)
	}
}
