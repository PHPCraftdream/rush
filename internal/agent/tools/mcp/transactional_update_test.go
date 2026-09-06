package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func transactionalTestServer(t *testing.T, toolName, text string) *httptest.Server {
	t.Helper()
	_, httpServer := transactionalNotifyingServer(t, toolName, text)
	return httpServer
}

func transactionalNotifyingServer(t *testing.T, toolName, text string) (*mcp.Server, *httptest.Server) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: toolName}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: toolName}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
	return server, httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
}

func connectedTransactionalServer(t *testing.T, name string, httpServer *httptest.Server) (*config.ConfigStore, *Owner) {
	t.Helper()
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	owner.Initialize(context.Background(), nil, store, false)
	_, ok := sessions.Get(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, mustState(t, name).State)
	return store, owner
}

func mustState(t *testing.T, name string) ClientInfo {
	t.Helper()
	state, ok := GetState(name)
	require.True(t, ok)
	return state
}

func diskMCP(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.MCP
}

func TestReplaceServerFailedSameNamePreservesLiveServerAndDisk(t *testing.T) {
	oldServer, oldHTTP := transactionalNotifyingServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	store, owner := connectedTransactionalServer(t, "same-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession, ok := sessions.Get("same-name")
	require.True(t, ok)
	oldPrompts := []*Prompt{{Name: "old-prompt"}}
	oldResources := []*Resource{{Name: "old-resource", URI: "old://resource"}}
	allPrompts.Set("same-name", oldPrompts)
	allResources.Set("same-name", oldResources)
	oldDisk := diskMCP(t)
	err := ReplaceServer(context.Background(), store, "same-name", "same-name", config.MCPConfig{
		Type: config.MCPHttp, URL: "http://127.0.0.1:1/unreachable", Timeout: 1,
	})
	require.Error(t, err)

	current, ok := sessions.Get("same-name")
	require.True(t, ok)
	require.Same(t, oldSession, current)
	require.Equal(t, []string{"old-tool"}, GetServerToolNames("same-name"))
	gotPrompts, ok := allPrompts.Get("same-name")
	require.True(t, ok)
	require.Equal(t, oldPrompts, gotPrompts)
	gotResources, ok := allResources.Get("same-name")
	require.True(t, ok)
	require.Equal(t, oldResources, gotResources)
	require.Equal(t, StateConnected, mustState(t, "same-name").State)
	require.NoError(t, current.Ping(context.Background(), nil))
	result, err := RunTool(context.Background(), store, "same-name", "old-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "old", result.Content)
	require.Equal(t, oldDisk, diskMCP(t))
	mcp.AddTool(oldServer, &mcp.Tool{Name: "after-failed-replace"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	require.Eventually(t, func() bool {
		return len(GetServerToolNames("same-name")) == 2
	}, 5*time.Second, time.Millisecond, "failed replacement invalidated the old session notification admission")
}

func TestReplaceServerPersistenceFailurePreservesOldAdmissionAndCallbacks(t *testing.T) {
	oldServer, oldHTTP := transactionalNotifyingServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "same-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	oldSession, ok := sessions.Get("same-name")
	require.True(t, ok)
	oldDisk := diskMCP(t)
	persistErr := errors.New("injected persistence failure")

	err := replaceServerWithPersistence(context.Background(), store, "same-name", "same-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}, func(*config.ConfigStore, string, string, config.MCPConfig) error {
		return persistErr
	})
	require.ErrorIs(t, err, persistErr)
	current, ok := sessions.Get("same-name")
	require.True(t, ok)
	require.Same(t, oldSession, current)
	require.Equal(t, oldDisk, diskMCP(t))

	mcp.AddTool(oldServer, &mcp.Tool{Name: "after-persist-failure"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	require.Eventually(t, func() bool {
		return len(GetServerToolNames("same-name")) == 2
	}, 5*time.Second, time.Millisecond, "persistence failure invalidated the old session notification admission")
}

func TestReplaceServerFailedRenamePreservesLiveServerAndDisk(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession, ok := sessions.Get("old-name")
	require.True(t, ok)
	oldPrompts := []*Prompt{{Name: "old-prompt"}}
	oldResources := []*Resource{{Name: "old-resource", URI: "old://resource"}}
	allPrompts.Set("old-name", oldPrompts)
	allResources.Set("old-name", oldResources)
	oldDisk := diskMCP(t)
	err := ReplaceServer(context.Background(), store, "old-name", "new.name", config.MCPConfig{
		Type: config.MCPHttp, URL: "http://127.0.0.1:1/unreachable", Timeout: 1,
	})
	require.Error(t, err)
	current, ok := sessions.Get("old-name")
	require.True(t, ok)
	require.Same(t, oldSession, current)
	require.False(t, hasSession("new.name"))
	require.Equal(t, []string{"old-tool"}, GetServerToolNames("old-name"))
	gotPrompts, ok := allPrompts.Get("old-name")
	require.True(t, ok)
	require.Equal(t, oldPrompts, gotPrompts)
	gotResources, ok := allResources.Get("old-name")
	require.True(t, ok)
	require.Equal(t, oldResources, gotResources)
	require.Equal(t, StateConnected, mustState(t, "old-name").State)
	require.NoError(t, current.Ping(context.Background(), nil))
	result, err := RunTool(context.Background(), store, "old-name", "old-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "old", result.Content)
	require.Equal(t, oldDisk, diskMCP(t))
}

func TestReplaceServerConditionalTargetCollisionPreservesOldRuntime(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, root := originScopeStore(t)
	oldName := "collision-old.literal#?"
	newName := "collision-new.literal#?"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
		Type: config.MCPHttp, URL: oldHTTP.URL, Timeout: 60,
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	contender, err := config.Init(root, root, false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)
	oldSession := mustSession(t, oldName)
	winner := config.MCPConfig{Type: config.MCPHttp, URL: "http://winner.example"}
	err = replaceServerWithResultPersistence(context.Background(), store, oldName, newName, config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}, func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
		require.NoError(t, contender.PersistMCPConfig(config.ScopeGlobal, newName, winner))
		return cfg.PersistReplaceMCPResult(scope, oldName, newName, mcpCfg)
	})
	require.ErrorIs(t, err, config.ErrMCPTargetExists)
	require.Same(t, oldSession, mustSession(t, oldName))
	require.False(t, hasSession(newName))
	require.Equal(t, winner, requireMCPFileConfig(t, config.GlobalConfigData(), newName))
}

func hasSession(name string) bool {
	_, ok := sessions.Get(name)
	return ok
}

func requireTransactionalEvent(
	t *testing.T,
	events <-chan pubsub.Event[Event],
	eventType pubsub.EventType,
	name string,
	state State,
) {
	t.Helper()
	select {
	case event := <-events:
		require.Equal(t, eventType, event.Type)
		require.Equal(t, EventStateChanged, event.Payload.Type)
		require.Equal(t, name, event.Payload.Name)
		require.Equal(t, state, event.Payload.State)
	case <-time.After(time.Second):
		t.Fatalf("missing %s event for MCP server %q", eventType, name)
	}
}

func TestReplaceServerSuccessfulSwapPublishesNewSessionOnce(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "same-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	oldSession, ok := sessions.Get("same-name")
	require.True(t, ok)
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventCtx)

	require.NoError(t, ReplaceServer(context.Background(), store, "same-name", "same-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	newSession, ok := sessions.Get("same-name")
	require.True(t, ok)
	require.NotSame(t, oldSession, newSession)
	require.Equal(t, []string{"new-tool"}, GetServerToolNames("same-name"))
	require.Equal(t, StateConnected, mustState(t, "same-name").State)
	require.NoError(t, newSession.Ping(context.Background(), nil))
	result, err := RunTool(context.Background(), store, "same-name", "new-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "new", result.Content)
	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)["same-name"], &persisted))
	require.Equal(t, newHTTP.URL, persisted.URL)
	select {
	case event := <-events:
		require.Equal(t, EventStateChanged, event.Payload.Type)
		require.Equal(t, "same-name", event.Payload.Name)
	case <-time.After(time.Second):
		t.Fatal("successful replacement did not publish its state transition")
	}
	select {
	case event := <-events:
		t.Fatalf("successful same-name replacement published an extra event: %v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReplaceServerSuccessfulRenameRemovesOnlyOldRuntimeState(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	allResources.Set("old-name", []*Resource{{Name: "old-resource", URI: "old://resource"}})

	require.NoError(t, ReplaceServer(context.Background(), store, "old-name", "new.name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	_, ok := sessions.Get("old-name")
	require.False(t, ok)
	newSession, ok := sessions.Get("new.name")
	require.True(t, ok)
	require.Equal(t, []string{"new-tool"}, GetServerToolNames("new.name"))
	require.Empty(t, GetServerToolNames("old-name"))
	_, ok = allResources.Get("old-name")
	require.False(t, ok)
	require.False(t, mustStateExists("old-name"))
	require.NoError(t, newSession.Ping(context.Background(), nil))
	result, err := RunTool(context.Background(), store, "new.name", "new-tool", `{}`)
	require.NoError(t, err)
	require.Equal(t, "new", result.Content)
	_, oldPersisted := diskMCP(t)["old-name"]
	_, newPersisted := diskMCP(t)["new.name"]
	require.False(t, oldPersisted)
	require.True(t, newPersisted)
}

func TestReplaceServerDisabledSameNameCommitsInactiveRuntime(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "same-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	allPrompts.Set("same-name", []*Prompt{{Name: "stale-prompt"}})
	allResources.Set("same-name", []*Resource{{Name: "stale-resource", URI: "stale://resource"}})
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	require.NoError(t, ReplaceServer(context.Background(), store, "same-name", "same-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60, Disabled: true,
	}))
	require.False(t, hasSession("same-name"))
	require.Empty(t, GetServerToolNames("same-name"))
	_, hasPrompts := allPrompts.Get("same-name")
	require.False(t, hasPrompts)
	_, hasResources := allResources.Get("same-name")
	require.False(t, hasResources)
	state := mustState(t, "same-name")
	require.Equal(t, StateDisabled, state.State)
	require.Nil(t, state.Client)
	configured, ok := store.MCPConfig("same-name")
	require.True(t, ok)
	require.True(t, configured.Disabled)
	require.Equal(t, newHTTP.URL, configured.URL)
	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)["same-name"], &persisted))
	require.Equal(t, configured, persisted)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, "same-name", StateDisabled)
}

func TestReplaceServerDisabledRenameCommitsInactiveRuntime(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	allPrompts.Set("old-name", []*Prompt{{Name: "stale-prompt"}})
	allResources.Set("old-name", []*Resource{{Name: "stale-resource", URI: "stale://resource"}})
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	require.NoError(t, ReplaceServer(context.Background(), store, "old-name", "new-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60, Disabled: true,
	}))
	for _, name := range []string{"old-name", "new-name"} {
		require.False(t, hasSession(name))
		require.Empty(t, GetServerToolNames(name))
		_, hasPrompts := allPrompts.Get(name)
		require.False(t, hasPrompts)
		_, hasResources := allResources.Get(name)
		require.False(t, hasResources)
	}
	require.False(t, mustStateExists("old-name"))
	state := mustState(t, "new-name")
	require.Equal(t, StateDisabled, state.State)
	require.Nil(t, state.Client)
	_, oldConfigured := store.MCPConfig("old-name")
	configured, newConfigured := store.MCPConfig("new-name")
	require.False(t, oldConfigured)
	require.True(t, newConfigured)
	require.True(t, configured.Disabled)
	_, oldPersisted := diskMCP(t)["old-name"]
	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)["new-name"], &persisted))
	require.False(t, oldPersisted)
	require.Equal(t, configured, persisted)
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, "old-name", StateDisabled)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, "new-name", StateDisabled)
}

func TestReplaceServerConcurrentPostPersistDisableCommitsInactiveRuntime(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	err := replaceServerWithPersistence(context.Background(), store, "old-name", "new-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}, func(cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
		if err := cfg.PersistReplaceMCP(oldName, newName, mcpCfg); err != nil {
			return err
		}
		if err := cfg.PersistMCPDisabledOverride(config.ScopeGlobal, newName, true); err != nil {
			return err
		}
		_, ok := cfg.SetMCPDisabled(newName, true)
		require.True(t, ok)
		return nil
	})
	require.NoError(t, err)
	require.False(t, hasSession("old-name"))
	require.False(t, hasSession("new-name"))
	require.Empty(t, GetServerToolNames("old-name"))
	require.Empty(t, GetServerToolNames("new-name"))
	require.Equal(t, StateDisabled, mustState(t, "new-name").State)
	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)["new-name"], &persisted))
	require.True(t, persisted.Disabled)
}

func TestReplaceServerMissingPostPersistConfigLeavesNoOrphanRuntime(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newHTTP := transactionalTestServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	err := replaceServerWithPersistence(context.Background(), store, "old-name", "new-name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}, func(cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
		if err := cfg.PersistReplaceMCP(oldName, newName, mcpCfg); err != nil {
			return err
		}
		if err := cfg.PersistRemoveMCPConfig(config.ScopeGlobal, newName); err != nil {
			return err
		}
		_, _ = cfg.RemoveMCP(newName)
		return nil
	})
	require.NoError(t, err)
	for _, name := range []string{"old-name", "new-name"} {
		require.False(t, hasSession(name))
		require.Empty(t, GetServerToolNames(name))
		require.False(t, mustStateExists(name))
		_, configured := store.MCPConfig(name)
		require.False(t, configured)
		_, persisted := diskMCP(t)[name]
		require.False(t, persisted)
	}
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, "old-name", StateDisabled)
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, "new-name", StateDisabled)
}

func TestReplacedRenameAdmissionTracksCommittedConfig(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newServer, newHTTP := transactionalNotifyingServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	require.NoError(t, ReplaceServer(context.Background(), store, "old-name", "new.name", config.MCPConfig{
		Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
	}))
	mcp.AddTool(newServer, &mcp.Tool{Name: "valid-after-rename"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	require.Eventually(t, func() bool {
		return len(GetServerToolNames("new.name")) == 2
	}, 5*time.Second, time.Millisecond, "renamed session callback should be valid while committed config is enabled")

	_, ok := store.SetMCPDisabled("new.name", true)
	require.True(t, ok)
	mcp.AddTool(newServer, &mcp.Tool{Name: "ignored-while-disabled"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	time.Sleep(50 * time.Millisecond)
	require.Len(t, GetServerToolNames("new.name"), 2, "disabled committed config must invalidate renamed callbacks")

	_, ok = store.SetMCPDisabled("new.name", false)
	require.True(t, ok)
	_, ok = store.RemoveMCP("new.name")
	require.True(t, ok)
	mcp.AddTool(newServer, &mcp.Tool{Name: "ignored-after-remove"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	time.Sleep(50 * time.Millisecond)
	require.Len(t, GetServerToolNames("new.name"), 2, "removed committed config must invalidate renamed callbacks")
}

func TestReplaceServerDurableCommitDuringCloseHonorsCloseDeadline(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	newServer, newHTTP := transactionalNotifyingServer(t, "new-tool", "new")
	defer oldHTTP.Close()
	defer newHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	durable := make(chan struct{})
	releasePersistence := make(chan struct{})
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- replaceServerWithPersistence(context.Background(), store, "old-name", "new-name", config.MCPConfig{
			Type: config.MCPHttp, URL: newHTTP.URL, Timeout: 60,
		}, func(cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
			err := cfg.PersistReplaceMCP(oldName, newName, mcpCfg)
			close(durable)
			<-releasePersistence
			return err
		})
	}()
	select {
	case <-durable:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach its durable commit")
	}
	mcp.AddTool(newServer, &mcp.Tool{Name: "candidate-only"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	select {
	case event := <-events:
		t.Fatalf("uncommitted replacement candidate published an event: %v", event)
	case <-time.After(50 * time.Millisecond):
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	start := time.Now()
	err := owner.Close(closeCtx)
	cancelClose()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 500*time.Millisecond, "lifecycle mutex blocked Owner.Close past its deadline")
	close(releasePersistence)
	require.NoError(t, <-replaceDone, "durable replacement remains successful when shutdown wins runtime publication")
	require.NoError(t, owner.Close(context.Background()))
	for event := range events {
		require.False(t, event.Payload.Name == "new-name" && event.Payload.State == StateConnected,
			"closing owner published a connected replacement session after durable commit")
	}

	disk := diskMCP(t)
	_, oldExists := disk["old-name"]
	_, newExists := disk["new-name"]
	require.False(t, oldExists)
	require.True(t, newExists)
	require.False(t, hasSession("new-name"), "closing owner must not publish the prepared replacement session")
}

func mustStateExists(name string) bool {
	_, ok := GetState(name)
	return ok
}

func TestReplaceServerConcurrentRemoveRejectsCandidateWithoutStalePublish(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	started := make(chan struct{}, 1)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer candidate.Close()
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- ReplaceServer(context.Background(), store, "old-name", "new-name", config.MCPConfig{
			Type: config.MCPHttp, URL: candidate.URL, Timeout: 60,
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not start candidate connection")
	}
	require.NoError(t, RemoveServer(store, "old-name"))
	require.Error(t, <-replaceDone)
	_, ok := sessions.Get("new-name")
	require.False(t, ok)
	_, ok = store.MCPConfig("old-name")
	require.False(t, ok)
}

func TestReplaceServerConcurrentDisableRejectsCandidateAndKeepsDisabledState(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	started := make(chan struct{}, 1)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer candidate.Close()
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- ReplaceServer(context.Background(), store, "old-name", "new-name", config.MCPConfig{
			Type: config.MCPHttp, URL: candidate.URL, Timeout: 60,
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not start candidate connection")
	}
	require.NoError(t, DisableServer(context.Background(), store, "old-name"))
	require.Error(t, <-replaceDone)
	oldCfg, ok := store.MCPConfig("old-name")
	require.True(t, ok)
	require.True(t, oldCfg.Disabled)
	_, ok = sessions.Get("new-name")
	require.False(t, ok)
}

func TestReplaceServerOwnerCloseRejectsCandidateWithoutCommit(t *testing.T) {
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
	defer oldHTTP.Close()
	store, owner := connectedTransactionalServer(t, "old-name", oldHTTP)

	started := make(chan struct{}, 1)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer candidate.Close()
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- ReplaceServer(context.Background(), store, "old-name", "new-name", config.MCPConfig{
			Type: config.MCPHttp, URL: candidate.URL, Timeout: 60,
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not start candidate connection")
	}
	require.NoError(t, owner.Close(context.Background()))
	require.Error(t, <-replaceDone)
	_, ok := diskMCP(t)["old-name"]
	require.True(t, ok)
	_, ok = diskMCP(t)["new-name"]
	require.False(t, ok)
}
