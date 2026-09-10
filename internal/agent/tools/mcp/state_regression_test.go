package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func isolatedMCPStore(t *testing.T) *config.ConfigStore {
	t.Helper()
	// Isolated lifecycle fixtures must not fetch provider metadata. Keep this
	// set before config.Init, which otherwise starts Catwalk and Hyper loads.
	t.Setenv("RUSH_DISABLE_DEFAULT_PROVIDERS", "1")
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	store, err := config.Init(root, root, false)
	require.NoError(t, err)
	return store
}

func cleanupTestMCPServer(t *testing.T, server *mcp.Server, httpServer *httptest.Server) {
	t.Helper()
	t.Cleanup(func() {
		for session := range server.Sessions() {
			_ = session.Close()
		}
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
}

func persistedMCPStore(t *testing.T, name, url string, disabled bool) *config.ConfigStore {
	t.Helper()
	store := isolatedMCPStore(t)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name, config.MCPConfig{
		Type:     config.MCPHttp,
		URL:      url,
		Disabled: disabled,
		Timeout:  60,
	}))
	return store
}

func waitForRequest(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP initialization did not reach the blocked request")
	}
}

func TestDisableServerCancelsBlockedInitAndLeavesNoLateSession(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			signalStarted(canceled)
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	const name = "blocked-disable"
	store := persistedMCPStore(t, name, server.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	finished := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(finished)
	}()
	waitForRequest(t, started)
	lifecycleMu.Lock()
	_, admitted := owner.serverCancels[name]
	lifecycleMu.Unlock()
	require.True(t, admitted, "blocked initialization must retain its cancellation admission")

	require.NoError(t, DisableServer(context.Background(), store, name))
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("disable did not cancel the blocked HTTP request")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disable did not cancel the blocked initializer")
	}
	_, ok := sessions.Get(name)
	require.False(t, ok)
	m, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.True(t, m.Disabled)
}

func TestRemoveServerCancelsBlockedInitAndRejectsLateCommit(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()
	const name = "blocked-remove"
	store := persistedMCPStore(t, name, server.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	finished := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(finished)
	}()
	waitForRequest(t, started)

	require.NoError(t, RemoveServer(store, name))
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("remove did not cancel the blocked HTTP request")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("remove did not cancel the blocked initializer")
	}
	_, ok := store.MCPConfig(name)
	require.False(t, ok)
	_, ok = sessions.Get(name)
	require.False(t, ok)
}

func TestFailedDisableDoesNotCancelStartupCandidate(t *testing.T) {
	testFailedServerMutation(t, false)
}

func TestFailedRemoveDoesNotCancelStartupCandidate(t *testing.T) {
	testFailedServerMutation(t, true)
}

func testFailedServerMutation(t *testing.T, remove bool) {
	t.Helper()
	const name = "failed-persistence-candidate"
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }) }()
	canceled := make(chan struct{}, 1)
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(),
			store,
			name,
			config.MCPConfig{Type: config.MCPStdio, Command: "unused"},
			func(
				_ context.Context,
				cfg *config.ConfigStore,
				serverName string,
				_ config.MCPConfig,
				_ config.VariableResolver,
				admission *serverAdmission,
			) error {
				defer admission.done()
				updateAdmissionState(admission, StateStarting, nil, nil, Counts{})
				close(started)
				select {
				case <-admission.ctx.Done():
					signalStarted(canceled)
					return admission.ctx.Err()
				case <-release:
				}
				return publishPreparedClient(cfg, serverName, &preparedClient{
					session: &ClientSession{},
				}, admission)
			},
		)
	}()
	waitForRequest(t, started)
	persistErr := errors.New("injected persistence failure")
	if remove {
		err = removeServerWithPersistence(store, name, func(*config.ConfigStore, string) error {
			return persistErr
		})
	} else {
		err = disableServerWithPersistence(context.Background(), store, name,
			func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) error { return persistErr })
	}
	require.ErrorIs(t, err, persistErr)
	requireMCPFileLacks(t, config.GlobalConfigData(), name)
	select {
	case <-canceled:
		t.Fatal("failed persistence canceled the startup candidate")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-addDone)
	current, ok := sessions.Get(name)
	require.True(t, ok)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.Same(t, current, state.Client)
}

func TestEnableServerFromOldOwnerCannotPublishIntoNewOwner(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()
	const name = "old-owner-enable"
	store := persistedMCPStore(t, name, server.URL, true)
	oldOwner, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, EnableServer(context.Background(), store, name))
	waitForRequest(t, started)
	require.NoError(t, oldOwner.Close(context.Background()))
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("owner close did not cancel the blocked HTTP request")
	}

	newOwner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, newOwner.Close(context.Background())) }()
	_, ok := sessions.Get(name)
	require.False(t, ok, "an initializer admitted by the old owner must not cross the owner fence")
}

func TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	transport, err := cloneHTTPTransport()
	require.NoError(t, err)
	rt := &headerRoundTripper{ctx: ownerCtx, transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewReader([]byte(`{"jsonrpc":"2.0"}`)))
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		// The point of this test is that ownerCancel unblocks RoundTrip, so
		// the response is normally nil here. Close it on the path where the
		// race resolves the other way, rather than leaking a connection.
		resp, err := rt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	waitForRequest(t, started)

	ownerCancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("transport cancellation did not reach the blocked HTTP request")
	}
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("transport RoundTrip did not return after cancellation")
	}
}

func TestListChangedRefreshIsAsyncAndStaleAdmissionIsIgnored(t *testing.T) {
	const name = "notification-refresh"
	server := mcp.NewServer(&mcp.Implementation{Name: "notification-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "advertised"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "notification-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)
	defer clientCancel()

	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session})
	admission, err := owner.admitServer(context.Background(), store, name, true)
	require.NoError(t, err)
	defer admission.done()

	start := time.Now()
	notifyListChanged(&admission, name, refreshToolsKind)
	require.Less(t, time.Since(start), 100*time.Millisecond, "SDK callback must not synchronously take the MCP lease")
	require.Eventually(t, func() bool {
		return len(GetServerToolNames(name)) == 1
	}, time.Second, time.Millisecond)

	allTools.Set(name, []*Tool{{Name: "keep"}})
	owner.invalidateServer(name)
	notifyListChanged(&admission, name, refreshToolsKind)
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, []string{"keep"}, GetServerToolNames(name), "a stale notification must not refresh the registry")
}

func TestCandidateListChangedIsRetainedUntilExactCommit(t *testing.T) {
	const name = "candidate-notification"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession := &ClientSession{}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})
	admission, err := owner.admitReplacementCandidate(context.Background(), store, name)
	require.NoError(t, err)
	admission.suppressUntilCommit = true
	defer admission.done()

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	notifyListChanged(&admission, name, refreshToolsKind)
	select {
	case event := <-events:
		t.Fatalf("uncommitted candidate published an event: %v", event)
	default:
	}

	request, pending := func() (refreshRequest, bool) {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		for _, request := range owner.refreshPending {
			if request.candidateToken == admission.serverCancelToken {
				return request, true
			}
		}
		return refreshRequest{}, false
	}()
	require.True(t, pending)
	require.True(t, request.deferUntilCommit)
	require.False(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		_, running := owner.refreshRunning[refreshKeyForRequest(request)]
		return running
	}(), "pre-commit notification must not refresh the old session")
	_, ok := owner.nextRefresh()
	require.False(t, ok, "a deferred candidate notification must wait for its commit")
}

func TestFailedCandidateListChangedDropsDeferredEvent(t *testing.T) {
	const name = "failed-candidate-notification"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	session := &ClientSession{}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{})
	admission, err := owner.admitReplacementCandidate(context.Background(), store, name)
	require.NoError(t, err)
	admission.suppressUntilCommit = true

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	notifyListChanged(&admission, name, refreshToolsKind)
	admission.done()

	lifecycleMu.Lock()
	for _, request := range owner.refreshPending {
		require.NotEqual(t, admission.serverCancelToken, request.candidateToken)
	}
	lifecycleMu.Unlock()
	_, ok := owner.nextRefresh()
	require.False(t, ok, "failed candidate left a refresh queued")
	select {
	case event := <-events:
		t.Fatalf("failed candidate published an event: %v", event)
	default:
	}
}

func TestCandidateListChangedPublishesAfterCommitAndRefreshesNewSession(t *testing.T) {
	const name = "candidate-commit-notification"
	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	mcp.AddTool(oldServer, &mcp.Tool{Name: "old-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	var oldListCalls atomic.Int32
	oldServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				oldListCalls.Add(1)
			}
			return next(ctx, method, request)
		}
	})
	oldServerTransport, oldClientTransport := mcp.NewInMemoryTransports()
	oldServerSession, err := oldServer.Connect(context.Background(), oldServerTransport, nil)
	require.NoError(t, err)
	defer oldServerSession.Close()
	oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(context.Background(), oldClientTransport, nil)
	require.NoError(t, err)

	newServer := mcp.NewServer(&mcp.Implementation{Name: "new-server"}, nil)
	mcp.AddTool(newServer, &mcp.Tool{Name: "new-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	var newListCalls atomic.Int32
	newServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, request)
			if method == "tools/list" {
				newListCalls.Add(1)
			}
			return result, err
		}
	})
	newServerTransport, newClientTransport := mcp.NewInMemoryTransports()
	newServerSession, err := newServer.Connect(context.Background(), newServerTransport, nil)
	require.NoError(t, err)
	defer newServerSession.Close()
	newClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "new-client"}, nil).
		Connect(context.Background(), newClientTransport, nil)
	require.NoError(t, err)

	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	oldSession := &ClientSession{ClientSession: oldClientSession}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})
	allTools.Set(name, []*Tool{{Name: "old-tool"}})

	admission, err := owner.admitReplacementCandidate(context.Background(), store, name)
	require.NoError(t, err)
	admission.suppressUntilCommit = true
	defer admission.done()
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	initialAdmission := store.SnapshotMCPAdmission(name)
	admission.mcpAdmission = initialAdmission
	admission.mcpRevision = initialAdmission.MCPRevision
	admission.resolverRevision = initialAdmission.ResolverRevision
	notifyListChanged(&admission, name, refreshToolsKind)
	select {
	case event := <-events:
		t.Fatalf("uncommitted candidate published an event: %v", event)
	default:
	}
	_, changed := store.UpdateMCP(name, func(*config.MCPConfig) {})
	require.True(t, changed)
	finalAdmission := store.SnapshotMCPAdmission(name)
	admission.mcpAdmission = finalAdmission
	admission.mcpRevision = finalAdmission.MCPRevision
	admission.resolverRevision = finalAdmission.ResolverRevision

	newSession := &ClientSession{ClientSession: newClientSession}
	require.NoError(t, owner.commitRenewal(&admission, name, newSession, Counts{}))

	var rawEvents int
	var refreshPublished bool
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for !refreshPublished {
		select {
		case event := <-events:
			if event.Payload.Type == EventToolsListChanged {
				rawEvents++
				require.Equal(t, name, event.Payload.Name)
			}
			if event.Payload.Type == EventStateChanged && event.Payload.State == StateConnected && event.Payload.Counts.Tools == 1 {
				refreshPublished = true
			}
		case <-deadline.C:
			t.Fatal("committed candidate did not publish its deferred notification")
		}
	}
	require.Equal(t, 1, rawEvents, "candidate notification published more than once")
	require.Equal(t, int32(1), newListCalls.Load())
	require.Zero(t, oldListCalls.Load(), "candidate refresh used the old session")
	require.Equal(t, []string{"new-tool"}, GetServerToolNames(name))
}

func TestReplacementCandidateListChangedUsesCommittedDestinationAdmission(t *testing.T) {
	const name = "replacement-candidate-notification"
	store := isolatedMCPStore(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{Type: config.MCPStdio, Command: "old"}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	oldTransport, oldClientTransport := mcp.NewInMemoryTransports()
	oldServerSession, err := oldServer.Connect(context.Background(), oldTransport, nil)
	require.NoError(t, err)
	defer oldServerSession.Close()
	oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(context.Background(), oldClientTransport, nil)
	require.NoError(t, err)

	newServer := mcp.NewServer(&mcp.Implementation{Name: "new-server"}, nil)
	mcp.AddTool(newServer, &mcp.Tool{Name: "prepared-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	var newListCalls atomic.Int32
	newServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, request)
			if method == "tools/list" {
				newListCalls.Add(1)
			}
			return result, err
		}
	})
	newTransport, newClientTransport := mcp.NewInMemoryTransports()
	newServerSession, err := newServer.Connect(context.Background(), newTransport, nil)
	require.NoError(t, err)
	defer newServerSession.Close()
	newClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "new-client"}, nil).
		Connect(context.Background(), newClientTransport, nil)
	require.NoError(t, err)

	oldSession := &ClientSession{ClientSession: oldClientSession}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{Tools: 1})
	allTools.Set(name, []*Tool{{Name: "old-tool"}})

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	prepare := func(_ context.Context, _ *config.ConfigStore, serverName string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) (*preparedClient, error) {
		notifyListChanged(admission, serverName, refreshToolsKind)
		mcp.AddTool(newServer, &mcp.Tool{Name: "committed-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
		return &preparedClient{
			session: &ClientSession{ClientSession: newClientSession},
			tools:   []*Tool{{Name: "prepared-tool"}},
		}, nil
	}

	err = replaceServerWithResultPersistenceAndPreparation(
		context.Background(), store, name, name,
		config.MCPConfig{Type: config.MCPStdio, Command: "new"},
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
		}, prepare,
	)
	require.NoError(t, err)

	var listChanged int
	require.Eventually(t, func() bool {
		select {
		case event := <-events:
			if event.Payload.Name == name && event.Payload.Type == EventToolsListChanged {
				listChanged++
			}
		default:
		}
		tools := GetServerToolNames(name)
		return listChanged == 1 && slices.Contains(tools, "committed-tool")
	}, 2*time.Second, time.Millisecond)
	require.Equal(t, int32(1), newListCalls.Load())
	require.NotContains(t, GetServerToolNames(name), "old-tool")
}

func TestAddServerRetainsLeaseAcrossRemoveDuringInitialization(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()

	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "add-remove-lease"
	addDone := make(chan error, 1)
	go func() {
		addDone <- AddServer(context.Background(), store, name, config.MCPConfig{
			Type:    config.MCPHttp,
			URL:     server.URL,
			Timeout: 60,
		})
	}()
	waitForRequest(t, started)

	leases.mu.Lock()
	addLease, retained := leases.entries[name]
	refsDuringInit := 0
	if retained {
		refsDuringInit = addLease.refs
	}
	leases.mu.Unlock()
	require.True(t, retained, "AddServer must keep its lease registered while initialization runs unlocked")
	require.Equal(t, 1, refsDuringInit)

	removeDone := make(chan error, 1)
	go func() { removeDone <- RemoveServer(store, name) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveServer did not cancel AddServer's blocked request")
	}
	require.NoError(t, <-removeDone)
	require.Error(t, <-addDone)

	leases.mu.Lock()
	remaining := leases.entries[name]
	finalRefs := addLease.refs
	leases.mu.Unlock()
	require.Nil(t, remaining, "the final release must reclaim the original lease")
	require.Zero(t, finalRefs, "the Add/Remove sequence must not underflow lease references")
}

func TestRemoveDisabledFallbackPublishesOneStateEvent(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "remove-disabled-fallback-event"
	configured := config.MCPConfig{Type: config.MCPStdio, Command: name}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, configured))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{})

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	disabled := configured
	disabled.Disabled = true
	err = removeServerWithResultPersistence(store, name, func(
		*config.ConfigStore,
		config.Scope,
		string,
	) (config.MCPMutationResult, error) {
		return config.MCPMutationResult{
			Operation: "remove",
			OldName:   name,
			NewName:   name,
			NewExists: true,
			NewConfig: disabled,
		}, nil
	})
	require.NoError(t, err)

	select {
	case event := <-events:
		require.Equal(t, pubsub.UpdatedEvent, event.Type)
		require.Equal(t, EventStateChanged, event.Payload.Type)
		require.Equal(t, name, event.Payload.Name)
		require.Equal(t, StateDisabled, event.Payload.State)
	case <-time.After(time.Second):
		t.Fatal("disabled fallback did not publish its state event")
	}
	select {
	case event := <-events:
		t.Fatalf("disabled fallback published duplicate state event: %v", event)
	default:
	}
}

func TestPendingAddMutationCommitsCompleteConfigBeforeInvalidatingAdd(t *testing.T) {
	tests := map[string]struct {
		mutate func(*config.ConfigStore, string) error
		check  func(*testing.T, string)
	}{
		"disable": {
			mutate: func(store *config.ConfigStore, name string) error {
				return DisableServer(context.Background(), store, name)
			},
			check: func(t *testing.T, name string) {
				data, err := os.ReadFile(config.GlobalConfigData())
				require.NoError(t, err)
				var root struct {
					MCP map[string]config.MCPConfig `json:"mcp"`
				}
				require.NoError(t, json.Unmarshal(data, &root))
				persisted, ok := root.MCP[name]
				require.True(t, ok)
				require.Equal(t, config.MCPHttp, persisted.Type)
				require.Equal(t, "http://pending-add.example", persisted.URL)
				require.True(t, persisted.Disabled)
			},
		},
		"remove": {
			mutate: func(store *config.ConfigStore, name string) error {
				return RemoveServer(store, name)
			},
			check: func(t *testing.T, name string) {
				data, err := os.ReadFile(config.GlobalConfigData())
				require.NoError(t, err)
				var root struct {
					MCP map[string]json.RawMessage `json:"mcp"`
				}
				require.NoError(t, json.Unmarshal(data, &root))
				_, ok := root.MCP[name]
				require.False(t, ok)
			},
		},
	}
	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			store := isolatedMCPStore(t)
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close(context.Background())) }()

			const name = "pending-add-mutation"
			release := make(chan struct{})
			started := make(chan struct{})
			addDone := make(chan error, 1)
			go func() {
				addDone <- addServerWithInitializer(
					context.Background(), store, name,
					config.MCPConfig{Type: config.MCPHttp, URL: "http://pending-add.example"},
					func(
						_ context.Context,
						cfg *config.ConfigStore,
						serverName string,
						_ config.MCPConfig,
						_ config.VariableResolver,
						admission *serverAdmission,
					) error {
						if err := publishPreparedClient(cfg, serverName, &preparedClient{
							session: &ClientSession{},
						}, admission); err != nil {
							return err
						}
						// The initializer signals done before durable Add persistence;
						// Add retains the admission through its durable decision.
						admission.done()
						close(started)
						<-release
						return nil
					},
				)
			}()
			waitForRequest(t, started)

			lifecycleMu.Lock()
			transaction := owner.pendingGlobalAdds[name]
			lifecycleMu.Unlock()
			require.NotNil(t, transaction)
			select {
			case <-transaction.done:
				t.Fatal("pending Add transaction finished before the durable Add decision")
			default:
			}

			require.NoError(t, test.mutate(store, name))
			test.check(t, name)
			select {
			case <-transaction.done:
				t.Fatal("pending Add transaction finished before the durable Add rollback")
			default:
			}

			close(release)
			require.ErrorIs(t, <-addDone, ErrOwnerBusy)
			select {
			case <-transaction.done:
			case <-time.After(time.Second):
				t.Fatal("pending Add transaction was not cleaned up")
			}
			require.False(t, hasPendingGlobalAdd(name))
		})
	}
}
