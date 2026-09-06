//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/stretchr/testify/require"
)

func TestMCPTransactionExcludesForeignOwnedFixedAndExternalInputs(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)

	global := filepath.Join(dataDir, "rush.json")
	workspace := filepath.Join(root, "workspace", "rush.json")
	system := filepath.Join(root, "system", "rush.json")
	projectExternal := filepath.Join(root, ".mcp.json")
	for _, path := range []string{global, workspace, system, projectExternal} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	}
	setMCPFile(t, global, "server", "http://owned.example")
	setMCPFile(t, workspace, "server", "http://foreign-workspace.example")
	setMCPFile(t, system, "server", "http://foreign-system.example")
	setExternalMCPFile(t, projectExternal, "server", "http://foreign-external.example")

	for _, path := range []string{workspace, system, projectExternal} {
		makeForeignOwned(t, path)
	}

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: global,
		workspacePath:  workspace,
	})
	store.workingDir = root
	store.systemConfigPathOverride = system

	paths := store.orderedMCPPaths()
	for _, foreign := range []string{workspace, system} {
		require.NotContains(t, paths, foreign)
	}
	require.NotContains(t, mcpJSONCandidatePaths(root), canonicalConfigPath(projectExternal))

	result, err := store.PersistRemoveMCPConfigResult(ScopeGlobal, "server")
	require.NoError(t, err)
	require.False(t, result.FallbackExists, "foreign inputs must not become a fallback definition")
}

func TestConfigStalenessDetectsSymlinkRetargetABA(t *testing.T) {
	root := t.TempDir()
	targetA := filepath.Join(root, "a.json")
	targetB := filepath.Join(root, "b.json")
	alias := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(targetA, []byte(`{"options":{"debug":true}}`), 0o600))
	require.NoError(t, os.WriteFile(targetB, []byte(`{"options":{"debug":false}}`), 0o600))
	if err := os.Symlink(targetA, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := newTestConfigStore(testStoreOpts{config: &Config{}})
	store.captureStalenessSnapshot([]string{alias})
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(targetB, alias))
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(targetA, alias))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, normalizeDiscoveryPath(alias))
}

func TestOwnedConfigReadChecksOwnerOnOpenedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{"debug":true}}`), 0o600))
	owner, err := fsext.Owner(filepath.Dir(path))
	require.NoError(t, err)
	require.Equal(t, path, eligibleConfigCandidate(path, owner))
	foreign := 0
	if owner == foreign {
		foreign = 1
	}
	if err := os.Chown(path, foreign, -1); err != nil {
		t.Skipf("changing test-file ownership unavailable: %v", err)
	}

	_, _, err = readStableConfigFileOwned(path, owner, true)
	require.ErrorIs(t, err, errConfigOwnerMismatch)
}

func TestExactWorkspaceMutationSharesPhysicalSymlinkDocument(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global", "rush.json")
	project := filepath.Join(root, "rush.json")
	workspace := filepath.Join(root, "workspace", "rush.json")
	setMCPFile(t, project, "old", "http://old.example")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o755))
	if err := os.Symlink(project, workspace); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(global), 0o755))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{}},
		globalDataPath: global,
		workspacePath:  workspace,
	})
	store.workingDir = root
	store.captureStalenessSnapshot([]string{project, workspace})

	result, err := store.mutateMCPExact("add", ScopeWorkspace, "new", "new", MCPConfig{
		Type: MCPHttp, URL: "http://new.example",
	}, nil)
	require.NoError(t, err)
	require.True(t, result.NewExists)
	require.Equal(t, MCPOriginWorkspace, result.NewOrigin.Kind)
	require.Equal(t, "http://new.example", result.NewConfig.URL)
	projectData, err := os.ReadFile(project)
	require.NoError(t, err)
	workspaceData, err := os.ReadFile(workspace)
	require.NoError(t, err)
	require.Equal(t, projectData, workspaceData)
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestMCPCommitRejectsParentReplacement(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "config")
	global := filepath.Join(parent, "rush.json")
	setMCPFile(t, global, "old", "http://old.example")
	store := newTestConfigStore(testStoreOpts{config: &Config{MCP: MCPs{}}, globalDataPath: global})
	store.workingDir = root
	var once sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = func() {
		once.Do(func() {
			moved := parent + ".old"
			require.NoError(t, os.Rename(parent, moved))
			require.NoError(t, os.MkdirAll(parent, 0o755))
			setMCPFile(t, global, "replacement", "http://replacement.example")
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	})

	_, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.ErrorIs(t, err, ErrMCPStale)
	got, err := os.ReadFile(global)
	require.NoError(t, err)
	require.Contains(t, string(got), "replacement")
}

func TestStableDocumentsDedupAfterPreOpenRetarget(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	pathP := filepath.Join(root, "p.json")
	pathQ := filepath.Join(root, "q.json")
	require.NoError(t, os.WriteFile(first, []byte(`{"options":{"debug":true}}`), 0o600))
	require.NoError(t, os.WriteFile(second, []byte(`{"options":{"debug":false}}`), 0o600))
	if err := os.Symlink(first, pathP); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(second, pathQ); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var once sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.beforeOpen
	configTestHooks.beforeOpen = func(path string) {
		if filepath.Clean(path) != filepath.Clean(pathP) {
			return
		}
		once.Do(func() {
			require.NoError(t, os.Remove(pathP))
			require.NoError(t, os.Symlink(second, pathP))
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeOpen = previous
		configTestHooks.Unlock()
	})

	documents, err := readStableConfigDocuments([]string{pathP, pathQ})
	require.NoError(t, err)
	require.Len(t, documents, 1)
	require.Len(t, documents[0].aliases, 2)
}

func TestWorkspaceOwnerPolicyAppliesOutsideCheckout(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	checkout := t.TempDir()
	dataDir := t.TempDir()
	workspacePath := filepath.Join(dataDir, "rush.json")
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{"mcp":{"foreign":{"type":"http"}}}`), 0o600))
	makeForeignOwned(t, workspacePath)

	_, err := Load(checkout, dataDir, false)
	require.ErrorIs(t, err, errConfigOwnerMismatch)
}

func makeForeignOwned(t *testing.T, path string) {
	t.Helper()
	owner, err := fsext.Owner(path)
	require.NoError(t, err)
	foreign := 0
	if owner == foreign {
		foreign = 1
	}
	if err := os.Chown(path, foreign, -1); err != nil {
		t.Skipf("changing test-file ownership unavailable: %v", err)
	}
}
