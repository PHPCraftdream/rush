package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func isolatedMCPStore(t *testing.T) *config.ConfigStore {
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
	return store
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()
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
	req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader([]byte(`{"jsonrpc":"2.0"}`)))
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		_, err := rt.RoundTrip(req)
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

func TestStaleAddRollbackPreservesNewerSameNameServer(t *testing.T) {
	const name = "replaced-add"
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)

	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var releaseOldOnce sync.Once
	defer func() { releaseOldOnce.Do(func() { close(releaseOld) }) }()
	oldFailure := errors.New("old add failed")
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- addServerWithInitializer(
			context.Background(),
			store,
			name,
			config.MCPConfig{Type: config.MCPHttp, URL: "http://old.invalid"},
			func(
				_ context.Context,
				_ *config.ConfigStore,
				_ string,
				_ config.MCPConfig,
				_ config.VariableResolver,
				admission *serverAdmission,
			) error {
				defer admission.done()
				close(oldStarted)
				<-releaseOld
				return oldFailure
			},
		)
	}()
	waitForRequest(t, oldStarted)
	require.NoError(t, RemoveServer(store, name))

	newServer := mcp.NewServer(&mcp.Implementation{Name: "replacement-server"}, nil)
	mcp.AddTool(newServer, &mcp.Tool{Name: "replacement-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newServer }, nil))
	defer func() {
		require.NoError(t, owner.Close(context.Background()))
		httpServer.Close()
	}()
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60}
	require.NoError(t, AddServer(context.Background(), store, name, replacement))

	releaseOldOnce.Do(func() { close(releaseOld) })
	require.ErrorIs(t, <-oldDone, oldFailure)
	currentConfig, exists := store.MCPConfig(name)
	require.True(t, exists)
	require.Equal(t, replacement, currentConfig)
	currentSession, exists := sessions.Get(name)
	require.True(t, exists)
	state, exists := GetState(name)
	require.True(t, exists)
	require.Equal(t, StateConnected, state.State)
	require.Same(t, currentSession, state.Client)
	require.Equal(t, []string{"replacement-tool"}, GetServerToolNames(name))
}

func TestGetOrRenewClientKeepsLeaseIdentityAcrossConcurrentReplacement(t *testing.T) {
	const name = "concurrent-renewal"
	pingStarted := make(chan struct{}, 2)
	releasePings := make(chan struct{})
	var releasePingsOnce sync.Once
	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	oldServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method != "ping" {
				return next(ctx, method, request)
			}
			pingStarted <- struct{}{}
			<-releasePings
			return nil, errors.New("force renewal")
		}
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	oldServerSession, err := oldServer.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer oldServerSession.Close()
	oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	old := &ClientSession{ClientSession: oldClientSession, cancel: func() {}}

	newServer := mcp.NewServer(&mcp.Implementation{Name: "new-server"}, nil)
	mcp.AddTool(newServer, &mcp.Tool{Name: "new-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newServer }, nil))
	defer httpServer.Close()
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	defer func() { releasePingsOnce.Do(func() { close(releasePings) }) }()
	sessions.Set(name, old)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: old})

	results := make(chan *clientLease, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			client, renewErr := getOrRenewClient(context.Background(), store, name)
			if renewErr != nil {
				errs <- renewErr
				return
			}
			results <- client
		}()
	}
	waitForRequest(t, pingStarted)
	waitForRequest(t, pingStarted)

	leases.mu.Lock()
	originalLease := leases.entries[name]
	refsDuringPing := 0
	if originalLease != nil {
		refsDuringPing = originalLease.refs
	}
	leases.mu.Unlock()
	require.NotNil(t, originalLease)
	require.Equal(t, 2, refsDuringPing)
	releasePingsOnce.Do(func() { close(releasePings) })

	var first *clientLease
	select {
	case first = <-results:
	case err := <-errs:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first renewal did not complete")
	}
	require.Eventually(t, func() bool {
		leases.mu.Lock()
		defer leases.mu.Unlock()
		return leases.entries[name] == originalLease && originalLease.refs == 2
	}, time.Second, time.Millisecond, "the queued renewal must retain the original lease while the first caller reads")
	first.close()

	var second *clientLease
	select {
	case second = <-results:
	case err := <-errs:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued renewal did not reacquire the current session")
	}
	require.Same(t, first.session, second.session)
	leases.mu.Lock()
	registered := leases.entries[name]
	refsAfterReacquire := originalLease.refs
	leases.mu.Unlock()
	require.Same(t, originalLease, registered, "getOrRenewClient must not cross an ABA-replaced lease")
	require.Equal(t, 1, refsAfterReacquire)
	second.close()

	require.Eventually(t, func() bool {
		leases.mu.Lock()
		defer leases.mu.Unlock()
		return leases.entries[name] == nil && originalLease.refs == 0
	}, time.Second, time.Millisecond)
}

func TestRenewedSessionNotificationSurvivesConfigMutations(t *testing.T) {
	const name = "renewed-notification"
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-notification-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()

	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	oldServerSession, err := oldServer.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer oldServerSession.Close()
	oldCtx, oldCancel := context.WithCancel(context.Background())
	oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(oldCtx, clientTransport, nil)
	require.NoError(t, err)
	old := &ClientSession{ClientSession: oldClientSession, cancel: oldCancel}
	require.NoError(t, old.Close())
	sessions.Set(name, old)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: old})

	eventsCtx, eventsCancel := context.WithCancel(context.Background())
	defer eventsCancel()
	events := SubscribeEvents(eventsCtx)
	client, err := getOrRenewClient(context.Background(), store, name)
	require.NoError(t, err)
	defer client.close()
	require.NotSame(t, old, client.session)
	store.SetSkipPermissionRequests(true)
	require.True(t, store.AddMCP("unrelated", config.MCPConfig{Type: config.MCPStdio, Command: "unused"}))
	updated, exists := store.UpdateMCP(name, func(mcpConfig *config.MCPConfig) {
		mcpConfig.DisabledTools = []string{"initial"}
		mcpConfig.Timeout++
	})
	require.True(t, exists)
	require.Equal(t, []string{"initial"}, updated.DisabledTools)

	mcp.AddTool(server, &mcp.Tool{Name: "after-renewal"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	for {
		select {
		case event := <-events:
			if event.Payload.Name == name && event.Payload.Type == EventToolsListChanged {
				client.close()
				goto notified
			}
		case <-time.After(5 * time.Second):
			client.close()
			t.Fatal("renewed session did not receive a tool-list notification")
		}
	}

notified:
	require.Eventually(t, func() bool {
		return len(GetServerToolNames(name)) == 1
	}, 5*time.Second, time.Millisecond, "same-server config mutation invalidated the renewed session notification")
	require.Equal(t, []string{"after-renewal"}, GetServerToolNames(name), "refresh must apply the current disabled_tools filter")
}

func TestListChangedDuringRefreshSchedulesDirtyRerun(t *testing.T) {
	const name = "dirty-refresh"
	firstObserved := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	var listCalls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "dirty-refresh-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "first"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, request)
			if method == "tools/list" && listCalls.Add(1) == 1 {
				close(firstObserved)
				<-releaseFirst
			}
			return result, err
		}
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dirty-refresh-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)

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
	defer func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }()

	notifyListChanged(&admission, name, refreshToolsKind)
	waitForRequest(t, firstObserved)
	mcp.AddTool(server, &mcp.Tool{Name: "latest"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	start := time.Now()
	notifyListChanged(&admission, name, refreshToolsKind)
	require.Less(t, time.Since(start), 100*time.Millisecond, "dirty notification must not block behind the in-flight list call")

	key := refreshKey{name: name, kind: refreshToolsKind, epoch: admission.epoch}
	lifecycleMu.Lock()
	_, dirty := owner.refreshPending[key]
	lifecycleMu.Unlock()
	require.True(t, dirty, "a notification during an in-flight refresh must retain one coalesced rerun")
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	require.Eventually(t, func() bool {
		return listCalls.Load() >= 2 && len(GetServerToolNames(name)) == 2
	}, time.Second, time.Millisecond, "the dirty rerun did not publish the latest tool list")
	require.ElementsMatch(t, []string{"first", "latest"}, GetServerToolNames(name))
}

func TestRefreshQueueSaturationCoalescesWithoutDropping(t *testing.T) {
	const blockedName = "refresh-blocked"
	const queued = 256
	mcpConfigs := make(config.MCPs, queued+1)
	mcpConfigs[blockedName] = config.MCPConfig{Type: config.MCPStdio, Command: "unused"}
	for i := range queued {
		mcpConfigs[fmt.Sprintf("refresh-%03d", i)] = config.MCPConfig{Type: config.MCPStdio, Command: "unused"}
	}
	store := config.NewTestStore(&config.Config{MCP: mcpConfigs})
	owner, err := Acquire()
	require.NoError(t, err)

	blocker := serverLeaseFor(blockedName)
	blocker.Lock()
	blockerLocked := true
	defer func() {
		if blockerLocked {
			blocker.Unlock()
		}
		require.NoError(t, owner.Close(context.Background()))
	}()

	admissionFor := func(name string) serverAdmission {
		return serverAdmission{
			owner:      owner,
			generation: owner.generation,
			epoch:      owner.serverEpochs[name],
			cfg:        store,
			name:       name,
			ctx:        owner.lifecycleCtx,
		}
	}
	blockedAdmission := admissionFor(blockedName)
	owner.enqueueRefresh(refreshRequest{
		kind:      refreshToolsKind,
		name:      blockedName,
		cfg:       store,
		admission: blockedAdmission,
	})
	require.Eventually(t, func() bool {
		leases.mu.Lock()
		defer leases.mu.Unlock()
		return leases.entries[blockedName] == blocker && blocker.refs == 2
	}, time.Second, time.Millisecond, "refresh worker did not block behind the held server lease")

	start := time.Now()
	for i := range queued {
		name := fmt.Sprintf("refresh-%03d", i)
		admission := admissionFor(name)
		owner.enqueueRefresh(refreshRequest{
			kind:      refreshToolsKind,
			name:      name,
			cfg:       store,
			admission: admission,
		})
	}
	require.Less(t, time.Since(start), 500*time.Millisecond, "notification enqueue must remain non-blocking under saturation")

	lifecycleMu.Lock()
	pending := len(owner.refreshPending)
	lifecycleMu.Unlock()
	require.Equal(t, queued, pending, "every distinct refresh must remain scheduled after the wake channel saturates")

	duplicateName := "refresh-000"
	duplicateAdmission := admissionFor(duplicateName)
	owner.enqueueRefresh(refreshRequest{
		kind:      refreshToolsKind,
		name:      duplicateName,
		cfg:       store,
		admission: duplicateAdmission,
	})
	lifecycleMu.Lock()
	pending = len(owner.refreshPending)
	lifecycleMu.Unlock()
	require.Equal(t, queued, pending, "duplicate notifications must coalesce")

	blocker.Unlock()
	blockerLocked = false
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return len(owner.refreshPending) == 0
	}, 5*time.Second, time.Millisecond, "refresh worker did not eventually drain saturated work")
}

func TestLeaseRegistryReclaimsChurnAndProtectsWaiterFromABA(t *testing.T) {
	const name = "lease-churn"
	for range 500 {
		lease := serverLeaseFor(name)
		lease.Lock()
		lease.Unlock()
	}
	leases.mu.Lock()
	require.Empty(t, leases.entries)
	leases.mu.Unlock()

	old := serverLeaseFor(name)
	old.Lock()
	ready := make(chan struct{})
	acquired := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		waiter := serverLeaseFor(name)
		close(ready)
		waiter.Lock()
		close(acquired)
		waiter.Unlock()
	}()
	<-ready
	require.Eventually(t, func() bool {
		leases.mu.Lock()
		defer leases.mu.Unlock()
		return leases.entries[name] == old && old.refs == 2
	}, time.Second, time.Millisecond)
	old.Unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("lease waiter did not acquire after the holder released")
	}
	wg.Wait()
	leases.mu.Lock()
	require.Empty(t, leases.entries)
	leases.mu.Unlock()
}

func signalStarted(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func TestInitializePublishesHTTPSessionBeyondAdmission(t *testing.T) {
	const name = "initialize-live-session"
	var initializeCalls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "live-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				initializeCalls.Add(1)
			}
			return next(ctx, method, request)
		}
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))

	store := persistedMCPStore(t, name, httpServer.URL, false)
	_, ok := store.UpdateMCP(name, func(mcpConfig *config.MCPConfig) {
		mcpConfig.Timeout = 1
	})
	require.True(t, ok)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, owner.Close(context.Background()))
		httpServer.Close()
	}()

	owner.Initialize(context.Background(), nil, store, false)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.Equal(t, int32(1), initializeCalls.Load())
	time.Sleep(1200 * time.Millisecond)

	callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, state.Client.Ping(callCtx, nil))
	_, err = state.Client.CallTool(callCtx, &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)
	require.Equal(t, int32(1), initializeCalls.Load(), "admission completion must not retire the published session")
}

func TestInitializeSinglePinsOwnerAcrossRollover(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	const name = "initialize-single-rollover"
	store := persistedMCPStore(t, name, server.URL, false)
	oldOwner, err := Acquire()
	require.NoError(t, err)
	oldDone := make(chan error, 1)
	go func() { oldDone <- InitializeSingle(context.Background(), name, store) }()
	waitForRequest(t, started)

	require.NoError(t, oldOwner.Close(context.Background()))
	newOwner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, newOwner.Close(context.Background())) }()
	require.Error(t, <-oldDone)
	_, ok := sessions.Get(name)
	require.False(t, ok, "a single-server initializer admitted by the old owner must not use the new owner")
}

func TestRefreshAdmissionDoesNotOwnInitializerCancellation(t *testing.T) {
	const name = "refresh-admission-ownership"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	initializer, err := owner.admitServer(context.Background(), store, name, true)
	require.NoError(t, err)
	refresh, err := owner.admitServer(owner.lifecycleCtx, store, name, false)
	require.NoError(t, err)
	refresh.done()

	lifecycleMu.Lock()
	_, retained := owner.serverCancels[name][initializer.serverCancelToken]
	lifecycleMu.Unlock()
	require.True(t, retained, "refresh cleanup must not remove the initializer cancellation")

	owner.invalidateServer(name)
	select {
	case <-initializer.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("server invalidation did not cancel the initializer")
	}
	initializer.done()
}

func TestInitializeBarrierIsGenerationScopedAcrossRollover(t *testing.T) {
	oldOwner, err := Acquire()
	require.NoError(t, err)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- WaitForInit(waitCtx) }()

	require.NoError(t, oldOwner.Close(context.Background()))
	select {
	case err := <-waitDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("waiter captured by the old owner remained blocked after close")
	}

	newOwner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, newOwner.Close(context.Background())) }()

	oldOwner.Initialize(context.Background(), nil, config.NewTestStore(&config.Config{}), false)
	newWaitCtx, newWaitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer newWaitCancel()
	require.ErrorIs(t, WaitForInit(newWaitCtx), context.DeadlineExceeded,
		"a stale old-owner invocation must not close the new owner's barrier")
}

func TestHeaderRoundTripperUsesOwnedTransportPool(t *testing.T) {
	first, err := cloneHTTPTransport()
	require.NoError(t, err)
	second, err := cloneHTTPTransport()
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.NotSame(t, http.DefaultTransport, first)
	require.NotSame(t, http.DefaultTransport, second)

	firstRT := &headerRoundTripper{transport: first}
	secondRT := &headerRoundTripper{transport: second}
	require.NotSame(t, firstRT.transport, secondRT.transport)
	firstRT.CloseIdleConnections()
	secondRT.CloseIdleConnections()
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCloneHTTPTransportFallsBackFromCustomDefault(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = testRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("custom default transport must not be used")
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	first, err := cloneHTTPTransport()
	require.NoError(t, err)
	second, err := cloneHTTPTransport()
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.True(t, first.ForceAttemptHTTP2)

	transport, err := createTransport(context.Background(), config.MCPConfig{
		Type: config.MCPHttp,
		URL:  "https://mcp.example.com",
	}, config.IdentityResolver())
	require.NoError(t, err)
	streamable, ok := transport.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	roundTripper, ok := streamable.HTTPClient.Transport.(*headerRoundTripper)
	require.True(t, ok)
	require.NotSame(t, first, roundTripper.transport)
	require.NotNil(t, roundTripper.transport)
}

func TestMaybeTimeoutErrPreservesNonTimeoutCancellation(t *testing.T) {
	timeout := time.Second
	timeoutCause := errors.New("owned timeout")
	require.ErrorIs(t, maybeTimeoutErr(context.Canceled, timeout, context.Canceled, timeoutCause), context.Canceled)
	require.ErrorIs(t, maybeTimeoutErr(context.DeadlineExceeded, timeout, context.DeadlineExceeded, timeoutCause), context.DeadlineExceeded)
	require.EqualError(t, maybeTimeoutErr(context.DeadlineExceeded, timeout, timeoutCause, timeoutCause), "timed out after 1s")
}

func TestSessionCancellationClassification(t *testing.T) {
	newBlockingServer := func() (*httptest.Server, <-chan struct{}) {
		started := make(chan struct{}, 1)
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			signalStarted(started)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})), started
	}

	t.Run("caller cancellation remains canceled", func(t *testing.T) {
		server, started := newBlockingServer()
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := createSession(ctx, "caller-cancel", config.MCPConfig{
				Type: config.MCPHttp, URL: server.URL, Timeout: 60,
			}, config.IdentityResolver())
			done <- err
		}()
		waitForRequest(t, started)
		cancel()
		err := <-done
		require.ErrorIs(t, err, context.Canceled)
		require.NotContains(t, err.Error(), "timed out")
	})

	t.Run("caller deadline remains caller deadline", func(t *testing.T) {
		server, started := newBlockingServer()
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := createSession(ctx, "caller-deadline", config.MCPConfig{
				Type: config.MCPHttp, URL: server.URL, Timeout: 60,
			}, config.IdentityResolver())
			done <- err
		}()
		waitForRequest(t, started)
		err := <-done
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotContains(t, err.Error(), "timed out after 60s")
	})

	t.Run("admission cancellation remains canceled", func(t *testing.T) {
		server, started := newBlockingServer()
		defer server.Close()
		const name = "admission-cancel"
		store := config.NewTestStore(&config.Config{MCP: config.MCPs{
			name: {Type: config.MCPHttp, URL: server.URL, Timeout: 60},
		}})
		owner, err := Acquire()
		require.NoError(t, err)
		admission, err := owner.admitServer(context.Background(), store, name, true)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := createSessionWithAdmission(admission.ctx, name, store.Config().MCP[name], config.IdentityResolver(), &admission)
			done <- err
		}()
		waitForRequest(t, started)
		owner.invalidateServer(name)
		err = <-done
		require.ErrorIs(t, err, context.Canceled)
		require.NotContains(t, err.Error(), "timed out")
		admission.done()
		require.NoError(t, owner.Close(context.Background()))
	})

	t.Run("owned timeout reports timed out", func(t *testing.T) {
		server, started := newBlockingServer()
		defer server.Close()
		done := make(chan error, 1)
		go func() {
			_, err := createSession(context.Background(), "owned-timeout", config.MCPConfig{
				Type: config.MCPHttp, URL: server.URL, Timeout: 1,
			}, config.IdentityResolver())
			done <- err
		}()
		waitForRequest(t, started)
		err := <-done
		require.EqualError(t, err, "timed out after 1s")
	})
}

func TestPingTimeoutClassification(t *testing.T) {
	newSession := func(t *testing.T) (*ClientSession, func()) {
		t.Helper()
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		server := mcp.NewServer(&mcp.Implementation{Name: "blocking-ping-server"}, nil)
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
				if method == "ping" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return next(ctx, method, request)
			}
		})
		serverSession, err := server.Connect(context.Background(), serverTransport, nil)
		require.NoError(t, err)
		clientCtx, clientCancel := context.WithCancel(context.Background())
		clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "blocking-ping-client"}, nil).
			Connect(clientCtx, clientTransport, nil)
		require.NoError(t, err)
		session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}
		return session, func() {
			require.NoError(t, session.Close())
			require.NoError(t, serverSession.Close())
		}
	}

	t.Run("caller deadline", func(t *testing.T) {
		session, cleanup := newSession(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := pingWithTimeout(ctx, session, time.Minute)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotContains(t, err.Error(), "timed out after 1m0s")
	})

	t.Run("owned timeout", func(t *testing.T) {
		session, cleanup := newSession(t)
		defer cleanup()
		err := pingWithTimeout(context.Background(), session, 50*time.Millisecond)
		require.EqualError(t, err, "timed out after 50ms")
	})
}
