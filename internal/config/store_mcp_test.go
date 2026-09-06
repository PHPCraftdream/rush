package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStoreExternalMCPDisabledOverlayPreservesDefinitionAcrossReloadAndRestart(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	name := `literal.foo\bar#*?`
	external := map[string]any{
		"mcpServers": map[string]any{
			name: map[string]any{
				"type":    "stdio",
				"command": "npx",
				"args":    []string{"server", "--flag"},
				"env":     map[string]string{"TOKEN": "$TOKEN"},
				"url":     "http://ignored.example/mcp",
				"headers": map[string]string{"Authorization": "Bearer $TOKEN"},
			},
		},
	}
	data, err := json.Marshal(external)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".mcp.json"), data, 0o600))

	assertExternalMCPDefinition := func(wantDisabled bool, current *ConfigStore) {
		t.Helper()
		mcpConfig, ok := current.MCPConfig(name)
		require.True(t, ok)
		require.Equal(t, MCPStdio, mcpConfig.Type)
		require.Equal(t, "npx", mcpConfig.Command)
		require.Equal(t, []string{"server", "--flag"}, mcpConfig.Args)
		require.Equal(t, map[string]string{"TOKEN": "$TOKEN"}, mcpConfig.Env)
		require.Equal(t, "http://ignored.example/mcp", mcpConfig.URL)
		require.Equal(t, map[string]string{"Authorization": "Bearer $TOKEN"}, mcpConfig.Headers)
		require.Equal(t, MCPSourceExternal, mcpConfig.Source)
		require.Equal(t, wantDisabled, mcpConfig.Disabled)
		require.Equal(t, 60, mcpConfig.Timeout)
	}

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	assertExternalMCPDefinition(false, store)
	for _, disabled := range []bool{true, false} {
		require.NoError(t, store.PersistMCPDisabledOverride(ScopeWorkspace, name, disabled))
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		assertExternalMCPDefinition(disabled, store)
	}

	restarted, err := Init(root, root, false)
	require.NoError(t, err)
	assertExternalMCPDefinition(false, restarted)
	require.NoError(t, restarted.PersistMCPDisabledOverride(ScopeWorkspace, name, true))
	require.NoError(t, restarted.ReloadFromDisk(context.Background()))
	assertExternalMCPDefinition(true, restarted)
}

func TestConfigStoreResolveMCPWritableScope(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		store, _ := isolatedMCPConfigStore(t)
		writeMCPDefinition(t, GlobalConfigData(), "global-name")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		scope, err := store.ResolveMCPWritableScope("global-name")
		require.NoError(t, err)
		require.Equal(t, ScopeGlobal, scope)
	})

	t.Run("workspace", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		workspacePath := filepath.Join(root, "rush.json")
		writeMCPDefinition(t, workspacePath, "workspace-name")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		scope, err := store.ResolveMCPWritableScope("workspace-name")
		require.NoError(t, err)
		require.Equal(t, ScopeWorkspace, scope)
	})

	t.Run("workspace wins over global", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		writeMCPDefinition(t, GlobalConfigData(), "same.name")
		writeMCPDefinition(t, filepath.Join(root, "rush.json"), "same.name")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		scope, err := store.ResolveMCPWritableScope("same.name")
		require.NoError(t, err)
		require.Equal(t, ScopeWorkspace, scope)
	})

	t.Run("external is not writable", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		writeExternalMCPDefinition(t, filepath.Join(root, ".mcp.json"), "external.name")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		_, err := store.ResolveMCPWritableScope("external.name")
		require.ErrorIs(t, err, ErrMCPExternal)
	})

	t.Run("project definition fails closed", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		writeMCPDefinition(t, filepath.Join(root, "rush.json"), "project.name")
		// Keep the writable workspace file elsewhere so root/rush.json is a
		// project definition rather than the workspace scope.
		store, err := Init(root, filepath.Join(root, "workspace-data"), false)
		require.NoError(t, err)
		_, err = store.ResolveMCPWritableScope("project.name")
		require.ErrorIs(t, err, ErrMCPUnwritableOrigin)
	})

	t.Run("missing and literal name", func(t *testing.T) {
		store, _ := isolatedMCPConfigStore(t)
		name := `literal.foo\bar#*?`
		writeMCPDefinition(t, GlobalConfigData(), name)
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		scope, err := store.ResolveMCPWritableScope(name)
		require.NoError(t, err)
		require.Equal(t, ScopeGlobal, scope)
		_, err = store.ResolveMCPWritableScope("missing")
		require.ErrorIs(t, err, ErrMCPNotFound)
	})
}

func TestConfigStoreReloadRetriesAfterConcurrentReplace(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	oldName := "old.literal"
	newName := "new.literal"
	writeMCPDefinition(t, filepath.Join(dataDir, "rush.json"), oldName)
	store, err := Load(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)

	read := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.reloadAfterDiskRead = func() {
		once.Do(func() {
			close(read)
			<-release
		})
	}

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- store.ReloadFromDisk(context.Background()) }()
	<-read
	require.NoError(t, store.PersistReplaceMCP(oldName, newName, MCPConfig{Type: MCPHttp, URL: "http://new.example"}))
	close(release)
	require.NoError(t, <-reloadDone)

	_, oldExists := store.MCPConfig(oldName)
	updated, newExists := store.MCPConfig(newName)
	require.False(t, oldExists)
	require.True(t, newExists)
	require.Equal(t, "http://new.example", updated.URL)
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestConfigStorePersistReplaceMCPInScopeTargetsWorkspaceAndPreservesFallback(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	oldName := "replace.literal"
	writeMCPDefinition(t, GlobalConfigData(), oldName)
	workspacePath := filepath.Join(root, "rush.json")
	writeMCPDefinition(t, workspacePath, oldName)
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	require.NoError(t, store.PersistReplaceMCPInScope(ScopeWorkspace, oldName, "renamed.literal", MCPConfig{
		Type: MCPHttp,
		URL:  "http://workspace.example",
	}))
	globalFallback, ok := store.MCPConfig(oldName)
	require.True(t, ok)
	require.Equal(t, "http://example.com", globalFallback.URL)
	renamed, ok := store.MCPConfig("renamed.literal")
	require.True(t, ok)
	require.Equal(t, "http://workspace.example", renamed.URL)

	data, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	entry, ok := mcpEntryFromJSON(data, "renamed.literal")
	require.True(t, ok)
	require.NotNil(t, entry["type"])
	_, ok = mcpEntryFromJSON(data, oldName)
	require.False(t, ok)

	globalData, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	_, ok = mcpEntryFromJSON(globalData, oldName)
	require.True(t, ok)
}

func writeMCPDefinition(t *testing.T, path, name string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{name: MCPConfig{Type: MCPHttp, URL: "http://example.com"}}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func writeExternalMCPDefinition(t *testing.T, path, name string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: map[string]any{"type": "http", "url": "http://example.com"}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

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

func TestConfigStoreExactMCPDisabledOverrideMutatesShadowedGlobal(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	name := "shadowed-disabled"
	workspacePath := filepath.Join(root, "rush.json")
	writeMCPDefinition(t, workspacePath, name)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.NoError(t, store.PersistMCPConfigExact(ScopeGlobal, name, MCPConfig{
		Type: MCPHttp,
		URL:  "http://global.example",
	}))

	// The workspace definition remains effective, but the exact operation must
	// mutate the literal global entry instead of rejecting the shadowed name.
	require.NoError(t, store.PersistMCPDisabledOverrideExact(ScopeGlobal, name, true))
	globalData, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	entry, ok := mcpEntryFromJSON(globalData, name)
	require.True(t, ok)
	var disabled bool
	require.NoError(t, json.Unmarshal(entry["disabled"], &disabled))
	require.True(t, disabled)
	effective, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.False(t, effective.Disabled)

	require.NoError(t, store.PersistMCPDisabledOverrideExact(ScopeGlobal, name, false))
	globalData, err = os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	entry, ok = mcpEntryFromJSON(globalData, name)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(entry["disabled"], &disabled))
	require.False(t, disabled)
}

func TestLoadFromBytesRejectsWrongTypedMCPDisabledOverlay(t *testing.T) {
	_, err := loadFromBytes([][]byte{[]byte(`{"mcp":{"external":{"disabled":"true"}}}`)})
	require.Error(t, err)
	require.Contains(t, err.Error(), `invalid MCP disabled override for "external"`)
}
