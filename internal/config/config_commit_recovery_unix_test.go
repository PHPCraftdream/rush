//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPCommitRecoversRestartedOwnTemporaryAlias(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, path, "old", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
	tempName := ".rush.json." + strings.Repeat("a", 32) + ".tmp"
	require.NoError(t, os.Link(path, filepath.Join(root, tempName)))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"old": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: path,
	})
	store.workingDir = root
	_, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, tempName))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Contains(t, string(mustReadFile(t, path)), "new.example")
}

func TestMCPCommitRejectsForeignHardlinksWithoutDeletingThem(t *testing.T) {
	t.Run("non-temporary alias", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "rush.json")
		alias := filepath.Join(root, "foreign-alias")
		writeMCPDefinitionState(t, path, "old", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
		require.NoError(t, os.Link(path, alias))

		store := newTestConfigStore(testStoreOpts{
			config:         &Config{MCP: MCPs{"old": {Type: MCPHttp, URL: "http://old.example"}}},
			globalDataPath: path,
		})
		store.workingDir = root
		_, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
		require.ErrorIs(t, err, ErrConfigHardLink)
		require.Contains(t, string(mustReadFile(t, alias)), "old.example")
	})

	t.Run("same-pattern foreign inode", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "rush.json")
		ownTemp := filepath.Join(root, ".rush.json."+strings.Repeat("a", 32)+".tmp")
		foreignTemp := filepath.Join(root, ".rush.json."+strings.Repeat("b", 32)+".tmp")
		writeMCPDefinitionState(t, path, "old", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
		require.NoError(t, os.Link(path, ownTemp))
		require.NoError(t, os.WriteFile(foreignTemp, []byte("foreign temporary data"), 0o600))

		store := newTestConfigStore(testStoreOpts{
			config:         &Config{MCP: MCPs{"old": {Type: MCPHttp, URL: "http://old.example"}}},
			globalDataPath: path,
		})
		store.workingDir = root
		_, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
		require.ErrorIs(t, err, ErrConfigHardLink)
		require.Contains(t, string(mustReadFile(t, ownTemp)), "old.example")
		require.Equal(t, []byte("foreign temporary data"), mustReadFile(t, foreignTemp))
	})
}
