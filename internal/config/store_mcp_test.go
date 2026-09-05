package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func isolatedMCPConfigStore(t *testing.T) (*ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	store, err := Init(root, root, false)
	require.NoError(t, err)
	return store, root
}

func TestConfigStoreMCPLiteralNamesRoundTripAndExactRemoval(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	names := []string{"foo", "foo.bar", `foo\bar`, "foo#*?", "foo.disabled", "foo.timeout"}
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "foo", MCPConfig{Type: MCPHttp, URL: "http://foo"}))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "foo.bar", MCPConfig{Type: MCPHttp, URL: "http://foo.bar"}))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, `foo\bar`, MCPConfig{Type: MCPHttp, URL: "http://foo-backslash"}))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "foo#*?", MCPConfig{Type: MCPHttp, URL: "http://foo-metachar"}))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "foo.disabled", MCPConfig{Type: MCPHttp, URL: "http://foo-disabled"}))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "foo.timeout", MCPConfig{Type: MCPHttp, URL: "http://foo-timeout"}))

	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	for _, name := range names {
		_, ok := root.MCP[name]
		require.True(t, ok, "disk must contain literal MCP key %q", name)
	}

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	for _, name := range names {
		_, ok := store.MCPConfig(name)
		require.True(t, ok, "reload must find literal MCP key %q", name)
	}
	specialNames := []string{"foo.bar", `foo\bar`, "foo#*?", "foo.disabled", "foo.timeout"}
	for _, name := range specialNames {
		require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, name, true))
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		disabledCfg, ok := store.MCPConfig(name)
		require.True(t, ok)
		require.True(t, disabledCfg.Disabled, "literal server %q was not disabled", name)
		fooCfg, ok := store.MCPConfig("foo")
		require.True(t, ok)
		require.False(t, fooCfg.Disabled, "disabling %q changed sibling foo", name)
		require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, name, false))
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		enabledCfg, ok := store.MCPConfig(name)
		require.True(t, ok)
		require.False(t, enabledCfg.Disabled, "literal server %q was not re-enabled", name)
	}

	for _, name := range specialNames {
		require.NoError(t, store.PersistRemoveMCPConfig(ScopeGlobal, name))
	}
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	for _, name := range specialNames {
		_, ok := store.MCPConfig(name)
		require.False(t, ok, "literal removal missed MCP key %q", name)
	}
	_, ok := store.MCPConfig("foo")
	require.True(t, ok, "removing literal special names must preserve sibling foo")
}

func TestConfigStoreExternalMCPDisabledOverrideUsesLiteralName(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	external := map[string]any{
		"mcpServers": map[string]any{
			"foo":          map[string]any{"type": "http", "url": "http://foo"},
			"foo.bar":      map[string]any{"type": "http", "url": "http://foo.bar"},
			`foo\bar`:      map[string]any{"type": "http", "url": "http://foo-backslash"},
			"foo#*?":       map[string]any{"type": "http", "url": "http://foo-metachar"},
			"foo.disabled": map[string]any{"type": "http", "url": "http://foo-disabled"},
		},
	}
	data, err := json.Marshal(external)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".mcp.json"), data, 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	for _, name := range []string{"foo", "foo.bar", `foo\bar`, "foo#*?", "foo.disabled"} {
		m, ok := store.MCPConfig(name)
		require.True(t, ok)
		require.False(t, m.Disabled)
	}
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeWorkspace, "foo.disabled", true))
	disabled, exists := readMCPDisabledOverride(store, ScopeWorkspace, "foo.disabled")
	require.True(t, exists)
	require.True(t, disabled)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	m, ok := store.MCPConfig("foo.disabled")
	require.True(t, ok)
	require.True(t, m.Disabled)
	for _, name := range []string{"foo", "foo.bar", `foo\bar`, "foo#*?"} {
		m, ok := store.MCPConfig(name)
		require.True(t, ok)
		require.False(t, m.Disabled, "override must not match sibling %q", name)
	}

	// False must remain an explicit override, otherwise a later reload would
	// lose the user's choice when the external source is merged again.
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeWorkspace, "foo.disabled", false))
	disabled, exists = readMCPDisabledOverride(store, ScopeWorkspace, "foo.disabled")
	require.True(t, exists)
	require.False(t, disabled)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	m, ok = store.MCPConfig("foo.disabled")
	require.True(t, ok)
	require.False(t, m.Disabled)
}
