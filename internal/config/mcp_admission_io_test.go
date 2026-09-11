package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestMCPAdmissionCanceledSidecarAcquisitionUsesCallerContext(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, path, "server", MCPConfig{Type: MCPHttp, URL: "http://admission.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://admission.example"}}},
		globalDataPath: path,
	})
	snapshot := store.SnapshotMCPAdmission("server")

	called := make(chan struct{})
	configTestHooks.Lock()
	previous := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, _ string) (*session.FileLock, error) {
		close(called)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previous
		configTestHooks.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- store.WithCurrentMCPAdmissionContext(ctx, snapshot, "server", func(MCPAdmissionGuard) error {
			return nil
		})
	}()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("admission did not reach sidecar acquisition")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled admission waited for the default lock timeout")
	}
}

func TestMCPAdmissionTokenCleanupOnPanic(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, path, "server", MCPConfig{Type: MCPHttp, URL: "http://panic.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://panic.example"}}},
		globalDataPath: path,
	})
	snapshot := store.SnapshotMCPAdmission("server")
	require.NoError(t, store.withMCPAdmissionLocks(func(files *mcpLockedFiles) error {
		evaluation, err := store.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		snapshot.MCPInput, snapshot.HasMCPInput = evaluation.mcpInputs["server"]
		return nil
	}))
	var guard MCPAdmissionGuard
	var token *mcpAdmissionToken
	func() {
		defer func() { require.Equal(t, "admission panic", recover()) }()
		require.NoError(t, store.WithCurrentMCPAdmission(snapshot, "server", func(current MCPAdmissionGuard) error {
			guard = current
			token = current.ref.current()
			require.NoError(t, current.ValidateCurrent())
			panic("admission panic")
		}))
	}()
	require.ErrorIs(t, guard.ValidateCurrent(), ErrMCPMutationStale)
	require.True(t, token.closed.Load())
	require.Nil(t, token.handles)
	if guard.ref != nil {
		require.Nil(t, guard.ref.current())
	}
}

func TestMCPAdmissionRejectsAppearanceOfAbsentHigherPrioritySource(t *testing.T) {
	root := normalizeReloadPath(t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "global-config"))
	globalPath := filepath.Join(root, "global-data", "rush.json")
	projectPath := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, globalPath, "server", MCPConfig{Type: MCPHttp, URL: "http://lower.example"})

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://lower.example"}}},
		globalDataPath: globalPath,
	})
	store.workingDir = root
	store.systemConfigPathOverride = filepath.Join(root, "missing-system", "rush.json")
	var snapshot MCPAdmissionSnapshot
	require.NoError(t, store.withMCPAdmissionLocks(func(files *mcpLockedFiles) error {
		evaluation, err := store.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		absent, ok := evaluation.fingerprints[normalizeDiscoveryPath(projectPath)]
		require.True(t, ok)
		require.False(t, absent.exists)
		snapshot = store.SnapshotMCPAdmission("server")
		snapshot.MCPInput, snapshot.HasMCPInput = evaluation.mcpInputs["server"]
		return nil
	}))

	var published bool
	err := store.WithCurrentMCPAdmissionContext(context.Background(), snapshot, "server", func(guard MCPAdmissionGuard) error {
		writeMCPDefinitionState(t, projectPath, "server", MCPConfig{Type: MCPHttp, URL: "http://higher.example"})
		if err := guard.RevalidateCurrentContext(context.Background()); err != nil {
			return err
		}
		published = true
		return nil
	})
	require.ErrorIs(t, err, ErrMCPMutationStale)
	require.False(t, published)
}

func TestMCPAdmissionTokenRejectsOwnerAndNlinkMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	writeMCPDefinitionState(t, path, "server", MCPConfig{Type: MCPHttp, URL: "http://fingerprint.example"})
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	t.Run("owner", func(t *testing.T) {
		file, openErr := openStableConfigFile(path)
		require.NoError(t, openErr)
		t.Cleanup(func() { _ = file.Close() })
		mismatch := expected
		mismatch.owner++
		require.ErrorIs(t, validatePreparedAdmissionFile(context.Background(), path, file, mismatch, 0, false), ErrMCPMutationStale)
	})

	t.Run("nlink", func(t *testing.T) {
		alias := filepath.Join(filepath.Dir(path), "alias.json")
		require.NoError(t, os.Link(path, alias))
		file, openErr := openStableConfigFile(path)
		require.NoError(t, openErr)
		t.Cleanup(func() {
			_ = file.Close()
			_ = os.Remove(alias)
		})
		require.ErrorIs(t, validatePreparedAdmissionFile(context.Background(), path, file, expected, 0, false), ErrMCPMutationStale)
	})
}

func TestMCPAdmissionLegacyTimeoutAndExplicitContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	writeMCPDefinitionState(t, path, "server", MCPConfig{Type: MCPHttp, URL: "http://timeout.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://timeout.example"}}},
		globalDataPath: path,
	})
	snapshot := store.SnapshotMCPAdmission("server")
	var observed []time.Duration
	configTestHooks.Lock()
	previousTimeout := configTestHooks.withConfigTimeout
	configTestHooks.withConfigTimeout = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		observed = append(observed, timeout)
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.withConfigTimeout = previousTimeout
		configTestHooks.Unlock()
	})

	require.ErrorIs(t, store.WithCurrentMCPAdmission(snapshot, "server", func(MCPAdmissionGuard) error {
		return nil
	}), context.Canceled)
	require.ErrorIs(t, store.withMCPAdmissionLocks(func(*mcpLockedFiles) error { return nil }), context.Canceled)
	require.Equal(t, []time.Duration{configWriteLockTimeout, configWriteLockTimeout}, observed)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, store.WithCurrentMCPAdmissionContext(ctx, snapshot, "server", func(MCPAdmissionGuard) error {
		return nil
	}), context.Canceled)
	require.Equal(t, []time.Duration{configWriteLockTimeout, configWriteLockTimeout}, observed,
		"explicit context admission must not install a legacy timeout")
}
