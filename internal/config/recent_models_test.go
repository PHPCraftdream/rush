package config

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// readConfigJSON reads and unmarshals the JSON config file at path.
func readConfigJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	baseDir := filepath.Dir(path)
	fileName := filepath.Base(path)
	b, err := fs.ReadFile(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// readRecentModels reads the recent_models section from the config file.
func readRecentModels(t *testing.T, path string) map[string]any {
	t.Helper()
	out := readConfigJSON(t, path)
	rm, ok := out["recent_models"].(map[string]any)
	require.True(t, ok)
	return rm
}

// testStoreWithPath creates a ConfigStore backed by a Config for recent model tests.
func testStoreWithPath(cfg *Config, dir string) *ConfigStore {
	return newTestConfigStore(testStoreOpts{
		config:         cfg,
		globalDataPath: filepath.Join(dir, "config.json"),
	})
}

func TestRecordRecentModel_AddsAndPersists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	err := store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "openai", Model: "gpt-4o"})
	require.NoError(t, err)

	// in-memory state
	require.Len(t, store.Config().RecentModels[SelectedModelTypeSmart], 1)
	require.Equal(t, "openai", store.Config().RecentModels[SelectedModelTypeSmart][0].Provider)
	require.Equal(t, "gpt-4o", store.Config().RecentModels[SelectedModelTypeSmart][0].Model)

	// persisted state
	rm := readRecentModels(t, store.globalDataPath)
	smart, ok := rm[string(SelectedModelTypeSmart)].([]any)
	require.True(t, ok)
	require.Len(t, smart, 1)
	item, ok := smart[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "openai", item["provider"])
	require.Equal(t, "gpt-4o", item["model"])
}

func TestRecordRecentModel_DedupeAndMoveToFront(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	// Add two entries
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "openai", Model: "gpt-4o"}))
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "anthropic", Model: "claude"}))
	// Re-add first; should move to front and not duplicate
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "openai", Model: "gpt-4o"}))

	got := store.Config().RecentModels[SelectedModelTypeSmart]
	require.Len(t, got, 2)
	require.Equal(t, SelectedModel{Provider: "openai", Model: "gpt-4o"}, got[0])
	require.Equal(t, SelectedModel{Provider: "anthropic", Model: "claude"}, got[1])
}

func TestRecordRecentModel_TrimsToMax(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	// Insert 6 unique models; max is 5
	entries := []SelectedModel{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
		{Provider: "p3", Model: "m3"},
		{Provider: "p4", Model: "m4"},
		{Provider: "p5", Model: "m5"},
		{Provider: "p6", Model: "m6"},
	}
	for _, e := range entries {
		require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, e))
	}

	// in-memory state
	got := store.Config().RecentModels[SelectedModelTypeSmart]
	require.Len(t, got, 5)
	// Newest first, capped at 5: p6..p2
	require.Equal(t, SelectedModel{Provider: "p6", Model: "m6"}, got[0])
	require.Equal(t, SelectedModel{Provider: "p5", Model: "m5"}, got[1])
	require.Equal(t, SelectedModel{Provider: "p4", Model: "m4"}, got[2])
	require.Equal(t, SelectedModel{Provider: "p3", Model: "m3"}, got[3])
	require.Equal(t, SelectedModel{Provider: "p2", Model: "m2"}, got[4])

	// persisted state: verify trimmed to 5 and newest-first order
	rm := readRecentModels(t, store.globalDataPath)
	smart, ok := rm[string(SelectedModelTypeSmart)].([]any)
	require.True(t, ok)
	require.Len(t, smart, 5)
	// Build provider:model IDs and verify order
	var ids []string
	for _, v := range smart {
		m := v.(map[string]any)
		ids = append(ids, m["provider"].(string)+":"+m["model"].(string))
	}
	require.Equal(t, []string{"p6:m6", "p5:m5", "p4:m4", "p3:m3", "p2:m2"}, ids)
}

func TestRecordRecentModel_SkipsEmptyValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	// Missing provider
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "", Model: "m"}))
	// Missing model
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "p", Model: ""}))

	_, ok := store.Config().RecentModels[SelectedModelTypeSmart]
	// Map may be initialized, but should have no entries
	if ok {
		require.Len(t, store.Config().RecentModels[SelectedModelTypeSmart], 0)
	}
	// No file should be written (stat via fs.FS)
	baseDir := filepath.Dir(store.globalDataPath)
	fileName := filepath.Base(store.globalDataPath)
	_, err := fs.Stat(os.DirFS(baseDir), fileName)
	require.True(t, os.IsNotExist(err))
}

func TestRecordRecentModel_NoPersistOnNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	entry := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, entry))

	baseDir := filepath.Dir(store.globalDataPath)
	fileName := filepath.Base(store.globalDataPath)
	before, err := fs.ReadFile(os.DirFS(baseDir), fileName)
	require.NoError(t, err)

	// Get file ModTime to verify no write occurs
	stBefore, err := fs.Stat(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	beforeMod := stBefore.ModTime()

	// Re-record same entry should be a no-op (no write)
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, entry))

	after, err := fs.ReadFile(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	// Verify ModTime unchanged to ensure truly no write occurred
	stAfter, err := fs.Stat(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	require.True(t, stAfter.ModTime().Equal(beforeMod), "file ModTime should not change on noop")
}

func TestUpdatePreferredModel_UpdatesRecents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	sel := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeFast, sel))

	// in-memory
	require.Equal(t, sel, store.Config().Models[SelectedModelTypeFast])
	require.Len(t, store.Config().RecentModels[SelectedModelTypeFast], 1)

	// persisted (read via fs.FS)
	rm := readRecentModels(t, store.globalDataPath)
	fast, ok := rm[string(SelectedModelTypeFast)].([]any)
	require.True(t, ok)
	require.Len(t, fast, 1)
}

func TestRecordRecentModel_TypeIsolation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := testStoreWithPath(cfg, dir)

	// Add models to both smart and small types
	smartModel := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	fastModel := SelectedModel{Provider: "anthropic", Model: "claude"}

	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, smartModel))
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeFast, fastModel))

	// in-memory: verify types maintain separate histories
	require.Len(t, store.Config().RecentModels[SelectedModelTypeSmart], 1)
	require.Len(t, store.Config().RecentModels[SelectedModelTypeFast], 1)
	require.Equal(t, smartModel, store.Config().RecentModels[SelectedModelTypeSmart][0])
	require.Equal(t, fastModel, store.Config().RecentModels[SelectedModelTypeFast][0])

	// Add another to large, verify small unchanged
	anotherSmart := SelectedModel{Provider: "google", Model: "gemini"}
	require.NoError(t, store.recordRecentModel(ScopeGlobal, SelectedModelTypeSmart, anotherSmart))

	require.Len(t, store.Config().RecentModels[SelectedModelTypeSmart], 2)
	require.Len(t, store.Config().RecentModels[SelectedModelTypeFast], 1)
	require.Equal(t, fastModel, store.Config().RecentModels[SelectedModelTypeFast][0])

	// persisted state: verify both types exist with correct lengths and contents
	rm := readRecentModels(t, store.globalDataPath)

	smart, ok := rm[string(SelectedModelTypeSmart)].([]any)
	require.True(t, ok)
	require.Len(t, smart, 2)
	// Verify newest first for large type
	require.Equal(t, "google", smart[0].(map[string]any)["provider"])
	require.Equal(t, "gemini", smart[0].(map[string]any)["model"])
	require.Equal(t, "openai", smart[1].(map[string]any)["provider"])
	require.Equal(t, "gpt-4o", smart[1].(map[string]any)["model"])

	fast, ok := rm[string(SelectedModelTypeFast)].([]any)
	require.True(t, ok)
	require.Len(t, fast, 1)
	require.Equal(t, "anthropic", fast[0].(map[string]any)["provider"])
	require.Equal(t, "claude", fast[0].(map[string]any)["model"])
}
