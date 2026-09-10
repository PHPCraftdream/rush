package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCycle7MCPTransactionIsConditionalAcrossBothScopes(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global", "rush.json")
	workspace := filepath.Join(root, "workspace", "rush.json")
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{}},
		globalDataPath: global,
		workspacePath:  workspace,
	})
	// The helper's working directory is deliberately set after construction so
	// the transaction exercises the same disk paths as a real store.
	store.workingDir = root
	store2 := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{}},
		globalDataPath: global,
		workspacePath:  workspace,
	})
	store2.workingDir = root

	want := MCPConfig{Type: MCPHttp, URL: "http://one.example"}
	var firstErr, secondErr error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i == 0 {
				firstErr = store.PersistMCPConfig(ScopeGlobal, "same", want)
			} else {
				secondErr = store2.PersistMCPConfig(ScopeGlobal, "same", want)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	require.NotEqual(t, firstErr == nil, secondErr == nil, "exactly one concurrent add must win")
	failed := firstErr
	if failed == nil {
		failed = secondErr
	}
	require.ErrorIs(t, failed, ErrMCPTargetExists)

	// A workspace owner cannot be redirected into global by a stale caller.
	require.NoError(t, store.PersistMCPConfig(ScopeWorkspace, "owned", MCPConfig{Type: MCPHttp, URL: "http://workspace.example"}))
	err := store.PersistReplaceMCPInScope(ScopeGlobal, "owned", "renamed", want)
	require.ErrorIs(t, err, ErrMCPStale)
	_, ok := mcpEntryFromJSON(mustReadFile(t, global), "renamed")
	require.False(t, ok)
}

func TestCycle7MCPResultUsesFullFallbackPipeline(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global", "rush.json")
	workspace := filepath.Join(root, "data", "rush.json")
	project := filepath.Join(root, "rush.json")
	setMCPFile(t, global, "server", "http://global.example")
	setMCPFile(t, project, "server", "http://project.example")
	setMCPFile(t, workspace, "server", "http://workspace.example")
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: global,
		workspacePath:  workspace,
	})
	store.workingDir = root
	result, err := store.PersistReplaceMCPResult(ScopeWorkspace, "server", "renamed", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.NoError(t, err)
	require.True(t, result.NewExists)
	require.Equal(t, "http://new.example", result.NewConfig.URL)
	require.Equal(t, MCPOriginWorkspace, result.NewOrigin.Kind)
	require.True(t, result.FallbackExists)
	require.Equal(t, "http://project.example", result.FallbackConfig.URL)
	require.Equal(t, MCPOriginProject, result.FallbackOrigin.Kind)

	// With the project layer removed, the same result must reveal the external
	// layer, not use a workspace/global shortcut.
	require.NoError(t, os.Remove(project))
	externalPath := filepath.Join(root, ".mcp.json")
	setExternalMCPFile(t, externalPath, "renamed", "http://external.example")
	result, err = store.PersistRemoveMCPConfigResult(ScopeWorkspace, "renamed")
	require.NoError(t, err)
	require.True(t, result.NewExists)
	require.Equal(t, "http://external.example", result.NewConfig.URL)
	require.Equal(t, MCPOriginExternal, result.NewOrigin.Kind)
}

func TestCycle7DisabledOnlyMCPOverlayPrecedesDeepMerge(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("overlay-%t", disabled), func(t *testing.T) {
			global, _ := json.Marshal(map[string]any{"mcp": map[string]any{
				"server": map[string]any{"type": "http", "url": "http://full.example", "disabled": false},
			}})
			workspace, _ := json.Marshal(map[string]any{"mcp": map[string]any{
				"server": map[string]any{"disabled": disabled},
			}})
			cfg, err := loadFromBytes([][]byte{global, workspace})
			require.NoError(t, err)
			require.False(t, cfg.MCP["server"].Disabled, "an overlay must not toggle a competing full Rush definition")
		})
	}
}

func TestCycle7FingerprintUsesExactCandidateBytesAcrossABA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	a := []byte(`{"mcp":{"server":{"type":"http","url":"http://a"}}}`)
	b := []byte(`{"mcp":{"server":{"type":"http","url":"http://b"}}}`)
	require.NoError(t, os.WriteFile(path, a, 0o600))
	parsed, fingerprint, err := readStableConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, a, parsed)
	// The path returns to A after the candidate was read. A candidate parsed
	// from B would still fail this exact-byte comparison, even if metadata did
	// not change, while an unchanged A remains clean.
	require.NoError(t, os.WriteFile(path, b, 0o600))
	require.NoError(t, os.WriteFile(path, a, 0o600))
	current, currentFingerprint, err := readStableConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, a, current)
	require.False(t, reloadFingerprintsChanged(map[string]reloadFileFingerprint{normalizeReloadPath(path): currentFingerprint}))
	require.NotEqual(t, sha256.Sum256(b), fingerprint.digest)
	candidateB := fingerprint
	candidateB.digest = sha256.Sum256(b)
	require.True(t, reloadFingerprintsChanged(map[string]reloadFileFingerprint{normalizeReloadPath(path): candidateB}))
}

func TestCycle7StalenessTracksNegativeLookupAndMCPCandidates(t *testing.T) {
	root := t.TempDir()
	store, err := Load(root, filepath.Join(root, "data"), false)
	require.NoError(t, err)
	tracked := store.loadSnapshot().trackedConfigPaths
	require.Contains(t, tracked, normalizeReloadPath(filepath.Join(root, "rush.json")))
	require.Contains(t, tracked, normalizeReloadPath(filepath.Join(root, ".rush.json")))
	require.Contains(t, tracked, normalizeReloadPath(filepath.Join(root, ".mcp.json")))
	require.Contains(t, tracked, normalizeReloadPath(filepath.Join(root, "data", "rush.json")))
	newCandidate := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(newCandidate, []byte(`{"mcp":{}}`), 0o600))
	require.True(t, store.ConfigStaleness().Dirty)
}

func TestCycle7WriterHandoffAfterNonRetryableReloadFailure(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{}}`), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path, workspacePath: filepath.Join(root, "ws.json")})
	store.workingDir = root
	firstHook := make(chan struct{})
	writerQueued := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	store.reloadAfterWriteQueued = func() { close(writerQueued) }

	var calls int
	var hookErr error
	store.reloadAfterDiskRead = func() {
		calls++
		switch calls {
		case 1:
			hookErr = writeStableConfigFile(path, []byte(`{"hooks":{"PreToolUse":[{"matcher":"[bad","command":"true"}]}}`))
			close(firstHook)
			<-release
		case 3:
			// Only the successor repairs the hook; retain the writer's data.
			var data []byte
			data, _, hookErr = readStableConfigFile(path)
			if hookErr != nil {
				return
			}
			var document map[string]any
			hookErr = json.Unmarshal(data, &document)
			if hookErr != nil {
				return
			}
			delete(document, "hooks")
			data, hookErr = json.Marshal(document)
			if hookErr == nil {
				hookErr = writeStableConfigFile(path, data)
			}
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	reloadDone := make(chan error, 1)
	reloadExited := make(chan struct{})
	var writerExited chan struct{}
	defer func() {
		releaseHook()
		cancel()
		<-reloadExited
		if writerExited != nil {
			<-writerExited
		}
	}()
	go func() {
		defer close(reloadExited)
		reloadDone <- store.ReloadFromDisk(ctx)
	}()
	select {
	case <-firstHook:
		require.NoError(t, hookErr)
	case err := <-reloadDone:
		t.Fatalf("reload returned before its handoff hook: %v", err)
	}

	writerDone := make(chan error, 1)
	writerExited = make(chan struct{})
	go func() {
		defer close(writerExited)
		writerDone <- store.SetConfigField(ScopeGlobal, "options.debug", true)
	}()
	select {
	case <-writerQueued:
	case err := <-writerDone:
		t.Fatalf("writer returned before queuing its successor reload: %v", err)
	}
	var written Config
	require.NoError(t, json.Unmarshal(mustReadCycle7File(t, path), &written))
	require.NotNil(t, written.Options)
	require.True(t, written.Options.Debug)
	releaseHook()

	require.ErrorContains(t, <-reloadDone, "invalid hook configuration")
	require.NoError(t, <-writerDone)
	<-writerExited
	require.NoError(t, hookErr)
	require.GreaterOrEqual(t, calls, 3, "the queued writer must build a successor")
	require.True(t, store.Config().Options.Debug, "the successor must publish the writer's value")
	store.reloadPendingMu.Lock()
	require.False(t, store.reloadPending)
	store.reloadPendingMu.Unlock()
}

func TestCycle7WriterHandoffAfterReloadBudgetExhaustion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{}}`), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
	store.workingDir = root
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHook)
	calls := 0
	store.reloadAfterDiskRead = func() {
		once.Do(func() { close(started) })
		if calls == 0 {
			deadline := time.Now().Add(5 * time.Second)
			for {
				select {
				case <-release:
					return
				default:
				}
				data := mustReadCycle7File(t, path)
				if strings.Contains(string(data), "writer") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("writer did not complete its disk write")
				}
				time.Sleep(time.Millisecond)
			}
		}
		calls++
		data := mustReadCycle7File(t, path)
		require.NoError(t, writeStableConfigFile(path, append(data, ' ')))
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- store.ReloadFromDisk(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not reach the handoff hook")
	}
	writerDone := make(chan error, 1)
	go func() { writerDone <- store.SetConfigField(ScopeGlobal, "writer", true) }()
	select {
	case err := <-reloadDone:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		releaseHook()
		t.Fatal("reload did not finish")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			require.ErrorIs(t, err, ErrConfigReloadUnstable)
		}
	case <-time.After(5 * time.Second):
		releaseHook()
		t.Fatal("writer did not finish")
	}
	store.reloadPendingMu.Lock()
	require.False(t, store.reloadPending)
	store.reloadPendingMu.Unlock()
}

func mustReadCycle7File(t *testing.T, path string) []byte {
	t.Helper()
	file, err := openStableConfigFile(path)
	require.NoError(t, err)
	defer file.Close()
	data, err := readOpenedConfigBytes(file)
	require.NoError(t, err)
	return data
}

func setMCPFile(t *testing.T, path, name, url string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{name: MCPConfig{Type: MCPHttp, URL: url}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func setExternalMCPFile(t *testing.T, path, name, url string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: map[string]any{"type": "http", "url": url}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
