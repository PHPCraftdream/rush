package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsWriteAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, string, string)
	}{
		{
			name: "global lock",
			run: func(t *testing.T, workspace, globalData string) {
				writeSettingsWriteAllowedLock(t, globalData, "locked")
				SetProcessPassword("")
				require.ErrorIs(t, SettingsWriteAllowed(workspace), ErrSettingsLocked)
				SetProcessPassword("locked")
				require.NoError(t, SettingsWriteAllowed(workspace))
			},
		},
		{
			name: "canonical lock despite redirected global",
			run: func(t *testing.T, workspace, _ string) {
				canonical := filepath.Join(t.TempDir(), "canonical", "rush.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o700))
				writeSettingsWriteAllowedLock(t, canonical, "locked")
				previous := canonicalGlobalDataPath
				canonicalGlobalDataPath = func() string { return canonical }
				t.Cleanup(func() { canonicalGlobalDataPath = previous })
				SetProcessPassword("")
				require.ErrorIs(t, SettingsWriteAllowed(workspace), ErrSettingsLocked)
				SetProcessPassword("locked")
				require.NoError(t, SettingsWriteAllowed(workspace))
			},
		},
		{
			name: "local lock",
			run: func(t *testing.T, workspace, _ string) {
				local := filepath.Join(workspace, ".rush", "rush.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o700))
				writeSettingsWriteAllowedLock(t, local, "locked")
				SetProcessPassword("")
				require.ErrorIs(t, SettingsWriteAllowed(workspace), ErrSettingsLocked)
				SetProcessPassword("locked")
				require.NoError(t, SettingsWriteAllowed(workspace))
			},
		},
		{
			name: "global lock outranks local",
			run: func(t *testing.T, workspace, globalData string) {
				writeSettingsWriteAllowedLock(t, globalData, "global")
				local := filepath.Join(workspace, ".rush", "rush.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o700))
				writeSettingsWriteAllowedLock(t, local, "local")
				SetProcessPassword("local")
				require.ErrorIs(t, SettingsWriteAllowed(workspace), ErrSettingsLocked)
				SetProcessPassword("global")
				require.NoError(t, SettingsWriteAllowed(workspace))
			},
		},
		{
			name: "nonregular global fails closed",
			run: func(t *testing.T, _, globalData string) {
				require.NoError(t, os.MkdirAll(GlobalConfigData(), 0o700))
				SetProcessPassword("correct")
				require.ErrorIs(t, SettingsWriteAllowed(t.TempDir()), ErrSettingsLocked)
			},
		},
		{
			name: "missing locks",
			run: func(t *testing.T, workspace, _ string) {
				SetProcessPassword("")
				require.NoError(t, SettingsWriteAllowed(workspace))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, globalDataDir := isolateAllGlobalConfigPaths(t)
			globalData := filepath.Join(globalDataDir, "rush.json")
			canonical := filepath.Join(t.TempDir(), "canonical", "rush.json")
			previous := canonicalGlobalDataPath
			canonicalGlobalDataPath = func() string { return canonical }
			t.Cleanup(func() { canonicalGlobalDataPath = previous })
			workspace := t.TempDir()
			SetProcessPassword("")
			t.Cleanup(func() { SetProcessPassword("") })
			tc.run(t, workspace, globalData)
		})
	}
}

func writeSettingsWriteAllowedLock(t *testing.T, path, password string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"sync_rev":"`+HashPassword(password)+`"}`), 0o600))
}
