package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestOperationLeaseRetiresBlockedCallWithoutWriterConvoy(t *testing.T) {
	const name = "blocked-operation-lease"
	started := make(chan struct{})
	release := make(chan struct{})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "lease-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "blocked"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		close(started)
		select {
		case <-release:
			return &mcp.CallToolResult{}, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "lease-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	defer clientSession.Close()

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	session := &ClientSession{ClientSession: clientSession}
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session})

	lease, err := getOrRenewClient(context.Background(), store, name)
	require.NoError(t, err)
	callDone := make(chan error, 1)
	go func() {
		_, callErr := lease.session.CallTool(lease.ctx, &mcp.CallToolParams{Name: "blocked"})
		callDone <- callErr
	}()
	<-started

	writerDone := make(chan struct{})
	go func() {
		serverLease := serverLeaseFor(name)
		serverLease.Lock()
		sessions.Del(name)
		retireMCPClient(name, session)
		serverLease.Unlock()
		close(writerDone)
	}()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer remained behind an in-flight network operation")
	}

	close(release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("blocked operation did not finish after retirement")
	}
	lease.close()
	require.Eventually(t, func() bool {
		session.operationMu.Lock()
		defer session.operationMu.Unlock()
		return session.retired && session.operationRefs == 0
	}, time.Second, time.Millisecond)
}

func TestCancelledFollowerLeavesOwnerInitReference(t *testing.T) {
	const name = "cancelled-follower"
	owner, err := Acquire()
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	session := &ClientSession{}
	sessions.Set(name, session)
	lease := serverLeaseFor(name)
	lease.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = getOrRenewClient(ctx, store, name)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	lease.Unlock()
	require.NoError(t, owner.Close(context.Background()))
}

func TestOperationLeaseDefersCloseUntilFinalReference(t *testing.T) {
	var closeCalls atomic.Int32
	session := &ClientSession{cancel: func() { closeCalls.Add(1) }}
	operationCtx, release, ok := session.acquireOperation(context.Background())
	require.True(t, ok)
	require.NoError(t, operationCtx.Err())
	require.False(t, session.retire())
	require.Zero(t, closeCalls.Load())

	var released sync.WaitGroup
	released.Add(1)
	go func() {
		defer released.Done()
		release()
	}()
	released.Wait()
	require.Eventually(t, func() bool { return closeCalls.Load() == 1 }, time.Second, time.Millisecond)
	require.True(t, session.retire())
}

func TestRenewalRetainsLeaseIdentityUntilAfterPublish(t *testing.T) {
	const name = "renewal-identity-barrier"
	beginReached := make(chan struct{})
	releaseBegin := make(chan struct{})
	publishReached := make(chan struct{})
	releasePublish := make(chan struct{})
	afterUnlockReached := make(chan *serverLease, 1)
	releaseAfterUnlock := make(chan struct{})
	endSnapshot := make(chan struct {
		lease *serverLease
		refs  int
	}, 1)
	oldTransport, oldClientTransport := mcp.NewInMemoryTransports()
	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	oldServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "ping" {
				return nil, context.Canceled
			}
			return next(ctx, method, request)
		}
	})
	oldServerSession, err := oldServer.Connect(context.Background(), oldTransport, nil)
	require.NoError(t, err)
	defer oldServerSession.Close()
	oldClient, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(context.Background(), oldClientTransport, nil)
	require.NoError(t, err)

	newServer := mcp.NewServer(&mcp.Implementation{Name: "new-server"}, nil)
	httpServer := newTestStreamableServer(t, newServer)
	defer httpServer.Close()
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	old := &ClientSession{ClientSession: oldClient}
	sessions.Set(name, old)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: old})

	lease := serverLeaseFor(name)
	lease.Lock()
	lease.renewalBeginHook = func() {
		close(beginReached)
		<-releaseBegin
	}
	lease.renewalPublishHook = func() {
		close(publishReached)
		<-releasePublish
	}
	lease.renewalAfterPublishUnlockHook = func() {
		writerLease := serverLeaseFor(name)
		writerLease.Lock()
		writerLease.Unlock()
		afterUnlockReached <- writerLease
		<-releaseAfterUnlock
	}
	lease.renewalEndHook = func() {
		leases.mu.Lock()
		registered := leases.entries[name]
		refs := 0
		if registered != nil {
			refs = registered.refs
		}
		leases.mu.Unlock()
		endSnapshot <- struct {
			lease *serverLease
			refs  int
		}{lease: registered, refs: refs}
	}
	require.True(t, lease.registry.retain(lease))
	lease.Unlock()

	result := make(chan *clientLease, 1)
	errs := make(chan error, 1)
	go func() {
		client, renewErr := getOrRenewClient(context.Background(), store, name)
		if renewErr != nil {
			errs <- renewErr
			return
		}
		result <- client
	}()
	<-beginReached
	// The renewal now owns its identity and renewal refs. Release the setup
	// ref before any publish/unlock window is observed.
	lease.registry.release(lease)
	leases.mu.Lock()
	registered := leases.entries[name]
	refs := 0
	if registered != nil {
		refs = registered.refs
	}
	leases.mu.Unlock()
	require.Same(t, lease, registered)
	require.Equal(t, 3, refs)
	close(releaseBegin)
	<-publishReached
	leases.mu.Lock()
	registered = leases.entries[name]
	refs = 0
	if registered != nil {
		refs = registered.refs
	}
	leases.mu.Unlock()
	require.Same(t, lease, registered)
	// The setup ref is gone: only the operation, renewal, and candidate lock
	// transition refs are present before the writer is admitted.
	require.Equal(t, 3, refs)

	close(releasePublish)
	writerLease := <-afterUnlockReached
	leases.mu.Lock()
	registered = leases.entries[name]
	refs = 0
	if registered != nil {
		refs = registered.refs
	}
	leases.mu.Unlock()
	require.Same(t, lease, registered)
	require.Same(t, lease, writerLease)
	// The setup ref is gone and the lookup happened after publish Unlock:
	// operation and renewal refs are the only remaining production refs.
	require.Equal(t, 2, refs)
	close(releaseAfterUnlock)
	snapshot := <-endSnapshot
	require.Same(t, lease, snapshot.lease)
	require.Equal(t, 1, snapshot.refs, "the operation identity ref must survive endRenewal")
	select {
	case err := <-errs:
		t.Fatalf("renewal failed: %v", err)
	case renewed := <-result:
		renewed.close()
	}
	lease.mu.Lock()
	lease.renewalBeginHook = nil
	lease.renewalPublishHook = nil
	lease.renewalEndHook = nil
	lease.mu.Unlock()
	require.Eventually(t, func() bool {
		leases.mu.Lock()
		defer leases.mu.Unlock()
		return leases.entries[name] == nil && lease.refs == 0
	}, time.Second, time.Millisecond)
}

func newTestStreamableServer(t *testing.T, server *mcp.Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
}
