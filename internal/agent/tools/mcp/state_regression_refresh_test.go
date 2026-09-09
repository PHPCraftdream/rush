package mcp

// Regression tests for the refresh queue and the notification paths downstream of it: coalescing under saturation, list-changed handling, and the state a refresh publishes. Split out of state_regression_test.go when the 1000-line file limit landed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

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

const stdioMCPHelperEnv = "RUSH_MCP_STDIO_HELPER"

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv(stdioMCPHelperEnv) != "1" {
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-test-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "stdio-ok"}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
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
	cleanupTestMCPServer(t, server, httpServer)

	store := persistedMCPStore(t, name, httpServer.URL, false)
	_, ok := store.UpdateMCP(name, func(mcpConfig *config.MCPConfig) {
		mcpConfig.Timeout = 1
	})
	require.True(t, ok)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, owner.Close(context.Background()))
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

func TestInitializePublishesStdioSessionBeyondAdmission(t *testing.T) {
	const name = "initialize-live-stdio-session"
	store := isolatedMCPStore(t)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name, config.MCPConfig{
		Type:    config.MCPStdio,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHelper"},
		Env:     map[string]string{stdioMCPHelperEnv: "1"},
		Timeout: 1,
	}))

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Close(closeCtx))
	}()

	owner.Initialize(context.Background(), nil, store, false)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	time.Sleep(1200 * time.Millisecond)

	callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, state.Client.Ping(callCtx, nil))
	result, err := state.Client.CallTool(callCtx, &mcp.CallToolParams{Name: "echo"})
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	require.Equal(t, "stdio-ok", result.Content[0].(*mcp.TextContent).Text)
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

func TestInitializeSingleReplacesSameNameOldSession(t *testing.T) {
	httpServer := transactionalTestServer(t, "initialize-single-new-tool", "new")
	const name = "initialize-single-same-name"
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		closeCommitOutcomeTestOwner(t, owner)
	}()

	oldSession := &ClientSession{}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})

	require.NoError(t, InitializeSingle(context.Background(), name, store))
	current, ok := sessions.Get(name)
	require.True(t, ok)
	require.NotSame(t, oldSession, current)
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestInitializeSingleDefersCandidateNotificationWithOldSameNameSession(t *testing.T) {
	const name = "initialize-single-notification"
	oldServer := mcp.NewServer(&mcp.Implementation{Name: "initialize-single-old"}, nil)
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
	oldClientSession, err := mcp.NewClient(&mcp.Implementation{Name: "initialize-single-old-client"}, nil).
		Connect(context.Background(), oldClientTransport, nil)
	require.NoError(t, err)

	candidateReady := make(chan struct{})
	candidateRelease := make(chan struct{})
	var candidateReleaseOnce sync.Once
	var candidateReadyOnce sync.Once
	var candidate *mcp.Server
	candidate = mcp.NewServer(&mcp.Implementation{Name: "initialize-single-candidate"}, &mcp.ServerOptions{
		InitializedHandler: func(context.Context, *mcp.InitializedRequest) {
			mcp.AddTool(candidate, &mcp.Tool{Name: "candidate-late"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
		},
	})
	mcp.AddTool(candidate, &mcp.Tool{Name: "candidate-initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	candidate.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				candidateReadyOnce.Do(func() { close(candidateReady) })
				select {
				case <-candidateRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next(ctx, method, request)
		}
	})
	candidateHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return candidate
	}, nil))
	cleanupTestMCPServer(t, candidate, candidateHTTP)

	store := persistedMCPStore(t, name, candidateHTTP.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		candidateReleaseOnce.Do(func() { close(candidateRelease) })
		_ = oldClientSession.Close()
		_ = oldServerSession.Close()
		closeCommitOutcomeTestOwner(t, owner)
	}()
	oldSession := &ClientSession{ClientSession: oldClientSession}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})
	allTools.Set(name, []*Tool{{Name: "old-tool"}})

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	initDone := make(chan error, 1)
	go func() { initDone <- InitializeSingle(context.Background(), name, store) }()
	select {
	case <-candidateReady:
	case <-time.After(5 * time.Second):
		t.Fatal("InitializeSingle did not reach candidate tools/list")
	}
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		for _, request := range owner.refreshPending {
			if request.deferUntilCommit && request.candidateToken != 0 {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
drainBeforeCommit:
	for {
		select {
		case event := <-events:
			if event.Payload.Type == EventToolsListChanged {
				t.Fatalf("candidate notification published before commit: %v", event)
			}
		default:
			break drainBeforeCommit
		}
	}
	candidateReleaseOnce.Do(func() { close(candidateRelease) })
	require.NoError(t, <-initDone)

	rawEvents := 0
	refreshPublished := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !refreshPublished || rawEvents != 1 {
		select {
		case event := <-events:
			if event.Payload.Type == EventToolsListChanged {
				rawEvents++
			}
			if event.Payload.Type == EventStateChanged && event.Payload.State == StateConnected && event.Payload.Counts.Tools == 2 {
				refreshPublished = true
			}
		case <-deadline.C:
			t.Fatal("committed candidate notification did not refresh the new session")
		}
	}
	require.Equal(t, 1, rawEvents)
	require.Zero(t, oldListCalls.Load())
	require.ElementsMatch(t, []string{"candidate-initial", "candidate-late"}, GetServerToolNames(name))
}

func TestInitializeSingleFailedCandidateDropsNotification(t *testing.T) {
	const name = "initialize-single-failed-notification"
	candidateReady := make(chan struct{})
	candidateRelease := make(chan struct{})
	var candidateReadyOnce sync.Once
	var candidateReleaseOnce sync.Once
	var candidate *mcp.Server
	candidate = mcp.NewServer(&mcp.Implementation{Name: "initialize-single-failed-candidate"}, &mcp.ServerOptions{
		InitializedHandler: func(context.Context, *mcp.InitializedRequest) {
			mcp.AddTool(candidate, &mcp.Tool{Name: "candidate-late"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{}, nil, nil
			})
		},
	})
	mcp.AddTool(candidate, &mcp.Tool{Name: "candidate-initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	candidate.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				candidateReadyOnce.Do(func() { close(candidateReady) })
				select {
				case <-candidateRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return nil, errors.New("injected candidate list failure")
			}
			return next(ctx, method, request)
		}
	})
	candidateHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return candidate
	}, nil))
	cleanupTestMCPServer(t, candidate, candidateHTTP)
	store := persistedMCPStore(t, name, candidateHTTP.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		candidateReleaseOnce.Do(func() { close(candidateRelease) })
		closeCommitOutcomeTestOwner(t, owner)
	}()

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	initDone := make(chan error, 1)
	go func() { initDone <- InitializeSingle(context.Background(), name, store) }()
	select {
	case <-candidateReady:
	case <-time.After(5 * time.Second):
		t.Fatal("InitializeSingle did not reach failed candidate tools/list")
	}
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		for _, request := range owner.refreshPending {
			if request.deferUntilCommit && request.candidateToken != 0 {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	candidateReleaseOnce.Do(func() { close(candidateRelease) })
	require.Error(t, <-initDone)
	time.Sleep(25 * time.Millisecond)
	for {
		select {
		case event := <-events:
			require.NotEqual(t, EventToolsListChanged, event.Payload.Type,
				"failed candidate notification must not be published")
		default:
			return
		}
	}
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
	newWaitCtx, newWaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer newWaitCancel()
	require.NoError(t, WaitForInit(newWaitCtx),
		"a new owner with no full Initialize must have an immediately completed barrier")
}

func TestWaitForInitIsImmediateWithoutFullInitialize(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForInit(ctx))
}

func TestSequentialInitializeReopensInitBarrier(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	server := mcp.NewServer(&mcp.Implementation{Name: "sequential-full-server"}, nil)
	delegate := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := false
		startedOnce.Do(func() {
			close(started)
			first = true
		})
		if first {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	cleanupTestMCPServer(t, server, httpServer)

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	defer func() { releaseOnce.Do(func() { close(release) }) }()

	store := config.NewLibraryStore(&config.Config{}, "")
	owner.Initialize(context.Background(), nil, store, false)
	require.NoError(t, WaitForInit(context.Background()))

	require.True(t, store.AddMCP("second", config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60}))
	secondDone := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(secondDone)
	}()
	waitForRequest(t, started)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	require.ErrorIs(t, WaitForInit(waitCtx), context.DeadlineExceeded)
	waitCancel()

	releaseOnce.Do(func() { close(release) })
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second full initialization did not complete after release")
	}
	require.NoError(t, WaitForInit(context.Background()))
}

func TestInitializeSingleDoesNotOwnInitBarrier(t *testing.T) {
	tests := map[string]struct {
		config  config.MCPConfig
		wantErr bool
	}{
		"failed": {
			config:  config.MCPConfig{Type: config.MCPHttp, URL: "http://127.0.0.1:1", Timeout: 1},
			wantErr: true,
		},
		"disabled": {
			config: config.MCPConfig{Type: config.MCPStdio, Command: "unused", Disabled: true},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close(context.Background())) }()
			store := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
				"single": test.config,
			}}, "")
			err = InitializeSingle(context.Background(), "single", store)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, WaitForInit(ctx))
		})
	}
}

func TestSuccessfulInitializeSingleDoesNotOwnInitBarrier(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "single-server"}, nil)
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	cleanupTestMCPServer(t, server, httpServer)

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	store := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		"single": {Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60},
	}}, "")
	require.NoError(t, InitializeSingle(context.Background(), "single", store))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForInit(ctx))
}

func TestInitializeSingleCannotCompleteFullInitBarrier(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := mcp.NewServer(&mcp.Implementation{Name: "full-server"}, nil)
	delegate := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := false
		startedOnce.Do(func() {
			close(started)
			first = true
		})
		if first {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	cleanupTestMCPServer(t, server, httpServer)

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	store := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		"full":   {Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60},
		"single": {Type: config.MCPStdio, Command: "unused", Disabled: true},
	}}, "")
	fullDone := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(fullDone)
	}()
	waitForRequest(t, started)
	require.NoError(t, InitializeSingle(context.Background(), "single", store))
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	require.ErrorIs(t, WaitForInit(waitCtx), context.DeadlineExceeded)
	waitCancel()
	close(release)
	select {
	case <-fullDone:
	case <-time.After(5 * time.Second):
		t.Fatal("full initialization did not complete after release")
	}
	require.NoError(t, WaitForInit(context.Background()))
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
