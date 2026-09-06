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

func TestLoadNewRushCandidateBetweenReadAndVerificationIsRetried(t *testing.T) {
	root := t.TempDir()
	globalConfig := filepath.Join(root, "global-config")
	globalData := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("XDG_CONFIG_HOME", globalConfig)
	t.Setenv("RUSH_GLOBAL_DATA", globalData)
	t.Setenv("XDG_DATA_HOME", globalData)

	store, err := Load(root, root, false)
	require.NoError(t, err)
	path := filepath.Join(root, "rush.json")
	var once sync.Once
	store.reloadAfterDiskRead = func() {
		once.Do(func() {
			writeMCPDefinition(t, path, "created-during-reload")
		})
	}
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	mcp, ok := store.MCPConfig("created-during-reload")
	require.True(t, ok, "a file created after candidate reading must be parsed on retry")
	require.Equal(t, "http://example.com", mcp.URL)
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestExactMCPMutationsCanEditShadowedGlobalLiteral(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	writeMCPDefinition(t, workspacePath, "shadowed")
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	require.NoError(t, store.PersistMCPConfigExact(ScopeGlobal, "shadowed", MCPConfig{
		Type: MCPHttp,
		URL:  "http://global.example",
	}))
	require.ErrorIs(t, store.PersistMCPFields(ScopeGlobal, "shadowed", map[string]any{
		"url": "http://must-fail.example",
	}), ErrMCPStale)
	require.NoError(t, store.PersistMCPFieldsExact(ScopeGlobal, "shadowed", map[string]any{
		"url": "http://global-updated.example",
	}))

	globalData, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	var globalRoot struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(globalData, &globalRoot))
	require.Equal(t, "http://global-updated.example", globalRoot.MCP["shadowed"]["url"])

	effective, ok := store.MCPConfig("shadowed")
	require.True(t, ok)
	require.Equal(t, "http://example.com", effective.URL, "workspace definition remains effective")
	require.NoError(t, store.PersistRemoveMCPConfigExact(ScopeGlobal, "shadowed"))
	effective, ok = store.MCPConfig("shadowed")
	require.True(t, ok)
	require.Equal(t, "http://example.com", effective.URL)
}

func TestMergeExternalMCPDocumentsUsesExactRushAndExternalPrecedence(t *testing.T) {
	server := func(url string) []byte {
		data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
			"same": map[string]any{"type": "http", "url": url},
		}})
		require.NoError(t, err)
		return data
	}
	rush := func(url string) []byte {
		data, err := json.Marshal(map[string]any{"mcp": map[string]any{
			"same": MCPConfig{Type: MCPHttp, URL: url},
		}})
		require.NoError(t, err)
		return data
	}

	externalDocuments := []stableConfigDocument{
		{path: "global.mcp.json", data: server("http://global.example"), present: true},
		{path: "project.mcp.json", data: server("http://project.example"), present: true},
	}
	cfg, err := loadFromBytes([][]byte{rush("http://rush.example")})
	require.NoError(t, err)
	mergeExternalMCPServersFromDocuments(cfg, []stableConfigDocument{
		{path: "project.rush.json", data: rush("http://rush.example"), present: true},
	}, externalDocuments)
	require.Equal(t, "http://rush.example", cfg.MCP["same"].URL)

	cfg = &Config{}
	mergeExternalMCPServersFromDocuments(cfg, nil, externalDocuments)
	require.Equal(t, "http://project.example", cfg.MCP["same"].URL)
}
