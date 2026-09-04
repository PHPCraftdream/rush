package mcp

import (
	"bytes"
	"context"
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
