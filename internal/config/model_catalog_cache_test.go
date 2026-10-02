package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
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
