package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestMCPAdmissionRejectsLogicalRetargetAfterSidecarResolution(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a", "rush.json")
	bPath := filepath.Join(root, "b", "rush.json")
	logicalPath := filepath.Join(root, "rush.json")
	data := []byte(`{"mcp":{"server":{"type":"http","url":"http://same.example"}}}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(aPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(bPath), 0o755))
	require.NoError(t, os.WriteFile(aPath, data, 0o600))
	require.NoError(t, os.WriteFile(bPath, data, 0o600))
	if err := os.Symlink(aPath, logicalPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://same.example"}}},
		globalDataPath: logicalPath,
	})
	snapshot := store.SnapshotMCPAdmission("server")
	var retarget sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.afterMCPResolveTarget
	configTestHooks.afterMCPResolveTarget = func(target *configWriteTarget) {
		if normalizeDiscoveryPath(target.selectedPath) != normalizeDiscoveryPath(logicalPath) {
			return
		}
		retarget.Do(func() {
			require.NoError(t, os.Remove(logicalPath))
			require.NoError(t, os.Symlink(bPath, logicalPath))
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.afterMCPResolveTarget = previous
		configTestHooks.Unlock()
	})

	var published bool
	err := store.WithCurrentMCPAdmission(snapshot, "server", func(MCPAdmissionGuard) error {
		published = true
		return nil
	})
	require.ErrorIs(t, err, ErrMCPMutationStale)
	require.False(t, published)

	contender := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://same.example"}}},
		globalDataPath: logicalPath,
	})
	require.NoError(t, contender.PersistMCPFieldsExact(ScopeGlobal, "server", map[string]any{
		"url": "http://b.example",
	}))
	require.Equal(t, data, mustReadFile(t, aPath))
	require.Contains(t, string(mustReadFile(t, bPath)), "http://b.example")
}

func TestVerifyConfigTargetBindingRejectsInvalidExistingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	aliasChain := [32]byte{1}
	identity := configFileIdentity{device: 1, inode: 1, valid: true}
	target := configWriteTarget{
		path: path, selectedPath: path,
		expected: reloadFileFingerprint{exists: true, identity: identity, aliasChain: aliasChain},
	}
	actual := target.expected
	actual.identity = configFileIdentity{}

	err := verifyConfigTargetBinding(target, actual, nil)
	require.ErrorIs(t, err, errConfigCommitVerification)
}

func TestMCPAdmissionInvokesBindingVerifierAfterSidecarAcquisition(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"mcp":{"server":{"type":"http","url":"http://same.example"}}}`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://same.example"}}},
		globalDataPath: path,
	})
	snapshot := store.SnapshotMCPAdmission("server")
	// Synchronize the white-box snapshot with the exact disk input so this
	// oracle reaches the binding verifier rather than an earlier stale fence.
	require.NoError(t, store.withMCPAdmissionLocks(func(files *mcpLockedFiles) error {
		evaluation, err := store.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		input, ok := evaluation.mcpInputs["server"]
		snapshot.MCPInput, snapshot.HasMCPInput = input, ok
		return nil
	}))

	var verified bool
	configTestHooks.Lock()
	previous := configTestHooks.beforeMCPTargetBindingVerification
	configTestHooks.beforeMCPTargetBindingVerification = func(target *configWriteTarget) {
		verified = true
		target.expected.aliasChain[0] ^= 0xff
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeMCPTargetBindingVerification = previous
		configTestHooks.Unlock()
	})

	var published bool
	err := store.WithCurrentMCPAdmission(snapshot, "server", func(MCPAdmissionGuard) error {
		published = true
		return nil
	})
	require.True(t, verified)
	require.ErrorIs(t, err, ErrMCPMutationStale)
	require.False(t, published)
}

func TestMCPFieldsPublishConfigAndFingerprintInOneGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, path, "server", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: filepath.Join(root, "global", "rush.json"),
		workspacePath:  path,
	})
	store.workingDir = root
	before := store.Generation()

	require.NoError(t, store.PersistMCPFieldsExact(ScopeWorkspace, "server", map[string]any{
		"url": "http://new.example",
	}))
	require.Equal(t, before+1, store.Generation())
	got, ok := store.MCPConfig("server")
	require.True(t, ok)
	require.Equal(t, "http://new.example", got.URL)
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestMCPMutationRejectsHardLinkAliasesWithoutChangingEitherDirent(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical.json")
	project := filepath.Join(root, "rush.json")
	workspace := filepath.Join(root, "data", "rush.json")
	writeMCPDefinitionState(t, physical, "server", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
	require.NoError(t, os.Link(physical, project))
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o755))
	require.NoError(t, os.Link(physical, workspace))
	before := mustReadFile(t, project)

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: filepath.Join(root, "global", "rush.json"),
		workspacePath:  workspace,
	})
	store.workingDir = root
	err := store.PersistMCPFieldsExact(ScopeWorkspace, "server", map[string]any{
		"url": "http://new.example",
	})
	require.ErrorIs(t, err, ErrMCPHardLinkTopology)
	require.Equal(t, before, mustReadFile(t, project))
	require.Equal(t, before, mustReadFile(t, workspace))
}

func TestMCPPostRenameFailureReconcilesCommittedDocument(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global", "rush.json")
	writeMCPDefinitionState(t, global, "server", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: global,
	})
	store.workingDir = root
	configTestHooks.Lock()
	previous := configTestHooks.afterCommitRename
	configTestHooks.afterCommitRename = func() error { return context.Canceled }
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.afterCommitRename = previous
		configTestHooks.Unlock()
	})

	_, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.NoError(t, err)
	require.Contains(t, string(mustReadFile(t, global)), "new.example")
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestMCPPostCommitFingerprintFailureReturnsUnreconciledOutcome(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	physical := filepath.Join(root, "project", "rush.json")
	alias := filepath.Join(root, "workspace", "rush.json")
	setMCPFile(t, physical, "old", "http://old.example")
	require.NoError(t, os.MkdirAll(filepath.Dir(alias), 0o755))
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"old": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: physical,
		workspacePath:  alias,
	})
	store.workingDir = root
	store.captureStalenessSnapshot([]string{physical, alias})
	beforeGeneration := store.Generation()
	configTestHooks.Lock()
	previous := configTestHooks.afterMCPCommit
	var commitReturnedNil bool
	configTestHooks.afterMCPCommit = func(path string) {
		if normalizeReloadPath(path) == normalizeReloadPath(physical) {
			commitReturnedNil = true
			require.NoError(t, os.Remove(alias))
		}
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.afterMCPCommit = previous
		configTestHooks.Unlock()
	})

	err := store.PersistMCPConfigExact(ScopeWorkspace, "new", MCPConfig{
		Type: MCPHttp, URL: "http://new.example",
	})
	require.True(t, commitReturnedNil, "the fingerprint failure must occur after a nil commit result")
	outcome, ok := CommitOutcomeFromError(err)
	require.True(t, ok)
	require.True(t, outcome.Committed)
	require.False(t, outcome.Reconciled)
	require.Equal(t, normalizeReloadPath(physical), outcome.Path)
	require.ErrorIs(t, outcome, errConfigCommitCommitted)
	require.ErrorIs(t, outcome, errConfigCommitUncertain)
	require.ErrorIs(t, err, ErrMCPCommitUncertain)
	require.False(t, mcpCommitWasReconciled(err))
	require.Equal(t, beforeGeneration, store.Generation())
	_, exists := store.MCPConfig("new")
	require.False(t, exists)
	require.True(t, store.ConfigStaleness().Dirty)
}

func TestConfigWriteSymlinkAliasesSharePhysicalSidecarLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	alias := filepath.Join(root, "alias.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0o600))
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lock, err := session.TryAcquireFileLock(normalizeReloadPath(target) + ".lock")
	require.NoError(t, err)
	defer lock.Release()
	store := newTestConfigStore(testStoreOpts{globalDataPath: alias})
	done := make(chan error, 1)
	go func() { done <- store.SetConfigField(ScopeGlobal, "held", true) }()
	select {
	case err := <-done:
		t.Fatalf("write crossed the physical alias lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, lock.Release())
	require.NoError(t, <-done)
	require.Contains(t, string(mustReadFile(t, target)), "held")
	_, err = os.Lstat(alias)
	require.NoError(t, err)
}

func TestOriginPrecedenceNormalizesAliasPaths(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "rush.json")
	project := filepath.Join(root, "project", "rush.json")
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{
		"server": MCPConfig{Type: MCPHttp, URL: "http://example.com"},
	}})
	require.NoError(t, err)
	origins := make(map[string]MCPOrigin)
	entryOrigins(origins, project, workspace, filepath.Join(root, "global", "rush.json"), "", data)
	require.Equal(t, MCPOriginProject, origins["server"].Kind)
	entryOrigins(origins, filepath.Join(root, "workspace", ".", "rush.json"), workspace, filepath.Join(root, "global", "rush.json"), "", data)
	require.Equal(t, MCPOriginWorkspace, origins["server"].Kind)
}
