package mcp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func persistedMCPStore(t *testing.T, name, url string, disabled bool) *config.ConfigStore {
	t.Helper()
	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
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
	rt := &headerRoundTripper{ctx: ownerCtx}
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

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
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

func TestServerLeaseDowngradeRetainsIdentityAndRefs(t *testing.T) {
	const name = "lease-downgrade"
	lease := serverLeaseFor(name)
	lease.Lock()
	lease.downgrade()

	leases.mu.Lock()
	registered := leases.entries[name]
	refs := lease.refs
	leases.mu.Unlock()
	require.Same(t, lease, registered, "write-to-read transition must not expose an ABA replacement window")
	require.Equal(t, 1, refs)

	lookup := serverLeaseFor(name)
	require.Same(t, lease, lookup)
	lookup.RLock()
	lookup.RUnlock()
	lease.RUnlock()

	leases.mu.Lock()
	remaining := leases.entries[name]
	finalRefs := lease.refs
	leases.mu.Unlock()
	require.Nil(t, remaining)
	require.Zero(t, finalRefs)
}

func TestRenewedSessionNotificationSchedulesRefresh(t *testing.T) {
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
		return len(GetServerToolNames(name)) == 2
	}, 5*time.Second, time.Millisecond, "renewed session notification did not refresh advertised tools")
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
			owner:            owner,
			generation:       owner.generation,
			epoch:            owner.serverEpochs[name],
			configGeneration: store.Generation(),
			cfg:              store,
			name:             name,
			ctx:              owner.lifecycleCtx,
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
