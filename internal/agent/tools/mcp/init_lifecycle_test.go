package mcp

// Owner lifecycle tests around initialization: registry reset on Close, the next lifecycle after it, and the init barrier's behaviour across owners. Split out of init_test.go when the 1000-line file limit landed.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestOwnerCloseResetsRegistryAndAllowsNextLifecycle(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)

	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy,
		"a live application owner must not be replaced even while its registry is empty")

	states.Set("owner-probe", ClientInfo{Name: "owner-probe", State: StateConnected})

	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy,
		"a second application owner must not share the live MCP registry")

	require.NoError(t, owner.Close(context.Background()))
	require.Empty(t, GetStates())
	require.Empty(t, func() map[string][]*Tool {
		got := map[string][]*Tool{}
		for name, tools := range Tools() {
			got[name] = tools
		}
		return got
	}())

	next, err := Acquire()
	require.NoError(t, err, "a completed owner must not poison a later lifecycle")
	require.NoError(t, next.Close(context.Background()))
}

func TestOwnerCloseCancelsBlockedStartupBeforeCleanup(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
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
	store.Config().MCP = config.MCPs{
		"blocked-startup": {
			Type:    config.MCPHttp,
			URL:     server.URL,
			Timeout: 60,
		},
	}

	owner, err := Acquire()
	require.NoError(t, err)
	initFinished := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(initFinished)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP startup did not reach the blocked transport")
	}

	require.NoError(t, owner.Close(context.Background()))
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("owner close did not cancel the blocked HTTP request")
	}
	select {
	case <-initFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP initialization must finish before Owner.Close returns")
	}
	require.Empty(t, GetStates())
	require.Empty(t, func() map[string][]*Tool {
		got := map[string][]*Tool{}
		for name, tools := range Tools() {
			got[name] = tools
		}
		return got
	}())
}

func TestOwnerCloseVsRenewalDoesNotPublishLateSession(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	const name = "late-renewal"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	createdCtx, createdCancel := context.WithCancel(context.Background())
	defer createdCancel()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	closeCalls := 0
	created := &ClientSession{ClientSession: clientSession, cancel: func() {
		closeCalls++
		createdCancel()
	}}

	require.True(t, owner.beginInit())
	closeStarted := make(chan error, 1)
	go func() {
		closeStarted <- owner.Close(context.Background())
	}()

	deadline := time.Now().Add(time.Second)
	for owner.acceptsSession() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.False(t, owner.acceptsSession())
	require.ErrorIs(t, owner.commitRenewal(&admission, name, created, Counts{}), context.Canceled)
	require.ErrorIs(t, createdCtx.Err(), context.Canceled)
	require.Equal(t, 1, closeCalls, "a rejected renewal session must be closed exactly once")
	require.Empty(t, func() map[string]*ClientSession {
		got := map[string]*ClientSession{}
		for name, session := range sessions.Seq2() {
			got[name] = session
		}
		return got
	}())

	owner.endInit()
	require.NoError(t, <-closeStarted)
	require.Eventually(t, func() bool {
		_, ok := GetState(name)
		return !ok
	}, time.Second, time.Millisecond)
}

func TestOwnerCommitRenewalPublishesSuccessfulSession(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	const name = "successful-renewal"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	_, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}

	states.Set(name, ClientInfo{Name: name, State: StateError})
	eventsCtx, eventsCancel := context.WithCancel(context.Background())
	defer eventsCancel()
	events := SubscribeEvents(eventsCtx)

	require.True(t, owner.beginInit())
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- owner.commitRenewal(&admission, name, session, Counts{Tools: 1})
	}()

	select {
	case err := <-commitDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("successful renewal deadlocked while publishing its state")
	}

	got, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, session, got)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.Same(t, session, state.Client)

	select {
	case event := <-events:
		require.Equal(t, pubsub.UpdatedEvent, event.Type)
		require.Equal(t, name, event.Payload.Name)
		require.Equal(t, StateConnected, event.Payload.State)
	case <-time.After(time.Second):
		t.Fatal("successful renewal did not publish a state event")
	}

	owner.endInit()
	require.NoError(t, owner.Close(context.Background()))
}

func TestRenewalRejectsCrossStoreDiskReplacement(t *testing.T) {
	testCrossStoreRenewalAdmission(t, false)
}

func TestRenewalRejectsCrossStoreDiskRemoval(t *testing.T) {
	testCrossStoreRenewalAdmission(t, true)
}

func TestFailClosedMCPHonorsCallerCancellation(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	const name = "fail-closed-canceled"
	lease := serverLeaseFor(name)
	lease.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = failClosedMCP(ctx, name)
	lease.Unlock()
	require.ErrorIs(t, err, context.Canceled)
}

func testCrossStoreRenewalAdmission(t *testing.T, remove bool) {
	t.Helper()
	const name = "cross-store-renewal-admission"
	store := persistedMCPStore(t, name, "http://old-renewal.example", false)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)
	if remove {
		require.NoError(t, contender.PersistRemoveMCPConfigExact(config.ScopeGlobal, name))
	} else {
		require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"url": "http://new-renewal.example",
		}))
	}
	session := newInMemoryRenewalSession(t)
	require.True(t, owner.beginInit())
	err = owner.commitRenewal(&admission, name, session, Counts{})
	owner.endInit()
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	_, published := sessions.Get(name)
	require.False(t, published, "a renewal from a stale cross-store snapshot must not publish")
}

func newInMemoryRenewalSession(t *testing.T) *ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return &ClientSession{ClientSession: clientSession}
}

func TestSessionContextPromotionLinearizesCancellation(t *testing.T) {
	t.Run("caller cancellation is observed synchronously", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		defer ownerCancel()
		callerCtx, callerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := newSessionContextWithCaller(ownerCtx, candidateCtx, callerCtx, candidateCancel, nil)
		callerCancel()

		require.False(t, handoff.promote())
		require.ErrorIs(t, handoff.Err(), context.Canceled)
		select {
		case <-handoff.Done():
		case <-time.After(time.Second):
			t.Fatal("caller cancellation did not close the handoff context")
		}
		select {
		case <-handoff.workerDone:
		case <-time.After(time.Second):
			t.Fatal("rejected promotion left its handoff goroutine running")
		}
	})

	t.Run("cancellation wins", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		defer ownerCancel()
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		candidateCancel()
		handoff := &sessionContext{
			owner: ownerCtx, candidate: candidateCtx, done: make(chan struct{}),
		}

		require.True(t, handoff.finish(candidateCtx, true))
		require.False(t, handoff.promote())
		require.ErrorIs(t, handoff.Err(), context.Canceled)
		select {
		case <-handoff.Done():
		default:
			t.Fatal("cancellation winner did not close the handoff context")
		}
	})

	t.Run("promotion wins", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := &sessionContext{
			owner: ownerCtx, candidate: candidateCtx, done: make(chan struct{}),
		}

		require.True(t, handoff.promote())
		candidateCancel()
		require.False(t, handoff.finish(candidateCtx, true))
		select {
		case <-handoff.Done():
			t.Fatal("candidate cancellation closed a promoted handoff context")
		default:
		}

		ownerCancel()
		require.True(t, handoff.finish(ownerCtx, false))
		select {
		case <-handoff.Done():
		default:
			t.Fatal("owner cancellation did not close the promoted handoff context")
		}
	})
}

func TestClientSessionCloseAbortsPromotedCandidate(t *testing.T) {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	candidateCtx, candidateCancel := context.WithCancel(context.Background())
	defer candidateCancel()
	handoff := newSessionContext(ownerCtx, candidateCtx)
	require.True(t, handoff.promote())
	session := &ClientSession{
		cancel:   func() {},
		terminal: handoff.abort,
	}

	require.NoError(t, session.Close())
	select {
	case <-handoff.Done():
	case <-time.After(time.Second):
		t.Fatal("closing a promoted candidate did not abort its context")
	}
	select {
	case <-handoff.workerDone:
	case <-time.After(time.Second):
		t.Fatal("closing a promoted candidate left its handoff goroutine running")
	}
	require.ErrorIs(t, handoff.Err(), context.Canceled)
}

func TestPublishedSessionIgnoresCandidateCancellationUntilClose(t *testing.T) {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	candidateCtx, candidateCancel := context.WithCancel(context.Background())
	handoff := newSessionContext(ownerCtx, candidateCtx)
	require.True(t, handoff.promote())
	session := &ClientSession{
		cancel:   func() {},
		terminal: handoff.abort,
	}

	candidateCancel()
	select {
	case <-handoff.Done():
		t.Fatal("candidate cancellation closed a published session context")
	default:
	}

	require.NoError(t, session.Close())
	select {
	case <-handoff.workerDone:
	case <-time.After(time.Second):
		t.Fatal("closing the published session did not stop its handoff goroutine")
	}
}

func TestRepeatedPromotedCandidateCloseStopsHandoff(t *testing.T) {
	for range 32 {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := newSessionContext(ownerCtx, candidateCtx)
		require.True(t, handoff.promote())
		session := &ClientSession{cancel: func() {}, terminal: handoff.abort}
		require.NoError(t, session.Close())
		select {
		case <-handoff.workerDone:
		case <-time.After(time.Second):
			t.Fatal("repeated candidate close left a handoff goroutine running")
		}
		ownerCancel()
		candidateCancel()
	}
}

func TestCommitRenewalPublishesOnlyAfterPromotion(t *testing.T) {
	const (
		rejectedName = "promotion-rejected"
		acceptedName = "promotion-accepted"
	)
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		rejectedName: {Type: config.MCPStdio, Command: "unused"},
		acceptedName: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })

	rejectedAdmission, err := owner.snapshotServerAdmission(context.Background(), store, rejectedName)
	require.NoError(t, err)
	rejectedCandidate, reject := context.WithCancel(context.Background())
	reject()
	rejectedHandoff := &sessionContext{
		owner: owner.lifecycleCtx, candidate: rejectedCandidate, done: make(chan struct{}),
	}
	require.True(t, rejectedHandoff.finish(rejectedCandidate, true))
	rejectedCloseCalls := 0
	rejected := &ClientSession{
		cancel:  func() { rejectedCloseCalls++ },
		promote: rejectedHandoff.promote,
	}
	require.ErrorIs(t, owner.commitRenewal(&rejectedAdmission, rejectedName, rejected, Counts{}), ErrOwnerBusy)
	require.Equal(t, 1, rejectedCloseCalls)
	_, ok := sessions.Get(rejectedName)
	require.False(t, ok)
	_, ok = GetState(rejectedName)
	require.False(t, ok)

	acceptedAdmission, err := owner.snapshotServerAdmission(context.Background(), store, acceptedName)
	require.NoError(t, err)
	acceptedCandidate, cancelCandidate := context.WithCancel(context.Background())
	acceptedHandoff := newSessionContext(owner.lifecycleCtx, acceptedCandidate)
	acceptedCloseCalls := 0
	accepted := &ClientSession{
		cancel:  func() { acceptedCloseCalls++ },
		promote: acceptedHandoff.promote,
	}
	require.NoError(t, owner.commitRenewal(&acceptedAdmission, acceptedName, accepted, Counts{}))
	cancelCandidate()
	require.False(t, acceptedHandoff.finish(acceptedCandidate, true))
	select {
	case <-acceptedHandoff.Done():
		t.Fatal("candidate cancellation closed the committed session context")
	default:
	}
	current, ok := sessions.Get(acceptedName)
	require.True(t, ok)
	require.Same(t, accepted, current)
	require.Equal(t, 0, acceptedCloseCalls)

	require.NoError(t, owner.Close(context.Background()))
	require.Equal(t, 1, acceptedCloseCalls)
}

func TestClientLeaseProtectsOperationFromRenewalAndClose(t *testing.T) {
	const name = "leased-operation"
	started := make(chan struct{})
	release := make(chan struct{})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "lease-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "blocked"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		close(started)
		<-release
		return &mcp.CallToolResult{}, nil, nil
	})
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "lease-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}

	owner, err := Acquire()
	require.NoError(t, err)
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session})
	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().MCP[name] = config.MCPConfig{Type: config.MCPStdio, Command: "echo"}

	lease, err := getOrRenewClient(context.Background(), store, name)
	require.NoError(t, err)
	callDone := make(chan error, 1)
	go func() {
		_, callErr := lease.session.CallTool(lease.ctx, &mcp.CallToolParams{Name: "blocked"})
		callDone <- callErr
	}()
	<-started

	serverLease := serverLeaseFor(name)
	renewalDone := make(chan struct{})
	go func() {
		serverLease.Lock()
		serverLease.Unlock()
		close(renewalDone)
	}()
	select {
	case <-renewalDone:
	case <-time.After(time.Second):
		t.Fatal("renewal remained blocked by an MCP operation")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	require.ErrorIs(t, owner.Close(closeCtx), context.DeadlineExceeded)
	closeCancel()

	close(release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("MCP operation did not finish after the server was released")
	}
	lease.close()
	require.NoError(t, owner.Close(context.Background()))
}

func TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose(t *testing.T) {
	serverCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	transport, err := cloneHTTPTransport()
	require.NoError(t, err)
	rt := &headerRoundTripper{ctx: ownerCtx, transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Body)

	ownerCancel()
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not reach the open response body")
	}
	require.NoError(t, resp.Body.Close())
}

type closeAfterContextBody struct {
	ctx     context.Context
	started chan struct{}
}

func (*closeAfterContextBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *closeAfterContextBody) Close() error {
	close(b.started)
	<-b.ctx.Done()
	return nil
}

func TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose(t *testing.T) {
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	underlying := &closeAfterContextBody{ctx: requestCtx, started: make(chan struct{})}
	stopCalls := 0
	body := &ownerResponseBody{
		ReadCloser: underlying,
		stop: func() bool {
			stopCalls++
			return true
		},
		cancel: requestCancel,
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- body.Close() }()
	<-underlying.started
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		requestCancel()
		<-closeDone
		t.Fatal("response body Close did not cancel the request before closing the underlying body")
	}
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Equal(t, 1, stopCalls)
}

func TestOwnerCloseDeadlineRetainsFenceUntilStuckSessionCloses(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "stuck-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "stuck"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		close(started)
		<-release
		return &mcp.CallToolResult{}, nil, nil
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "stuck-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	_, clientCancel := context.WithCancel(context.Background())
	sess := &ClientSession{ClientSession: clientSession, cancel: clientCancel}
	defer clientCancel()

	owner, err := Acquire()
	require.NoError(t, err)
	sessions.Set("stuck", sess)
	states.Set("stuck", ClientInfo{Name: "stuck", State: StateConnected, Client: sess})

	callDone := make(chan error, 1)
	go func() {
		_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "stuck"})
		callDone <- err
	}()
	<-started

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer closeCancel()
	require.ErrorIs(t, owner.Close(closeCtx), context.DeadlineExceeded)
	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy)

	close(release)
	require.Eventually(t, func() bool {
		select {
		case <-callDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, owner.Close(context.Background()))
	next, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, next.Close(context.Background()))
}
