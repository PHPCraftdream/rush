package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/require"
)

func TestModelCatalogCacheRefreshesAfterSevenDaysAndSeparatesCredentials(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	calls := 0
	fetch := func(context.Context) ([]catwalk.Model, error) {
		calls++
		return []catwalk.Model{{ID: "live-model", ReasoningLevels: []string{"low", "high"}}}, nil
	}
	get := func(identity string) []catwalk.Model {
		models, err := cachedProviderModels(t.Context(), "stepfun", "https://api.stepfun.ai/v1", identity, fetch)
		require.NoError(t, err)
		return models
	}

	require.Equal(t, []string{"low", "high"}, get("account-a")[0].ReasoningLevels)
	require.Equal(t, 1, calls)
	get("account-a")
	require.Equal(t, 1, calls, "fresh cache must avoid another request")

	path := modelCatalogPath("stepfun")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var entry cachedModelCatalog
	require.NoError(t, json.Unmarshal(data, &entry))
	entry.FetchedAt = time.Now().Add(-8 * 24 * time.Hour)
	data, err = json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, atomicWriteFile(path, data, 0o600))
	get("account-a")
	require.Equal(t, 2, calls, "expired catalog must refetch instead of being served forever")
	get("account-b")
	require.Equal(t, 3, calls, "a different account must not receive account-a's cached models")
}

// TestCachedProviderModelsHidesSupersededFamilies verifies the cache layer
// applies the same family filter as the discovery sources (#1171, #1173):
// a fresh fetch never caches hidden families, and a catalog cached before
// the filter existed still hides them when served from disk.
func TestCachedProviderModelsHidesSupersededFamilies(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fetch := func(context.Context) ([]catwalk.Model, error) {
		return []catwalk.Model{{ID: "glm-4.6"}, {ID: "glm-5.3"}, {ID: "unrelated"}}, nil
	}

	models, err := cachedProviderModels(t.Context(), "zai", "https://api.z.ai/api", "account-a", fetch)
	require.NoError(t, err)
	require.Equal(t, []string{"glm-5.3", "unrelated"}, modelIDs(models),
		"freshly fetched catalogs must drop the hidden glm families")

	// A cache entry written by an older Rush, still inside its TTL, with
	// hidden and visible models: serving it must filter, not replay.
	path := modelCatalogPath("zai")
	entry := cachedModelCatalog{
		Fingerprint: modelCatalogFingerprint("zai", "https://api.z.ai/api", "account-a"),
		FetchedAt:   time.Now(),
		Models:      []catwalk.Model{{ID: "glm-4.6"}, {ID: "glm-5.3"}},
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, atomicWriteFile(path, data, 0o600))

	fetchCalls := 0
	countingFetch := func(context.Context) ([]catwalk.Model, error) {
		fetchCalls++
		return nil, nil
	}
	models, err = cachedProviderModels(t.Context(), "zai", "https://api.z.ai/api", "account-a", countingFetch)
	require.NoError(t, err)
	require.Zero(t, fetchCalls, "a fresh cache must be served without refetching")
	require.Equal(t, []string{"glm-5.3"}, modelIDs(models),
		"a stale-schema cache must still hide the superseded families when served")
}

// TestCachedProviderModelsServesRawEntriesAndOverlayRaises: the cache layer
// serves catalog entries exactly as the fetching Rush wrote them (a Rush that
// predates the documented GPT-6 window wrote 272000 for every gpt-6 entry,
// valid for seven days). The documented windows are applied once, by the
// facts overlay at config load -- not by the cache.
//
// Revert-check: re-add cache-level normalization (or drop the final
// ApplyModelFacts pass in loadProviders) and the raised rows go red.
func TestCachedProviderModelsServesRawEntriesAndOverlayRaises(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	entry := cachedModelCatalog{
		Fingerprint: modelCatalogFingerprint("openai-codex", "https://chatgpt.com/backend-api/codex", "account-a"),
		FetchedAt:   time.Now(),
		Models: []catwalk.Model{
			{ID: "gpt-6-luna", ContextWindow: 272000, DefaultMaxTokens: 128000},
			{ID: "gpt-6.1-sol", ContextWindow: 272000, DefaultMaxTokens: 128000},
		},
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	path := modelCatalogPath("openai-codex")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, atomicWriteFile(path, data, 0o600))

	fetchCalls := 0
	models, err := cachedProviderModels(t.Context(), "openai-codex", "https://chatgpt.com/backend-api/codex", "account-a",
		func(context.Context) ([]catwalk.Model, error) { fetchCalls++; return nil, nil })
	require.NoError(t, err)
	require.Zero(t, fetchCalls, "a fresh cache must be served without refetching")
	require.Len(t, models, 2)
	for _, model := range models {
		require.EqualValues(t, 272000, model.ContextWindow,
			"the cache must replay exactly what was written; overlay rules are not its business")
	}
	raised := discover.ApplyModelFacts("openai-codex", models, nil)
	for _, model := range raised {
		require.EqualValues(t, 1_050_000, model.ContextWindow, model.ID)
	}
}

// modelIDs extracts model ids in order for the assertions above.
func modelIDs(models []catwalk.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestClearModelCatalogCacheOnlyRemovesRequestedProvider(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, id := range []string{"zai", "zai-docs", "stepfun"} {
		path := modelCatalogPath(id)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(`{"test":true}`), 0o600))
	}
	require.Error(t, ClearModelCatalogCache("unsupported"))
	require.NoError(t, ClearModelCatalogCache("zai"))
	_, err := os.Stat(modelCatalogPath("zai"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(modelCatalogPath("zai-docs"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(modelCatalogPath("stepfun"))
	require.NoError(t, err)
	require.NoError(t, ClearModelCatalogCache("all"))
	_, err = os.Stat(modelCatalogPath("stepfun"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
