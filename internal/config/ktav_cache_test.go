// Ktav-capable cache layer tests: provider/model-catalog local cache files
// follow the two-generation Ktav naming, gated by ktavAutoMigrate.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// ktavCacheFixtureJSON is a valid custom provider with numeric/string
// lexemes that stress lossless Ktav conversion fidelity.
const ktavCacheFixtureJSON = `[{"id":"ktav-p","name":"Ktav Provider","api_endpoint":"https://ktav.example/v1","models":[{"id":"m-1","name":"Model One","cost_per_1m_in":0.50,"cost_per_1m_out":1.5,"context_window":128000,"default_max_tokens":8192,"options":{"provider_options":{"ratio":0.50,"big":9007199254740993,"looks_num":"123","looks_bool":"true"}}}]}]`

const ktavHyperFixtureJSON = `{"id":"ktav-h","name":"Hyper Ktav","api_endpoint":"https://h.example/v1","models":[{"id":"hm-1","name":"Hyper Model","cost_per_1m_in":0.50,"cost_per_1m_out":1.5,"context_window":128000,"default_max_tokens":8192}]}`

// isolateCacheTestEnv points every cache location resolver at one canonical
// temp directory so tests never touch a real user's caches.
func isolateCacheTestEnv(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(dir, "global-data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(dir, "global-config"))
	return filepath.Join(dir, "data")
}

// setKtavAutoMigrateForCacheTest flips the gate and restores it on cleanup.
func setKtavAutoMigrateForCacheTest(t *testing.T, on bool) {
	t.Helper()
	saved := ktavAutoMigrate
	ktavAutoMigrate = on
	t.Cleanup(func() { ktavAutoMigrate = saved })
}

func TestCacheKtavReadPrecedence(t *testing.T) {
	fixture := []catwalk.Provider{}
	require.NoError(t, json.Unmarshal([]byte(ktavCacheFixtureJSON), &fixture))

	tests := []struct {
		name      string
		setup     func(t *testing.T, dir string)
		providers []catwalk.Provider
		invalid   bool
	}{
		{
			name: "ktav only",
			setup: func(t *testing.T, dir string) {
				ktav, err := ktavcodec.ConvertJSONToKtav([]byte(ktavCacheFixtureJSON))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "providers.ktav"), ktav, 0o644))
			},
			providers: fixture,
		},
		{
			name: "json only",
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "providers.json"), []byte(ktavCacheFixtureJSON), 0o644))
			},
			providers: fixture,
		},
		{
			name: "both prefers ktav",
			setup: func(t *testing.T, dir string) {
				ktavDoc := []byte(`[{"id":"ktav-p","name":"Ktav Provider","api_endpoint":"https://ktav.example/v1","models":[]}]`)
				ktav, err := ktavcodec.ConvertJSONToKtav(ktavDoc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "providers.ktav"), ktav, 0o644))
				jsonDoc := []byte(`[{"id":"json-p","name":"JSON Provider","api_endpoint":"https://json.example/v1","models":[]}]`)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "providers.json"), jsonDoc, 0o644))
			},
			providers: []catwalk.Provider{{ID: "ktav-p"}},
		},
		{
			name: "invalid ktav is an error",
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "providers.ktav"), []byte("not ktav {{{"), 0o644))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, "providers.json"), []byte(ktavCacheFixtureJSON), 0o644))
			},
			invalid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(dir, 0o755))
			tt.setup(t, dir)
			providers, etagValue, err := newCache[[]catwalk.Provider](
				filepath.Join(dir, "providers.json")).Get()
			if tt.invalid {
				require.Error(t, err)
				require.Contains(t, err.Error(), "invalid Ktav")
				require.Empty(t, providers)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, etagValue)
			require.Len(t, providers, len(tt.providers))
			require.Equal(t, string(tt.providers[0].ID), string(providers[0].ID))
		})
	}

	t.Run("backup only is a miss", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "providers.json.bak"), []byte(ktavCacheFixtureJSON), 0o644))
		_, _, err := newCache[[]catwalk.Provider](
			filepath.Join(dir, "providers.json")).Get()
		require.Error(t, err)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
}

func TestCacheStoreFormatFollowsGate(t *testing.T) {
	var fixture []catwalk.Provider
	require.NoError(t, json.Unmarshal([]byte(ktavCacheFixtureJSON), &fixture))
	data, err := json.Marshal(fixture)
	require.NoError(t, err)

	t.Run("gate off writes json", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "providers.json")
		require.NoError(t, newCache[[]catwalk.Provider](path).Store(fixture))
		_, err := os.Stat(filepath.Join(dir, "providers.ktav"))
		require.ErrorIs(t, err, os.ErrNotExist)
		written, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, data, written)
	})

	t.Run("gate on writes ktav", func(t *testing.T) {
		setKtavAutoMigrateForCacheTest(t, true)
		dir := t.TempDir()
		path := filepath.Join(dir, "providers.json")
		require.NoError(t, newCache[[]catwalk.Provider](path).Store(fixture))
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		raw, err := os.ReadFile(filepath.Join(dir, "providers.ktav"))
		require.NoError(t, err)
		tree, err := ktavcodec.DecodeKtav(raw)
		require.NoError(t, err)
		want, err := ktavcodec.ParseJSONLossless(data)
		require.NoError(t, err)
		require.NoError(t, ktavcodec.Equivalent(want, tree))
	})
}

func TestCacheKtavTypedDecodeMatchesJSON(t *testing.T) {
	var want []catwalk.Provider
	require.NoError(t, json.Unmarshal([]byte(ktavCacheFixtureJSON), &want))

	jsonDir := t.TempDir()
	jsonPath := filepath.Join(jsonDir, "providers.json")
	require.NoError(t, os.WriteFile(jsonPath, []byte(ktavCacheFixtureJSON), 0o644))
	fromJSON, _, err := newCache[[]catwalk.Provider](jsonPath).Get()
	require.NoError(t, err)

	ktavDir := t.TempDir()
	ktavPath := filepath.Join(ktavDir, "providers.json")
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(ktavCacheFixtureJSON))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ktavSiblingOf(ktavPath), ktav, 0o644))
	fromKtav, _, err := newCache[[]catwalk.Provider](ktavPath).Get()
	require.NoError(t, err)

	require.Equal(t, want, fromJSON)
	require.Equal(t, want, fromKtav)

	decoded, err := readCacheDocument(ktavPath)
	require.NoError(t, err)
	original, err := ktavcodec.ParseJSONLossless([]byte(ktavCacheFixtureJSON))
	require.NoError(t, err)
	reread, err := ktavcodec.ParseJSONLossless(decoded)
	require.NoError(t, err)
	require.NoError(t, ktavcodec.Equivalent(original, reread),
		"string/number lexemes must be preserved through the cache read")
}

func TestModelCatalogKtavFreshnessUsesStoredTimestamp(t *testing.T) {
	dataDir := isolateCacheTestEnv(t)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	original := time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	entry := cachedModelCatalog{
		Fingerprint: modelCatalogFingerprint("stepfun", "https://api.stepfun.ai/v1", "acct"),
		FetchedAt:   original,
		Models:      []catwalk.Model{{ID: "live-model"}},
	}

	// Round-trip: FetchedAt must survive JSON -> Ktav -> JSON unchanged.
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	ktav, err := ktavcodec.ConvertJSONToKtav(data)
	require.NoError(t, err)
	tree, err := ktavcodec.DecodeKtav(ktav)
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	var decodedEntry cachedModelCatalog
	require.NoError(t, json.Unmarshal(jsonBytes, &decodedEntry))
	require.True(t, original.Equal(decodedEntry.FetchedAt),
		"FetchedAt must round-trip through Ktav unchanged")

	ktavPath := ktavSiblingOf(modelCatalogPath("stepfun"))
	writeKtavEntry := func(fetchedAt time.Time) {
		t.Helper()
		entry.FetchedAt = fetchedAt
		data, err := json.Marshal(entry)
		require.NoError(t, err)
		ktav, err := ktavcodec.ConvertJSONToKtav(data)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(ktavPath), 0o700))
		require.NoError(t, atomicWriteFile(ktavPath, ktav, 0o600))
	}

	calls := 0
	fetch := func(context.Context) ([]catwalk.Model, error) {
		calls++
		return []catwalk.Model{{ID: "fetched-model"}}, nil
	}

	// A fresh Ktav catalog is served without a fetch.
	writeKtavEntry(time.Now())
	models, err := cachedProviderModels(
		t.Context(), "stepfun", "https://api.stepfun.ai/v1", "acct", fetch)
	require.NoError(t, err)
	require.Equal(t, 0, calls, "a fresh Ktav catalog must be served without fetching")
	require.Equal(t, []string{"live-model"}, modelIDs(models))

	// An 8-day-old Ktav catalog is stale and triggers a refetch. The
	// refetch rewrites the fresh JSON catalog, so drop it to keep the
	// Ktav sibling authoritative for the next step.
	writeKtavEntry(time.Now().Add(-8 * 24 * time.Hour))
	_, err = cachedProviderModels(
		t.Context(), "stepfun", "https://api.stepfun.ai/v1", "acct", fetch)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "an expired Ktav catalog must be refetched")
	require.NoError(t, os.Remove(modelCatalogPath("stepfun")))

	// Touching the stale Ktav file's mtime does not make it fresh.
	now := time.Now()
	require.NoError(t, os.Chtimes(ktavPath, now, now))
	_, err = cachedProviderModels(
		t.Context(), "stepfun", "https://api.stepfun.ai/v1", "acct", fetch)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "mtime must never refresh a stale stored timestamp")
}

func TestClearModelCatalogCacheRemovesBothGenerations(t *testing.T) {
	isolateCacheTestEnv(t)
	for _, id := range []string{"zai", "zai-docs", "stepfun"} {
		path := modelCatalogPath(id)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(`{"test":true}`), 0o600))
		require.NoError(t, os.WriteFile(
			ktavSiblingOf(path), []byte("ktav bytes"), 0o600))
	}
	require.Error(t, ClearModelCatalogCache("unsupported"))

	require.NoError(t, ClearModelCatalogCache("zai"))
	for _, id := range []string{"zai", "zai-docs"} {
		_, err := os.Stat(modelCatalogPath(id))
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Stat(ktavSiblingOf(modelCatalogPath(id)))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	_, err := os.Stat(modelCatalogPath("stepfun"))
	require.NoError(t, err)

	require.NoError(t, ClearModelCatalogCache("all"))
	_, err = os.Stat(modelCatalogPath("stepfun"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(ktavSiblingOf(modelCatalogPath("stepfun")))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, clearModelCatalog("stepfun"),
		"clearing an id with no files must be a no-op success")
}

func TestUpdateProvidersAcceptsKtavFile(t *testing.T) {
	isolateCacheTestEnv(t)
	input := filepath.Join(t.TempDir(), "providers-input.ktav")
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(ktavCacheFixtureJSON))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, ktav, 0o644))

	require.NoError(t, UpdateProviders(input))
	providers, _, err := newCache[[]catwalk.Provider](cachePathFor("providers")).Get()
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "ktav-p", string(providers[0].ID))
	require.Equal(t, "m-1", providers[0].Models[0].ID)
}

func TestUpdateHyperAcceptsKtavFile(t *testing.T) {
	isolateCacheTestEnv(t)
	input := filepath.Join(t.TempDir(), "hyper-input.ktav")
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(ktavHyperFixtureJSON))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, ktav, 0o644))

	require.NoError(t, UpdateHyper(input))
	provider, _, err := newCache[catwalk.Provider](cachePathFor("hyper")).Get()
	require.NoError(t, err)
	require.Equal(t, "ktav-h", string(provider.ID))
	require.Equal(t, "hm-1", provider.Models[0].ID)
}

func TestWriteCacheDocumentJSONWriterSeam(t *testing.T) {
	t.Run("gate off delegates to caller's writer", func(t *testing.T) {
		setKtavAutoMigrateForCacheTest(t, false)
		dir := t.TempDir()
		legacy := filepath.Join(dir, "providers.json")
		var calls [][]any
		sentinel := errors.New("writer seam boom")
		writer := func(path string, data []byte, perm os.FileMode) error {
			calls = append(calls, []any{path, append([]byte(nil), data...), perm})
			return sentinel
		}
		err := writeCacheDocument(legacy, []byte("data"), 0o600, writer)
		require.ErrorIs(t, err, sentinel)
		require.Len(t, calls, 1)
		require.Equal(t, legacy, calls[0][0])
		require.Equal(t, []byte("data"), calls[0][1])
		require.Equal(t, os.FileMode(0o600), calls[0][2])
		_, statErr := os.Stat(legacy)
		require.ErrorIs(t, statErr, fs.ErrNotExist)
	})

	t.Run("gate on writes ktav sibling via atomicWriteFile", func(t *testing.T) {
		setKtavAutoMigrateForCacheTest(t, true)
		dir := t.TempDir()
		legacy := filepath.Join(dir, "providers.json")
		poison := func(string, []byte, os.FileMode) error {
			return errors.New("poison writer must not be called")
		}
		require.NoError(t, writeCacheDocument(legacy, []byte(ktavHyperFixtureJSON), 0o600, poison))
		raw, err := os.ReadFile(ktavSiblingOf(legacy))
		require.NoError(t, err)
		tree, err := ktavcodec.DecodeKtav(raw)
		require.NoError(t, err)
		want, err := ktavcodec.ParseJSONLossless([]byte(ktavHyperFixtureJSON))
		require.NoError(t, err)
		require.NoError(t, ktavcodec.Equivalent(want, tree))
	})
}

func TestAgeUsesActiveGenerationMtime(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "providers.json")
	ktavPath := ktavSiblingOf(jsonPath)
	require.NoError(t, os.WriteFile(jsonPath, []byte("{}"), 0o644))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(jsonPath, old, old))

	require.NoError(t, os.WriteFile(ktavPath, []byte("{}"), 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(ktavPath, now, now))

	age, err := newCache[[]catwalk.Provider](jsonPath).Age()
	require.NoError(t, err)
	require.Less(t, age, time.Hour, "active Ktav generation mtime must govern")

	require.NoError(t, os.Remove(ktavPath))
	age, err = newCache[[]catwalk.Provider](jsonPath).Age()
	require.NoError(t, err)
	require.Greater(t, age, 47*time.Hour, "must fall back to the legacy JSON mtime")
}
