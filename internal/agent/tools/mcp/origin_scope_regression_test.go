package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func readMCPFile(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.MCP
}

func originScopeStore(t *testing.T) (*config.ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	store, err := config.Init(root, root, false)
	require.NoError(t, err)
	return store, root
}

func requireMCPFileConfig(t *testing.T, path, name string) config.MCPConfig {
	t.Helper()
	raw, ok := readMCPFile(t, path)[name]
	require.True(t, ok, "config file %q must contain MCP server %q", path, name)
	var value config.MCPConfig
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}

func requireMCPFileLacks(t *testing.T, path string, names ...string) {
	t.Helper()
	servers := readMCPFile(t, path)
	for _, name := range names {
		_, ok := servers[name]
		require.False(t, ok, "config file %q must not contain MCP server %q", path, name)
	}
}

func writeExternalMCPFile(t *testing.T, path, name string, value map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: value}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func originTestServer(t *testing.T, toolName, text string) *httptest.Server {
	t.Helper()
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: toolName}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{Content: []modelmcp.Content{&modelmcp.TextContent{Text: text}}}, nil, nil
	})
	return httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
}

func TestReplaceServerWorkspaceLiteralNamePersistsAcrossReloadAndRestart(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	oldName := "foo.disabled"
	newName := "foo.timeout#*?"
	oldHTTP := originTestServer(t, "old-tool", "old")
	newHTTP := originTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()

	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, "foo", config.MCPConfig{
		Type: config.MCPHttp, URL: "http://sibling.example", Timeout: 60,
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	owner.Initialize(context.Background(), nil, store, false)
	oldSession, ok := sessions.Get(oldName)
	require.True(t, ok)

	require.NoError(t, ReplaceServer(context.Background(), store, oldName, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	require.Equal(t, newHTTP.URL, requireMCPFileConfig(t, workspacePath, oldName).URL)
	updated, ok := store.MCPConfig(oldName)
	require.True(t, ok)
	require.Equal(t, newHTTP.URL, updated.URL)
	require.NotSame(t, oldSession, mustSession(t, oldName))

	require.NoError(t, ReplaceServer(context.Background(), store, oldName, newName, config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	requireMCPFileLacks(t, workspacePath, oldName)
	requireMCPFileLacks(t, globalPath, oldName, newName)
	require.Equal(t, newHTTP.URL, requireMCPFileConfig(t, workspacePath, newName).URL)
	require.NotNil(t, readMCPFile(t, workspacePath)["foo"])

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	updated, ok = store.MCPConfig(newName)
	require.True(t, ok)
	require.Equal(t, newHTTP.URL, updated.URL)
	_, ok = store.MCPConfig(oldName)
	require.False(t, ok)
	require.NoError(t, owner.Close(context.Background()))

	restarted, err := config.Init(root, root, false)
	require.NoError(t, err)
	restartedOwner, err := Acquire()
	require.NoError(t, err)
	restartedOwner.Initialize(context.Background(), nil, restarted, false)
	restartedCfg, ok := restarted.MCPConfig(newName)
	require.True(t, ok)
	require.Equal(t, newHTTP.URL, restartedCfg.URL)
	_, ok = restarted.MCPConfig(oldName)
	require.False(t, ok)
	requireMCPFileLacks(t, globalPath, oldName, newName)
	require.NoError(t, restartedOwner.Close(context.Background()))
}

func TestReplaceServerGlobalOriginKeepsGlobalPersistence(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	oldName := "global.foo"
	newName := "global.bar"
	oldHTTP := originTestServer(t, "old-tool", "old")
	newHTTP := originTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)

	require.NoError(t, ReplaceServer(context.Background(), store, oldName, newName, config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	requireMCPFileLacks(t, workspacePath, oldName, newName)
	requireMCPFileLacks(t, globalPath, oldName)
	require.Equal(t, newHTTP.URL, requireMCPFileConfig(t, globalPath, newName).URL)
}

func TestWorkspaceDisableEnableRemovePersistInWorkspaceFile(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	name := "workspace.disabled"
	server := originTestServer(t, "workspace-tool", "workspace")
	defer server.Close()
	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, name, config.MCPConfig{
		Type: config.MCPHttp, URL: server.URL, Timeout: 60,
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)

	require.NoError(t, DisableServer(context.Background(), store, name))
	require.True(t, requireMCPFileConfig(t, workspacePath, name).Disabled)
	requireMCPFileLacks(t, globalPath, name)
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	require.NoError(t, EnableServer(context.Background(), store, name))
	require.Eventually(t, func() bool {
		state, ok := GetState(name)
		return ok && state.State == StateConnected
	}, 5*time.Second, time.Millisecond)
	require.False(t, requireMCPFileConfig(t, workspacePath, name).Disabled)
	requireMCPFileLacks(t, globalPath, name)

	require.NoError(t, RemoveServer(store, name))
	requireMCPFileLacks(t, workspacePath, name)
	requireMCPFileLacks(t, globalPath, name)
}

func TestExternalDisableKeepsFullDefinitionAndUsesWorkspaceOverlay(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	name := "external.foo.bar"
	writeExternalMCPFile(t, filepath.Join(root, ".mcp.json"), name, map[string]any{
		"type": "http", "url": "http://external.example/mcp",
		"headers": map[string]string{"Authorization": "Bearer token"},
	})
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	require.NoError(t, DisableServer(context.Background(), store, name))
	override := requireMCPFileConfig(t, workspacePath, name)
	require.True(t, override.Disabled)
	requireMCPFileLacks(t, globalPath, name)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	external, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, config.MCPSourceExternal, external.Source)
	require.Equal(t, "http://external.example/mcp", external.URL)
	require.Equal(t, map[string]string{"Authorization": "Bearer token"}, external.Headers)
	require.True(t, external.Disabled)
}

func TestReplaceServerProjectOriginRejectsBeforeCandidateConnection(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	projectPath := filepath.Join(root, "rush.json")
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{
		"project.server": config.MCPConfig{Type: config.MCPHttp, URL: "http://project.example", Timeout: 60},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(projectPath, data, 0o600))
	store, err := config.Init(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	var candidateRequests atomic.Int32
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidateRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer candidate.Close()
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	err = ReplaceServer(context.Background(), store, "project.server", "project.renamed", config.MCPConfig{
		Type: config.MCPHttp, URL: candidate.URL, Timeout: 60,
	})
	require.ErrorIs(t, err, config.ErrMCPUnwritableOrigin)
	require.Zero(t, candidateRequests.Load())
}

func TestProjectMCPMutationsFailClosed(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	projectPath := filepath.Join(root, "rush.json")
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{
		"project.mutations": config.MCPConfig{Type: config.MCPHttp, URL: "http://project.example", Timeout: 60},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(projectPath, data, 0o600))
	store, err := config.Init(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	require.ErrorIs(t, DisableServer(context.Background(), store, "project.mutations"), config.ErrMCPUnwritableOrigin)
	require.ErrorIs(t, EnableServer(context.Background(), store, "project.mutations"), config.ErrMCPUnwritableOrigin)
	require.ErrorIs(t, RemoveServer(store, "project.mutations"), config.ErrMCPUnwritableOrigin)
	requireMCPFileLacks(t, config.GlobalConfigData(), "project.mutations")
}

func TestReplaceServerRejectsScopeChangeBeforeDurableWrite(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	oldName := "scope-change"
	oldHTTP := originTestServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)
	oldSession, ok := sessions.Get(oldName)
	require.True(t, ok)

	candidateServer := modelmcp.NewServer(&modelmcp.Implementation{Name: "candidate"}, nil)
	modelmcp.AddTool(candidateServer, &modelmcp.Tool{Name: "candidate-tool"}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{}, nil, nil
	})
	delegate := modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return candidateServer }, nil)
	var changed sync.Once
	mutationErr := make(chan error, 1)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		changed.Do(func() {
			if err := store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
				Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
			}); err != nil {
				mutationErr <- err
				return
			}
			if err := store.PersistRemoveMCPConfig(config.ScopeWorkspace, oldName); err != nil {
				mutationErr <- err
			}
		})
		delegate.ServeHTTP(w, r)
	}))
	defer candidate.Close()

	err = ReplaceServer(context.Background(), store, oldName, "scope-change-new", config.MCPConfig{
		Type: config.MCPHttp, URL: candidate.URL, Timeout: 60,
	})
	require.Error(t, err)
	select {
	case mutationErrValue := <-mutationErr:
		require.NoError(t, mutationErrValue)
	default:
	}
	require.Same(t, oldSession, mustSession(t, oldName))
	requireMCPFileLacks(t, workspacePath, "scope-change-new")
	requireMCPFileLacks(t, globalPath, "scope-change-new")

	// Restore the fixture so the postcondition is checked against the original
	// origin, not the deliberate disk mutation used to trigger the race.
	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
	require.NoError(t, store.PersistRemoveMCPConfig(config.ScopeGlobal, oldName))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	updated, ok := store.MCPConfig(oldName)
	require.True(t, ok)
	require.Equal(t, oldHTTP.URL, updated.URL)
}

func mustSession(t *testing.T, name string) *ClientSession {
	t.Helper()
	session, ok := sessions.Get(name)
	require.True(t, ok)
	return session
}
