package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStore_RuntimeOverrides_Independent(t *testing.T) {
	t.Parallel()

	store1 := newTestConfigStore(testStoreOpts{config: &Config{}})
	store2 := newTestConfigStore(testStoreOpts{config: &Config{}})

	require.False(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)

	store1.SetSkipPermissionRequests(true)

	require.True(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)
}

func TestConfigStore_RuntimeOverrides_SetterPublishesNewGeneration(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{config: &Config{}})

	require.False(t, store.Overrides().SkipPermissionRequests)

	store.SetSkipPermissionRequests(true)

	require.True(t, store.Overrides().SkipPermissionRequests)
}

func TestGlobalWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRUSH_GLOBAL_DATA", dir)

	wsDir := GlobalWorkspaceDir()
	globalData := GlobalConfigData()

	require.Equal(t, filepath.Dir(globalData), wsDir)
	require.Equal(t, dir, wsDir)
}

func TestScope_String(t *testing.T) {
	t.Parallel()

	require.Equal(t, "global", ScopeGlobal.String())
	require.Equal(t, "workspace", ScopeWorkspace.String())
	require.Contains(t, Scope(99).String(), "Scope(99)")
}
