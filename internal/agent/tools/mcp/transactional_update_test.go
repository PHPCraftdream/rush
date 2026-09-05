package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func transactionalTestServer(t *testing.T, toolName, text string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: toolName}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: toolName}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
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
	oldHTTP := transactionalTestServer(t, "old-tool", "old")
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

func hasSession(name string) bool {
	_, ok := sessions.Get(name)
	return ok
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
