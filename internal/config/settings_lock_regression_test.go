package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsLockRegression_F2RedirectedGlobalPath(t *testing.T) {
	t.Cleanup(func() { SetProcessPassword("") })
	root := t.TempDir()
	canonical := filepath.Join(root, "canonical", "rush.json")
	global := filepath.Join(root, "global.json")
	workspace := filepath.Join(root, "workspace.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o700))
	require.NoError(t, os.WriteFile(canonical, []byte(`{"sync_rev":"`+HashPassword("canonical-lock")+`"}`), 0o600))
	previousCanonical := canonicalGlobalDataPath
	canonicalGlobalDataPath = func() string { return canonical }
	t.Cleanup(func() { canonicalGlobalDataPath = previousCanonical })

	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global, workspacePath: workspace})
	store.workingDir = root
	SetProcessPassword("")
	require.ErrorIs(t, store.SetConfigField(ScopeWorkspace, "value", "blocked"), ErrSettingsLocked)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "value", "allowed"))
	SetProcessPassword("canonical-lock")
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "value", "allowed"))
	SetProcessPassword("")
	canonicalGlobalDataPath = func() string { return "" }
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "value", "unlocked"))
}

func TestSettingsLockRegression_F3RedirectedWorkspacePath(t *testing.T) {
	t.Cleanup(func() { SetProcessPassword("") })
	root := t.TempDir()
	global := filepath.Join(root, "global.json")
	workspace := filepath.Join(t.TempDir(), "rush.json")
	natural := filepath.Join(root, ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(natural), 0o700))
	require.NoError(t, os.WriteFile(natural, []byte(`{"sync_rev":"`+HashPassword("workspace-lock")+`"}`), 0o600))

	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global, workspacePath: workspace})
	store.workingDir = root
	SetProcessPassword("")
	require.ErrorIs(t, store.SetConfigField(ScopeGlobal, "value", "blocked"), ErrSettingsLocked)
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "value", "allowed"))
	SetProcessPassword("workspace-lock")
	require.NoError(t, store.SetConfigField(ScopeGlobal, "value", "allowed"))
}

// A redirected workspace reached through a directory alias is still inside its
// own directory (macOS TempDir is /var → /private/var; Windows CI has 8.3 names).
func TestSettingsLockRegression_F3AliasedWorkspaceDir(t *testing.T) {
	t.Cleanup(func() { SetProcessPassword("") })
	root := t.TempDir()
	natural := filepath.Join(root, ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(natural), 0o700))
	require.NoError(t, os.WriteFile(natural, []byte(`{"sync_rev":"`+HashPassword("workspace-lock")+`"}`), 0o600))
	realDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: filepath.Join(root, "global.json"), workspacePath: filepath.Join(alias, "rush.json")})
	store.workingDir = root
	SetProcessPassword("")
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "value", "allowed"))
}

func TestSettingsLockRegression_NonregularGlobalFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation func(*ConfigStore) error
	}{
		{"workspace set", func(s *ConfigStore) error { return s.SetConfigField(ScopeWorkspace, "value", "changed") }},
		{"workspace lock", func(s *ConfigStore) error { return s.LockSettings(ScopeWorkspace, "pw") }},
		{"workspace unlock", func(s *ConfigStore) error { return s.UnlockSettings(ScopeWorkspace, "local") }},
		{"password blank", func(s *ConfigStore) error { SetProcessPassword(""); return s.CheckProcessPassword() }},
		{"password nonblank", func(s *ConfigStore) error { SetProcessPassword("local"); return s.CheckProcessPassword() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { SetProcessPassword("") })
			SetProcessPassword("local")
			root := t.TempDir()
			globalPath := filepath.Join(root, "global.json")
			workspacePath := filepath.Join(root, "workspace.json")
			require.NoError(t, os.WriteFile(workspacePath, []byte(`{"sync_rev":"`+HashPassword("local")+`"}`), 0o600))
			require.NoError(t, os.Mkdir(globalPath, 0o700))
			store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: globalPath, workspacePath: workspacePath})
			require.ErrorIs(t, tc.operation(store), ErrSettingsLocked)
		})
	}

	t.Run("absent global allowed", func(t *testing.T) {
		t.Cleanup(func() { SetProcessPassword("") })
		root := t.TempDir()
		store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: filepath.Join(root, "absent.json"), workspacePath: filepath.Join(root, "workspace.json")})
		require.NoError(t, store.CheckProcessPassword())
		require.NoError(t, store.SetConfigField(ScopeWorkspace, "value", "allowed"))
	})
}

func TestSettingsLockRegression_ReservedAliasesBlockedAndAudited(t *testing.T) {
	aliases := []string{
		"sync_rev", `sync_rev.secret`, `sync\_rev`, `:sync_rev`,
		"cache_rev", `cache_rev.secret`, `cache\_rev`, `:cache_rev`,
	}
	methods := []struct {
		name string
		run  func(*ConfigStore, string) error
	}{
		{"SetConfigFields", func(s *ConfigStore, key string) error {
			return s.SetConfigFields(ScopeGlobal, map[string]any{key: "attempt", "models.smart": SelectedModel{Provider: "p", Model: "m"}})
		}},
		{"RemoveConfigField", func(s *ConfigStore, key string) error { return s.RemoveConfigField(ScopeGlobal, key) }},
	}
	for _, alias := range aliases {
		for _, method := range methods {
			t.Run(method.name+"/"+alias, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "global.json")
				store := lockStore(t, path)
				require.NoError(t, store.SetConfigField(ScopeGlobal, "seed", "unchanged"))
				require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
				SetProcessPassword("pw")
				t.Cleanup(func() { SetProcessPassword(""); SetSettingsAuditSink(nil) })
				before := fileBytes(t, path)
				var event SettingsEvent
				SetSettingsAuditSink(func(e SettingsEvent) { event = e })
				t.Cleanup(func() { SetSettingsAuditSink(nil) })
				err := method.run(store, alias)
				require.ErrorIs(t, err, ErrSettingsLocked)
				require.Equal(t, before, fileBytes(t, path))
				require.Contains(t, event.Keys, "*")
				require.NotContains(t, event.Keys, alias)
			})
		}
	}
}

func TestSettingsLockRegression_MissingWorkspaceErrorDoesNotExposeKey(t *testing.T) {
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: filepath.Join(t.TempDir(), "global.json")})
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"set reserved hash", func() error { return store.SetConfigField(ScopeWorkspace, "sync_rev", HashPassword("x")) }},
		{"remove reserved hash", func() error { return store.RemoveConfigField(ScopeWorkspace, "sync_rev") }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "sync_rev")
			require.NotContains(t, err.Error(), HashPassword("x"))
		})
	}
}

func TestSettingsLockRegression_LockedDebugLogIsBestEffortAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.json")
	store := lockStore(t, path)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "legacy", "keep"))
	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
	before := fileBytes(t, path)
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store.removeConfigFieldBestEffort(ScopeGlobal, "legacy")
	text := output.String()
	require.Equal(t, before, fileBytes(t, path))
	require.Contains(t, text, "DEBUG")
	require.NotContains(t, text, "WARN")
	require.NotContains(t, text, ErrSettingsLocked.Error())
	require.NotContains(t, text, HashPassword("pw"))
}
