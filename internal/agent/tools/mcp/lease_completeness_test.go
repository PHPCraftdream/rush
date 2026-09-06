package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestSuccessfulAddRemoveReclaimsEveryLeaseReference(t *testing.T) {
	const name = "successful-add-lease"
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	mcpConfig := config.MCPConfig{Type: config.MCPStdio, Command: "unused"}
	initialize := func(
		_ context.Context,
		cfg *config.ConfigStore,
		serverName string,
		_ config.MCPConfig,
		_ config.VariableResolver,
		admission *serverAdmission,
	) error {
		defer admission.done()
		return publishPreparedClient(cfg, serverName, &preparedClient{
			session: &ClientSession{},
		}, admission)
	}

	require.NoError(t, addServerWithInitializer(context.Background(), store, name, mcpConfig, initialize))
	require.Zero(t, leases.Len(), "a successful add must release its initialization identity reference")
	require.NoError(t, RemoveServer(store, name))
	require.Zero(t, leases.Len(), "add followed by remove must reclaim the lease registry entry")
}

func TestAddRollbackConsumesItsLastLeaseReferenceOnce(t *testing.T) {
	tests := map[string]struct {
		cancelBeforeReacquire bool
		want                  error
	}{
		"initializer error": {
			want: errors.New("initializer failed"),
		},
		"canceled reacquire": {
			cancelBeforeReacquire: true,
			want:                  context.Canceled,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := isolatedMCPStore(t)
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close(context.Background())) }()

			const serverName = "add-rollback-lease"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			initializerReady := make(chan struct{})
			releaseInitializer := make(chan struct{})
			waiterRetained := make(chan struct{})
			waiterAcquired := make(chan struct{})
			releaseWaiter := make(chan struct{})
			initialize := func(
				_ context.Context,
				_ *config.ConfigStore,
				_ string,
				_ config.MCPConfig,
				_ config.VariableResolver,
				admission *serverAdmission,
			) error {
				defer admission.done()
				blocker := serverLeaseFor(serverName)
				blocker.Lock()
				go func() {
					waiter := serverLeaseFor(serverName)
					close(waiterRetained)
					waiter.Lock()
					close(waiterAcquired)
					<-releaseWaiter
					waiter.Unlock()
				}()
				<-waiterRetained
				close(initializerReady)
				<-releaseInitializer
				blocker.Unlock()
				<-waiterAcquired
				if test.cancelBeforeReacquire {
					return nil
				}
				return test.want
			}

			addDone := make(chan error, 1)
			go func() {
				addDone <- addServerWithInitializer(
					ctx, store, serverName,
					config.MCPConfig{Type: config.MCPStdio, Command: "unused"},
					initialize,
				)
			}()
			waitForRequest(t, initializerReady)
			if test.cancelBeforeReacquire {
				cancel()
			}
			close(releaseInitializer)
			waitForRequest(t, waiterAcquired)
			close(releaseWaiter)

			got := <-addDone
			if test.cancelBeforeReacquire {
				require.ErrorIs(t, got, context.Canceled)
			} else {
				require.ErrorIs(t, got, test.want)
			}
			require.Zero(t, leases.Len())
			_, exists := store.MCPConfig(serverName)
			require.False(t, exists)
		})
	}
}

func TestRetiredClientCannotPublishAfterReplacement(t *testing.T) {
	const name = "retired-publication"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.rememberConfig(store)

	oldSession := &ClientSession{}
	newSession := &ClientSession{}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})
	oldLease, err := currentClientLease(context.Background(), name, store)
	require.NoError(t, err)
	defer oldLease.close()

	serverLease := serverLeaseFor(name)
	serverLease.Lock()
	owner.invalidateServer(name)
	sessions.Set(name, newSession)
	setState(name, StateConnected, nil, newSession, Counts{})
	allTools.Set(name, []*Tool{{Name: "winner-tool"}})
	allPrompts.Set(name, []*Prompt{{Name: "winner-prompt"}})
	allResources.Set(name, []*Resource{{Name: "winner-resource", URI: "winner://resource"}})
	serverLease.Unlock()

	published := oldLease.publishIfCurrent(context.Background(), func() {
		allTools.Set(name, []*Tool{{Name: "stale-tool"}})
		allPrompts.Set(name, []*Prompt{{Name: "stale-prompt"}})
		allResources.Set(name, []*Resource{{Name: "stale-resource", URI: "stale://resource"}})
		setState(name, StateConnected, nil, oldSession, Counts{})
	}, nil)
	require.False(t, published)
	require.Equal(t, "winner-tool", GetServerToolNames(name)[0])
	prompts, ok := allPrompts.Get(name)
	require.True(t, ok)
	require.Equal(t, "winner-prompt", prompts[0].Name)
	resources, ok := allResources.Get(name)
	require.True(t, ok)
	require.Equal(t, "winner-resource", resources[0].Name)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Same(t, newSession, state.Client)
}

func TestExportedRefreshResourcesRejectsStaleResultAfterReplacement(t *testing.T) {
	const name = "exported-refresh-resources"
	publishStarted := make(chan struct{}, 1)
	releasePublish := make(chan struct{})
	var releasePublishOnce sync.Once
	server := mcp.NewServer(&mcp.Implementation{Name: "resources-server"}, nil)
	server.AddResource(&mcp.Resource{URI: "old://resource", Name: "old-resource"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	oldClient, err := mcp.NewClient(&mcp.Implementation{Name: "resources-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	defer oldClient.Close()

	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	owner.rememberConfig(store)
	resourcesBeforePublishHook.Lock()
	resourcesBeforePublishHook.fn = func() {
		publishStarted <- struct{}{}
		<-releasePublish
	}
	resourcesBeforePublishHook.Unlock()
	defer func() {
		resourcesBeforePublishHook.Lock()
		resourcesBeforePublishHook.fn = nil
		resourcesBeforePublishHook.Unlock()
		releasePublishOnce.Do(func() { close(releasePublish) })
	}()
	oldSession := &ClientSession{ClientSession: oldClient}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{Resources: 1})

	refreshDone := make(chan struct{})
	go func() {
		RefreshResources(context.Background(), name)
		close(refreshDone)
	}()
	select {
	case <-publishStarted:
	case <-time.After(time.Second):
		t.Fatal("exported resource refresh did not reach the post-response publication seam")
	}

	newSession := &ClientSession{}
	serverLease := serverLeaseFor(name)
	serverLease.Lock()
	owner.invalidateServer(name)
	sessions.Set(name, newSession)
	allResources.Set(name, []*Resource{{URI: "new://resource", Name: "new-resource"}})
	setState(name, StateConnected, nil, newSession, Counts{Resources: 1})
	serverLease.Unlock()
	releasePublishOnce.Do(func() { close(releasePublish) })

	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("exported resource refresh did not finish")
	}
	resources, ok := allResources.Get(name)
	require.True(t, ok)
	require.Equal(t, "new-resource", resources[0].Name)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.Same(t, newSession, state.Client)
}

func TestLockContextRejectsCanceledContextWithoutAcquisition(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			lease := serverLeaseFor("canceled-lock")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			require.False(t, lease.lockContext(ctx, write))
			require.Zero(t, leases.Len())
		})
	}
}

func TestDisableCancelsRenewalAndReleasesFollowers(t *testing.T) {
	const name = "canceled-renewal"
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)

	oldServer := mcp.NewServer(&mcp.Implementation{Name: "old-server"}, nil)
	oldServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "ping" {
				return nil, errors.New("force renewal")
			}
			return next(ctx, method, request)
		}
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := oldServer.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	oldClient, err := mcp.NewClient(&mcp.Implementation{Name: "old-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)

	store := persistedMCPStore(t, name, blocked.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	old := &ClientSession{ClientSession: oldClient}
	sessions.Set(name, old)
	setState(name, StateConnected, nil, old, Counts{})

	renewed := make(chan error, 1)
	go func() {
		_, renewErr := getOrRenewClient(context.Background(), store, name)
		renewed <- renewErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("renewal did not reach the blocked initializer")
	}

	followerCtx, cancelFollower := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelFollower()
	follower := make(chan error, 1)
	go func() {
		_, followerErr := getOrRenewClient(followerCtx, store, name)
		follower <- followerErr
	}()
	require.NoError(t, DisableServer(context.Background(), store, name))
	select {
	case err := <-renewed:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("stalled renewal did not exit after disable")
	}
	select {
	case err := <-follower:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("renewal follower remained behind the canceled initializer")
	}
}
