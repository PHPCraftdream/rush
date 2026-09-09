package mcp

// Regression tests for the mutation paths: pending adds and removes, the conditional-write windows around them, and the durable-outcome handling that decides whether a mutation commits or rolls back. Split out of state_regression_test.go when the 1000-line file limit landed.

import (
	"context"
	"encoding/json"
	"errors"
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

func TestPendingAddRemoveUsesOneConditionalWriteWithoutEnabledWindow(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "pending-add-conditional-remove"
	releaseInitializer := make(chan struct{})
	initializerStarted := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPHttp, URL: "http://never-enabled.example"},
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
				admission.done()
				close(initializerStarted)
				<-releaseInitializer
				return nil
			},
		)
	}()
	waitForRequest(t, initializerStarted)

	var persistenceCalls atomic.Int32
	err = removeServerWithResultPersistence(store, name, func(
		cfg *config.ConfigStore,
		scope config.Scope,
		serverName string,
	) (config.MCPMutationResult, error) {
		if persistenceCalls.Add(1) != 1 {
			return config.MCPMutationResult{}, errors.New("pending remove persisted more than once")
		}
		return cfg.PersistRemovePendingMCPConfigResult(scope, serverName)
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), persistenceCalls.Load())
	disk := diskMCP(t)
	_, persisted := disk[name]
	require.False(t, persisted, "pending removal must never persist an enabled definition")
	_, runtime := sessions.Get(name)
	require.False(t, runtime)

	close(releaseInitializer)
	require.ErrorIs(t, <-addDone, ErrOwnerBusy)
}

func TestPendingAddRemoveRejectsDurableCollision(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	store, err := config.Init(root, root, false)
	require.NoError(t, err)
	contender, err := config.Init(root, root, false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "pending-add-collision"
	releaseInitializer := make(chan struct{})
	initializerStarted := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPHttp, URL: "http://pending.example"},
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
				admission.done()
				close(initializerStarted)
				<-releaseInitializer
				return nil
			},
		)
	}()
	waitForRequest(t, initializerStarted)

	winner := config.MCPConfig{Type: config.MCPHttp, URL: "http://winner.example"}
	require.NoError(t, contender.PersistMCPConfig(config.ScopeGlobal, name, winner))
	err = RemoveServer(store, name)
	require.ErrorIs(t, err, config.ErrMCPTargetExists)
	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)[name], &persisted))
	require.Equal(t, winner, persisted)

	close(releaseInitializer)
	require.ErrorIs(t, <-addDone, config.ErrMCPTargetExists)
}

func TestPendingAddDisableUsesOneWriteAndPreservesConcurrentWriter(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "pending-add-single-write"
	addedConfig := config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     "http://pending-single-write.example",
		Timeout: 37,
		Headers: map[string]string{"Authorization": "Bearer pending"},
	}
	initializerStarted := make(chan struct{})
	releaseInitializer := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(), store, name, addedConfig,
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
				admission.done()
				close(initializerStarted)
				<-releaseInitializer
				return nil
			},
		)
	}()
	waitForRequest(t, initializerStarted)

	var persistenceCalls atomic.Int32
	selectedConfig := make(chan config.MCPConfig, 1)
	releasePersistence := make(chan struct{})
	disableDone := make(chan error, 1)
	go func() {
		disableDone <- disableServerWithPersistence(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, pending *config.MCPConfig) error {
				if persistenceCalls.Add(1) != 1 {
					return errors.New("disable persister called more than once")
				}
				if pending == nil {
					return errors.New("pending Add did not select a full config write")
				}
				selectedConfig <- *pending
				<-releasePersistence
				return cfg.PersistMCPConfig(scope, serverName, *pending)
			},
		)
	}()

	var pendingConfig config.MCPConfig
	select {
	case pendingConfig = <-selectedConfig:
	case <-time.After(time.Second):
		t.Fatal("disable did not reach the injected persistence seam")
	}
	require.Equal(t, int32(1), persistenceCalls.Load())
	require.True(t, pendingConfig.Disabled)
	require.Equal(t, addedConfig.Type, pendingConfig.Type)
	require.Equal(t, addedConfig.URL, pendingConfig.URL)
	require.Equal(t, addedConfig.Timeout, pendingConfig.Timeout)
	require.Equal(t, addedConfig.Headers, pendingConfig.Headers)

	const concurrentName = "concurrent-file-writer"
	concurrentConfig := config.MCPConfig{
		Type: config.MCPHttp,
		URL:  "http://concurrent-writer.example",
	}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, concurrentName, concurrentConfig))
	close(releasePersistence)
	require.NoError(t, <-disableDone)
	require.Equal(t, int32(1), persistenceCalls.Load())

	disk := diskMCP(t)
	var persistedPending config.MCPConfig
	require.NoError(t, json.Unmarshal(disk[name], &persistedPending))
	require.Equal(t, pendingConfig, persistedPending)
	var persistedConcurrent config.MCPConfig
	require.NoError(t, json.Unmarshal(disk[concurrentName], &persistedConcurrent))
	require.Equal(t, concurrentConfig, persistedConcurrent)

	close(releaseInitializer)
	require.ErrorIs(t, <-addDone, ErrOwnerBusy)
	require.False(t, hasPendingGlobalAdd(name))
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
	cleanupTestMCPServer(t, newServer, httpServer)
	defer func() {
		require.NoError(t, owner.Close(context.Background()))
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
	cleanupTestMCPServer(t, newServer, httpServer)
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

	old.operationMu.Lock()
	refsDuringPing := old.operationRefs
	old.operationMu.Unlock()
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
	second.close()

	require.Eventually(t, func() bool {
		old.operationMu.Lock()
		defer old.operationMu.Unlock()
		return old.retired && old.operationRefs == 0
	}, time.Second, time.Millisecond)
}

func TestRenewedSessionNotificationSurvivesConfigMutations(t *testing.T) {
	const name = "renewed-notification"
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-notification-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)

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

func TestPublishedHTTPNotificationSurvivesUnrelatedReload(t *testing.T) {
	const name = "reload-live-notification"
	server := mcp.NewServer(&mcp.Implementation{Name: "reload-live-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	owner.Initialize(context.Background(), nil, store, false)
	require.Eventually(t, func() bool { return hasSession(name) }, 5*time.Second, time.Millisecond)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.debug", true))
	mcp.AddTool(server, &mcp.Tool{Name: "after-reload"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})

	seenNotification := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenNotification {
		select {
		case event := <-events:
			seenNotification = event.Payload.Name == name && event.Payload.Type == EventToolsListChanged
		case <-deadline.C:
			t.Fatal("published session lost its live notification after an unrelated reload")
		}
	}
	require.Eventually(t, func() bool {
		return len(GetServerToolNames(name)) == 2
	}, 5*time.Second, time.Millisecond)
}

func TestReloadFencesPublishedSessionAfterRelevantMCPChange(t *testing.T) {
	const name = "reload-replaced-session"
	server := mcp.NewServer(&mcp.Implementation{Name: "reload-replaced-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "initial"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)
	require.Eventually(t, func() bool { return hasSession(name) }, 5*time.Second, time.Millisecond)

	require.NoError(t, store.PersistMCPFields(config.ScopeGlobal, name, map[string]any{
		"url": "http://replacement.invalid/mcp",
	}))
	require.NoError(t, ReloadAndReconcileMCPConfig(context.Background(), store))
	require.Eventually(t, func() bool { return !hasSession(name) }, 5*time.Second, time.Millisecond)
	require.Empty(t, GetServerToolNames(name))
}

func TestInvalidCommittedNotificationClearsAllAdvertisedData(t *testing.T) {
	const name = "invalid-committed-notification"
	value := config.MCPConfig{Type: config.MCPStdio, Command: "server"}
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{name: value}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{}
	sessions.Set(name, session)
	allTools.Set(name, []*Tool{{Name: "stale-tool"}})
	allPrompts.Set(name, []*Prompt{{Name: "stale-prompt"}})
	allResources.Set(name, []*Resource{{Name: "stale-resource", URI: "stale://resource"}})
	setState(name, StateConnected, nil, session, Counts{Tools: 1, Prompts: 1, Resources: 1})

	lifecycleMu.Lock()
	admission := &serverAdmission{
		owner: owner, generation: owner.generation, epoch: owner.serverEpochs[name],
		cfg: store, name: name, committed: true, committedName: name,
		committedEpoch: owner.serverEpochs[name], publishedSession: session,
		configIdentity: value, hasConfigIdentity: true,
	}
	owner.committedAdmissions[name] = admission
	lifecycleMu.Unlock()
	_, ok := store.SetMCPDisabled(name, true)
	require.True(t, ok)

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	notifyListChanged(admission, name, refreshToolsKind)

	require.Empty(t, GetServerToolNames(name))
	_, ok = allPrompts.Get(name)
	require.False(t, ok)
	_, ok = allResources.Get(name)
	require.False(t, ok)
	_, ok = sessions.Get(name)
	require.False(t, ok)
	select {
	case event := <-events:
		require.Equal(t, EventStateChanged, event.Payload.Type)
		require.Equal(t, StateDisabled, event.Payload.State)
		require.Equal(t, name, event.Payload.Name)
	case <-time.After(time.Second):
		t.Fatal("invalid committed session did not publish one disabled event")
	}
	select {
	case event := <-events:
		t.Fatalf("invalid committed session published an extra event: %v", event)
	default:
	}
}

func TestGetOrRenewFencesStaleCommittedSessionBeforePing(t *testing.T) {
	const name = "lazy-stale-session"
	var pingCalls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "lazy-stale-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "stale-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "ping" {
				pingCalls.Add(1)
			}
			return next(ctx, method, request)
		}
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	cleanupTestMCPServer(t, server, httpServer)
	store := persistedMCPStore(t, name, httpServer.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.Initialize(context.Background(), nil, store, false)
	require.Eventually(t, func() bool { return hasSession(name) }, 5*time.Second, time.Millisecond)

	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name+".url", "http://replacement.invalid/mcp"))
	client, err := getOrRenewClient(context.Background(), store, name)
	require.Error(t, err)
	require.Nil(t, client)
	require.Zero(t, pingCalls.Load(), "stale session was used before validation")
	require.Eventually(t, func() bool { return !hasSession(name) }, time.Second, time.Millisecond)
	require.Empty(t, GetServerToolNames(name))
	state, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)
}

func TestReloadReconciliationWaitsForNewerLeaseWinner(t *testing.T) {
	const name = "reload-lease-winner"
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://old.example"}
	newConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://new.example"}
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{name: newConfig}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	oldSession := &ClientSession{}
	newSession := &ClientSession{}
	oldAdmission := &serverAdmission{
		owner: owner, generation: owner.generation, epoch: owner.serverEpochs[name],
		cfg: store, name: name, committed: true, committedName: name,
		committedEpoch: owner.serverEpochs[name], publishedSession: oldSession,
		configIdentity: oldConfig, hasConfigIdentity: true,
	}
	lifecycleMu.Lock()
	sessions.Set(name, oldSession)
	owner.committedAdmissions[name] = oldAdmission
	lifecycleMu.Unlock()
	allTools.Set(name, []*Tool{{Name: "old-tool"}})

	lease := serverLeaseFor(name)
	lease.Lock()
	waiter := make(chan struct{})
	var waiterOnce sync.Once
	serverLeaseHooks.Lock()
	serverLeaseHooks.beforeLockFn = func(candidate *serverLease) {
		if candidate == lease {
			waiterOnce.Do(func() { close(waiter) })
		}
	}
	serverLeaseHooks.Unlock()
	t.Cleanup(func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.Unlock()
	})

	reconcileDone := make(chan struct{})
	go func() {
		owner.reconcilePublishedSessions(store)
		close(reconcileDone)
	}()
	select {
	case <-waiter:
	case <-time.After(time.Second):
		lease.Unlock()
		t.Fatal("reconciliation did not reach the server lease")
	}

	lifecycleMu.Lock()
	owner.serverEpochs[name]++
	newAdmission := &serverAdmission{
		owner: owner, generation: owner.generation, epoch: owner.serverEpochs[name],
		cfg: store, name: name, committed: true, committedName: name,
		committedEpoch: owner.serverEpochs[name], publishedSession: newSession,
		configIdentity: newConfig, hasConfigIdentity: true,
	}
	sessions.Set(name, newSession)
	owner.committedAdmissions[name] = newAdmission
	allTools.Set(name, []*Tool{{Name: "new-tool"}})
	setState(name, StateConnected, nil, newSession, Counts{Tools: 1})
	lifecycleMu.Unlock()
	lease.Unlock()
	select {
	case <-reconcileDone:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not finish after the server lease was released")
	}
	current, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, newSession, current)
	require.Equal(t, []string{"new-tool"}, GetServerToolNames(name))
	state, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
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

func TestDirtyRefreshFailuresRetainCurrentSession(t *testing.T) {
	const name = "dirty-refresh-failures"
	firstObserved := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	var listCalls atomic.Int32
	var closeCalls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "dirty-refresh-failures-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "run"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				call := listCalls.Add(1)
				if call == 1 {
					close(firstObserved)
					<-releaseFirst
				}
				if call <= 2 {
					return nil, errors.New("injected refresh failure")
				}
			}
			return next(ctx, method, request)
		}
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dirty-refresh-failures-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)

	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{
		ClientSession: clientSession,
		cancel:        clientCancel,
		cleanup:       func() { closeCalls.Add(1) },
	}
	initialCounts := Counts{Tools: 7, Prompts: 5, Resources: 3}
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session, Counts: initialCounts})
	admission, err := owner.admitServer(context.Background(), store, name, true)
	require.NoError(t, err)
	defer admission.done()
	defer func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }()

	notifyListChanged(&admission, name, refreshToolsKind)
	waitForRequest(t, firstObserved)
	notifyListChanged(&admission, name, refreshToolsKind)
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	require.Eventually(t, func() bool {
		info, ok := GetState(name)
		return ok && info.State == StateError && listCalls.Load() == 2
	}, 5*time.Second, time.Millisecond, "two dirty refresh failures did not complete")
	require.Same(t, session, func() *ClientSession {
		current, _ := sessions.Get(name)
		return current
	}())
	failedState, ok := GetState(name)
	require.True(t, ok)
	require.Same(t, session, failedState.Client)
	require.Equal(t, initialCounts, failedState.Counts)
	require.Equal(t, int32(0), closeCalls.Load(), "refresh failures must not close the live session")

	notifyListChanged(&admission, name, refreshToolsKind)
	require.Eventually(t, func() bool {
		info, ok := GetState(name)
		return ok && info.State == StateConnected && listCalls.Load() == 3
	}, 5*time.Second, time.Millisecond, "a later refresh did not recover")
	_, err = RunTool(context.Background(), store, name, "run", `{}`)
	require.NoError(t, err)
	require.Equal(t, int32(0), closeCalls.Load(), "recovering RunTool must retain the session")
}

func TestExportedRefreshFailuresRetainSessionCountsAndRecover(t *testing.T) {
	const name = "exported-refresh-failures"
	var calls sync.Map
	server := mcp.NewServer(&mcp.Implementation{Name: "exported-refresh-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "prompt"}, nil)
	server.AddResource(&mcp.Resource{URI: "file:///resource", Name: "resource"}, nil)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" || method == "prompts/list" || method == "resources/list" {
				value, _ := calls.LoadOrStore(method, new(atomic.Int32))
				count := value.(*atomic.Int32).Add(1)
				if count <= 2 {
					return nil, errors.New("injected exported refresh failure")
				}
			}
			return next(ctx, method, request)
		}
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "exported-refresh-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)

	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}
	initialCounts := Counts{Tools: 7, Prompts: 5, Resources: 3}
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session, Counts: initialCounts})

	refreshes := []struct {
		name   string
		call   func()
		counts Counts
	}{
		{name: "tools", call: func() { RefreshTools(context.Background(), store, name) }, counts: Counts{Tools: 1, Prompts: 5, Resources: 3}},
		{name: "prompts", call: func() { RefreshPrompts(context.Background(), name) }, counts: Counts{Tools: 1, Prompts: 1, Resources: 3}},
		{name: "resources", call: func() { RefreshResources(context.Background(), name) }, counts: Counts{Tools: 1, Prompts: 1, Resources: 1}},
	}
	for _, refresh := range refreshes {
		for range 2 {
			refresh.call()
			state, ok := GetState(name)
			require.True(t, ok)
			require.Equal(t, StateError, state.State)
			require.Same(t, session, state.Client)
			require.Equal(t, initialCounts, state.Counts)
		}
		refresh.call()
		state, ok := GetState(name)
		require.True(t, ok)
		require.Equal(t, StateConnected, state.State)
		require.Same(t, session, state.Client)
		require.Equal(t, refresh.counts, state.Counts)
		initialCounts = refresh.counts
	}
}
