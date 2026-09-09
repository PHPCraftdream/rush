package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

func TestResolveSessionModels_ModelCacheIsBoundedAndEvictsPairs(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coord.modelCache = newBoundedModelPairCache(modelCacheMaxEntries)

	defaultSession, err := env.sessions.Create(t.Context(), "cache default")
	require.NoError(t, err)
	first, err := coord.resolveSessionModelsInternal(t.Context(), defaultSession.ID, false)
	require.NoError(t, err)

	for i := 0; i < modelCacheMaxEntries; i++ {
		sess, createErr := env.sessions.Create(t.Context(), fmt.Sprintf("cache override %d", i))
		require.NoError(t, createErr)
		require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
			&session.ModelSlotUpdate{Provider: "smart-provider", Model: fmt.Sprintf("smart-override-%d", i)},
			&session.ModelSlotUpdate{Provider: "fast-provider", Model: fmt.Sprintf("fast-override-%d", i)}))
		resolved, resolveErr := coord.resolveSessionModelsInternal(t.Context(), sess.ID, false)
		require.NoError(t, resolveErr)
		require.Equal(t, fmt.Sprintf("smart-override-%d", i), resolved.smart.ModelCfg.Model)
		require.Equal(t, fmt.Sprintf("fast-override-%d", i), resolved.fast.ModelCfg.Model)
	}

	require.Equal(t, modelCacheMaxEntries, coord.modelCache.Len())
	rebuilt, err := coord.resolveSessionModelsInternal(t.Context(), defaultSession.ID, false)
	require.NoError(t, err)
	require.NotEqual(t, first.smart.Model, rebuilt.smart.Model, "the evicted pair must be rebuilt")
	require.Equal(t, "smart-model", rebuilt.smart.ModelCfg.Model)
	require.Equal(t, "fast-model", rebuilt.fast.ModelCfg.Model)
	require.LessOrEqual(t, coord.modelCache.Len(), modelCacheMaxEntries)
}

func TestResolveSessionModels_ModelCacheSeparatesPunctuationTuples(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coord.modelCache = newBoundedModelPairCache(modelCacheMaxEntries)

	coord.cfg.Config().Providers.Set("pair", config.ProviderConfig{
		ID: "pair", Type: openai.Name, Models: []catwalk.Model{{ID: "one:two"}},
	})
	coord.cfg.Config().Providers.Set("pair:one", config.ProviderConfig{
		ID: "pair:one", Type: openai.Name, Models: []catwalk.Model{{ID: "two"}},
	})

	firstSession, err := env.sessions.Create(t.Context(), "punctuation A")
	require.NoError(t, err)
	secondSession, err := env.sessions.Create(t.Context(), "punctuation B")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateModels(t.Context(), firstSession.ID,
		&session.ModelSlotUpdate{Provider: "pair", Model: "one:two"}, nil))
	require.NoError(t, env.sessions.UpdateModels(t.Context(), secondSession.ID,
		&session.ModelSlotUpdate{Provider: "pair:one", Model: "two"}, nil))

	first, err := coord.resolveSessionModelsInternal(t.Context(), firstSession.ID, false)
	require.NoError(t, err)
	second, err := coord.resolveSessionModelsInternal(t.Context(), secondSession.ID, false)
	require.NoError(t, err)

	require.Equal(t, "pair", first.smart.ModelCfg.Provider)
	require.Equal(t, "one:two", first.smart.ModelCfg.Model)
	require.Equal(t, "pair:one", second.smart.ModelCfg.Provider)
	require.Equal(t, "two", second.smart.ModelCfg.Model)
	require.Equal(t, 2, coord.modelCache.Len())
}

func TestResolveSessionModels_ClearCannotPublishInFlightBuild(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coord.modelCache = newBoundedModelPairCache(modelCacheMaxEntries)
	sess, err := env.sessions.Create(t.Context(), "clear in flight")
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	var builds atomic.Int32
	coord.modelPairBuilder = func(_ context.Context, _ *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error) {
		if builds.Add(1) == 1 {
			close(started)
			<-release
		}
		return Model{ModelCfg: smartCfg}, Model{ModelCfg: fastCfg}, nil
	}

	result := make(chan error, 1)
	go func() {
		_, resolveErr := coord.resolveSessionModelsInternal(t.Context(), sess.ID, false)
		result <- resolveErr
	}()
	<-started
	coord.clearModelCache()
	require.Equal(t, 0, coord.modelCache.Len())
	close(release)
	require.NoError(t, <-result)
	require.Equal(t, 0, coord.modelCache.Len(), "a build started before clear must not resurrect its pair")

	_, err = coord.resolveSessionModelsInternal(t.Context(), sess.ID, false)
	require.NoError(t, err)
	require.Equal(t, int32(2), builds.Load(), "the next resolve must rebuild after clear")
	require.Equal(t, 1, coord.modelCache.Len())
}

func TestCoordinatorModelCache_ConcurrentClearAndPublishStaysBounded(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coord.modelCache = newBoundedModelPairCache(modelCacheMaxEntries)

	const workers = modelCacheMaxEntries + 1
	ready := make(chan struct{}, workers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		key := modelPairCacheKey{generation: uint64(i), smartModel: fmt.Sprintf("smart-%d", i)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, epoch := coord.getCachedModelPair(key)
			ready <- struct{}{}
			<-release
			coord.setCachedModelPairIfCurrent(key, cachedModelPair{}, epoch)
		}()
	}
	for i := 0; i < workers; i++ {
		<-ready
	}
	coord.clearModelCache()
	close(release)
	wg.Wait()

	require.Equal(t, 0, coord.modelCache.Len())
	for i := 0; i < workers; i++ {
		key := modelPairCacheKey{generation: uint64(i), smartModel: fmt.Sprintf("fresh-%d", i)}
		coord.setCachedModelPairIfCurrent(key, cachedModelPair{}, coord.modelCacheEpoch)
	}
	require.Equal(t, modelCacheMaxEntries, coord.modelCache.Len())
}
