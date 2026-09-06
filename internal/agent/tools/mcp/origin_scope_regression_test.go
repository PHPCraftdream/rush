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

func writeRushMCPDefinition(t *testing.T, path, name string, value config.MCPConfig) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcp": map[string]any{name: value}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func originTestServer(t *testing.T, toolName, text string) *httptest.Server {
	t.Helper()
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: toolName}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{Content: []modelmcp.Content{&modelmcp.TextContent{Text: text}}}, nil, nil
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)
	return httpServer
}

func TestReplaceServerWorkspaceLiteralNamePersistsAcrossReloadAndRestart(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	oldName := "foo.disabled"
	newName := "foo.timeout#*?"
	oldHTTP := originTestServer(t, "old-tool", "old")
	newHTTP := originTestServer(t, "new-tool", "new")

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

func TestAddServerConditionalCollisionPreservesTargetAndOwnRuntime(t *testing.T) {
	store, root := originScopeStore(t)
	contender, err := config.Init(root, root, false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "collision.literal#?"
	candidateReady := make(chan struct{})
	releaseCandidate := make(chan struct{})
	addDone := make(chan error, 1)
	candidate := config.MCPConfig{Type: config.MCPHttp, URL: "http://candidate.example"}
	go func() {
		addDone <- addServerWithInitializer(context.Background(), store, name, candidate,
			func(_ context.Context, cfg *config.ConfigStore, serverName string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) error {
				defer admission.done()
				if err := publishPreparedClient(cfg, serverName, &preparedClient{session: &ClientSession{}}, admission); err != nil {
					return err
				}
				close(candidateReady)
				<-releaseCandidate
				return nil
			})
	}()
	select {
	case <-candidateReady:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate did not reach the durable commit seam")
	}
	winner := config.MCPConfig{Type: config.MCPHttp, URL: "http://winner.example"}
	require.NoError(t, contender.PersistMCPConfig(config.ScopeGlobal, name, winner))
	close(releaseCandidate)
	err = <-addDone
	require.ErrorIs(t, err, config.ErrMCPTargetExists)
	require.Equal(t, winner, requireMCPFileConfig(t, config.GlobalConfigData(), name))
	require.False(t, hasSession(name), "a failed add must close only its own candidate session")
	_, ok := GetState(name)
	require.False(t, ok)
}

func TestWorkspaceRemovalAndReplaceStartRevealedFallbacks(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "workspace-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "global-data"))
	store, err := config.Init(root, dataDir, false)
	require.NoError(t, err)
	workspacePath := filepath.Join(dataDir, "rush.json")
	globalPath := config.GlobalConfigData()

	globalServer := originTestServer(t, "global-fallback-tool", "global-fallback")
	projectServer := originTestServer(t, "project-fallback-tool", "project-fallback")
	externalServer := originTestServer(t, "external-fallback-tool", "external-fallback")
	replaceServer := originTestServer(t, "replace-fallback-tool", "replace-fallback")
	newServer := originTestServer(t, "replacement-tool", "replacement")

	globalName := "fallback.global#?"
	projectName := "fallback.project#?"
	externalName := "fallback.external#?"
	replaceName := "fallback.replace#?"
	disabledName := "fallback.disabled#?"
	writeRushMCPDefinition(t, globalPath, globalName, config.MCPConfig{Type: config.MCPHttp, URL: globalServer.URL, Timeout: 60})
	writeRushMCPDefinition(t, filepath.Join(root, "rush.json"), projectName, config.MCPConfig{Type: config.MCPHttp, URL: projectServer.URL, Timeout: 60})
	writeExternalMCPFile(t, filepath.Join(root, ".mcp.json"), externalName, map[string]any{"type": "http", "url": externalServer.URL})
	writeRushMCPDefinition(t, filepath.Join(root, "rush.json"), replaceName, config.MCPConfig{Type: config.MCPHttp, URL: replaceServer.URL, Timeout: 60})
	writeRushMCPDefinition(t, filepath.Join(root, "rush.json"), disabledName, config.MCPConfig{Type: config.MCPHttp, URL: "http://disabled-fallback.example", Disabled: true})
	workspaceDefs := map[string]config.MCPConfig{
		globalName:   {Type: config.MCPHttp, URL: "http://workspace-global.example", Timeout: 60},
		projectName:  {Type: config.MCPHttp, URL: "http://workspace-project.example", Timeout: 60},
		externalName: {Type: config.MCPHttp, URL: "http://workspace-external.example", Timeout: 60},
		replaceName:  {Type: config.MCPHttp, URL: "http://workspace-replace.example", Timeout: 60},
		disabledName: {Type: config.MCPHttp, URL: "http://workspace-disabled.example", Timeout: 60},
	}
	data, err := json.Marshal(map[string]any{"mcp": workspaceDefs})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(workspacePath), 0o755))
	require.NoError(t, os.WriteFile(workspacePath, data, 0o600))
	// Preserve the disabled project fallback alongside the other project
	// definitions by rewriting the project document with every entry.
	projectDefs := map[string]config.MCPConfig{
		projectName:  {Type: config.MCPHttp, URL: projectServer.URL, Timeout: 60},
		replaceName:  {Type: config.MCPHttp, URL: replaceServer.URL, Timeout: 60},
		disabledName: {Type: config.MCPHttp, URL: "http://disabled-fallback.example", Disabled: true},
	}
	data, err = json.Marshal(map[string]any{"mcp": projectDefs})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "rush.json"), data, 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)

	for _, test := range []struct {
		name string
		tool string
		want string
	}{
		{globalName, "global-fallback-tool", "global-fallback"},
		{projectName, "project-fallback-tool", "project-fallback"},
		{externalName, "external-fallback-tool", "external-fallback"},
	} {
		require.NoError(t, RemoveServer(store, test.name))
		require.Eventually(t, func() bool {
			state, ok := GetState(test.name)
			return ok && state.State == StateConnected
		}, 5*time.Second, time.Millisecond)
		result, callErr := RunTool(context.Background(), store, test.name, test.tool, `{}`)
		require.NoError(t, callErr)
		require.Equal(t, test.want, result.Content)
	}

	require.NoError(t, ReplaceServer(context.Background(), store, replaceName, "replacement.literal#?", config.MCPConfig{
		Type: config.MCPHttp, URL: newServer.URL, Timeout: 60,
	}))
	require.Eventually(t, func() bool {
		state, ok := GetState(replaceName)
		return ok && state.State == StateConnected
	}, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		state, ok := GetState("replacement.literal#?")
		return ok && state.State == StateConnected
	}, 5*time.Second, time.Millisecond)
	result, err := RunTool(context.Background(), store, replaceName, "replace-fallback-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "replace-fallback", result.Content)
	result, err = RunTool(context.Background(), store, "replacement.literal#?", "replacement-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "replacement", result.Content)

	require.NoError(t, RemoveServer(store, disabledName))
	require.Eventually(t, func() bool {
		state, ok := GetState(disabledName)
		return ok && state.State == StateDisabled
	}, 5*time.Second, time.Millisecond)
	require.False(t, hasSession(disabledName))
}

func TestOwnerCloseFencesRevealedFallbackInitialization(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "workspace-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "global-data"))
	store, err := config.Init(root, dataDir, false)
	require.NoError(t, err)
	name := "fallback.close#?"
	projectPath := filepath.Join(root, "rush.json")
	workspacePath := filepath.Join(dataDir, "rush.json")
	started := make(chan struct{})
	canceled := make(chan struct{})
	releaseHandler := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "fallback-close"}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: "fallback-close-tool"}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			canceledOnce.Do(func() { close(canceled) })
			return
		case <-releaseHandler:
			canceledOnce.Do(func() { close(canceled) })
			return
		}
	}))
	defer httpServer.Close()
	writeRushMCPDefinition(t, projectPath, name, config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60})
	writeRushMCPDefinition(t, workspacePath, name, config.MCPConfig{Type: config.MCPHttp, URL: "http://workspace-shadow.example", Timeout: 60})
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	owner, err := Acquire()
	require.NoError(t, err)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	require.NoError(t, RemoveServer(store, name))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("revealed fallback did not begin initialization")
	}
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateStarting, state.State)
	require.NoError(t, owner.Close(context.Background()))
	close(releaseHandler)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("owner close did not cancel fallback initialization")
	}
	require.False(t, hasSession(name))
	_, ok = GetState(name)
	require.False(t, ok)
	for event := range events {
		require.NotEqual(t, StateConnected, event.Payload.State)
	}
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

func TestBlockedProjectInitializerIsNotPendingGlobalAdd(t *testing.T) {
	tests := map[string]func(context.Context, *config.ConfigStore, string, *atomic.Int32) error{
		"disable": func(ctx context.Context, store *config.ConfigStore, name string, attempts *atomic.Int32) error {
			return disableServerWithPersistence(ctx, store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string, _ *config.MCPConfig) error {
					attempts.Add(1)
					return cfg.PersistMCPDisabledOverride(scope, serverName, true)
				})
		},
		"remove": func(_ context.Context, store *config.ConfigStore, name string, attempts *atomic.Int32) error {
			return removeServerWithScopedPersistence(store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string) error {
					attempts.Add(1)
					return cfg.PersistRemoveMCPConfig(scope, serverName)
				})
		},
	}
	for testName, mutate := range tests {
		t.Run(testName, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(root, "global-config")
			dataDir := filepath.Join(root, "global-data")
			t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("RUSH_GLOBAL_DATA", dataDir)
			t.Setenv("XDG_DATA_HOME", dataDir)

			server := modelmcp.NewServer(&modelmcp.Implementation{Name: "blocked-project"}, nil)
			modelmcp.AddTool(server, &modelmcp.Tool{Name: "project-tool"}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
				return &modelmcp.CallToolResult{}, nil, nil
			})
			delegate := modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil)
			started := make(chan struct{})
			canceled := make(chan struct{}, 1)
			release := make(chan struct{})
			var blockOnce sync.Once
			var releaseOnce sync.Once
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first := false
				blockOnce.Do(func() {
					first = true
					close(started)
				})
				if first {
					select {
					case <-release:
					case <-r.Context().Done():
						signalStarted(canceled)
						return
					}
				}
				delegate.ServeHTTP(w, r)
			}))
			cleanupTestMCPServer(t, server, httpServer)

			name := "project.blocked." + testName
			projectData, err := json.Marshal(map[string]any{"mcp": map[string]any{
				name: config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60},
			}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "rush.json"), projectData, 0o600))
			store, err := config.Init(root, filepath.Join(root, "workspace-data"), false)
			require.NoError(t, err)
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() {
				releaseOnce.Do(func() { close(release) })
				require.NoError(t, owner.Close(context.Background()))
			}()

			initializeDone := make(chan struct{})
			go func() {
				owner.Initialize(context.Background(), nil, store, false)
				close(initializeDone)
			}()
			waitForRequest(t, started)
			require.False(t, hasPendingGlobalAdd(name))

			var persistenceAttempts atomic.Int32
			err = mutate(context.Background(), store, name, &persistenceAttempts)
			require.ErrorIs(t, err, config.ErrMCPUnwritableOrigin)
			require.Zero(t, persistenceAttempts.Load())
			requireMCPFileLacks(t, config.GlobalConfigData(), name)
			select {
			case <-canceled:
				t.Fatal("scope rejection canceled the blocked project initializer")
			default:
			}

			releaseOnce.Do(func() { close(release) })
			select {
			case <-initializeDone:
			case <-time.After(5 * time.Second):
				t.Fatal("project initializer did not finish after release")
			}
			require.Equal(t, StateConnected, mustState(t, name).State)
		})
	}
}

func TestReplaceServerRejectsScopeChangeBeforeDurableWrite(t *testing.T) {
	store, root := originScopeStore(t)
	workspacePath := filepath.Join(root, "rush.json")
	globalPath := config.GlobalConfigData()
	oldName := "scope-change"
	oldHTTP := originTestServer(t, "old-tool", "old")
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
			if err := store.PersistRemoveMCPConfig(config.ScopeWorkspace, oldName); err != nil {
				mutationErr <- err
				return
			}
			if err := store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
				Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
			}); err != nil {
				mutationErr <- err
			}
		})
		delegate.ServeHTTP(w, r)
	}))
	cleanupTestMCPServer(t, candidateServer, candidate)

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
	require.NoError(t, store.PersistRemoveMCPConfig(config.ScopeGlobal, oldName))
	require.NoError(t, store.PersistMCPConfig(config.ScopeWorkspace, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
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
