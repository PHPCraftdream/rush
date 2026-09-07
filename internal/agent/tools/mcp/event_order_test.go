package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestReplacePublishesStateEventsForResultBranches(t *testing.T) {
	tests := []struct {
		name        string
		oldName     string
		newName     string
		newDisabled bool
		absent      bool
		want        []struct {
			typeValue pubsub.EventType
			name      string
			state     State
		}
	}{
		{
			name:    "enabled",
			oldName: "replace-enabled",
			newName: "replace-enabled",
			want: []struct {
				typeValue pubsub.EventType
				name      string
				state     State
			}{{pubsub.UpdatedEvent, "replace-enabled", StateConnected}},
		},
		{
			name:        "disabled",
			oldName:     "replace-disabled",
			newName:     "replace-disabled",
			newDisabled: true,
			want: []struct {
				typeValue pubsub.EventType
				name      string
				state     State
			}{{pubsub.UpdatedEvent, "replace-disabled", StateDisabled}},
		},
		{
			name:    "absent renamed target",
			oldName: "replace-absent-old",
			newName: "replace-absent-new",
			absent:  true,
			want: []struct {
				typeValue pubsub.EventType
				name      string
				state     State
			}{
				{pubsub.DeletedEvent, "replace-absent-old", StateDisabled},
				{pubsub.DeletedEvent, "replace-absent-new", StateDisabled},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := isolatedMCPStore(t)
			oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: test.oldName}
			require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, test.oldName, oldConfig))
			owner, err := Acquire()
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close(context.Background())) }()

			oldSession := &ClientSession{}
			sessions.Set(test.oldName, oldSession)
			setState(test.oldName, StateConnected, nil, oldSession, Counts{})

			eventsCtx, cancelEvents := context.WithCancel(context.Background())
			defer cancelEvents()
			events := SubscribeEvents(eventsCtx)
			candidateConfig := config.MCPConfig{
				Type:     config.MCPStdio,
				Command:  test.newName,
				Disabled: test.newDisabled,
			}
			prepare := func(
				context.Context,
				*config.ConfigStore,
				string,
				config.MCPConfig,
				config.VariableResolver,
				*serverAdmission,
			) (*preparedClient, error) {
				return &preparedClient{session: &ClientSession{}}, nil
			}
			persist := func(
				cfg *config.ConfigStore,
				scope config.Scope,
				oldName, newName string,
				value config.MCPConfig,
			) (config.MCPMutationResult, error) {
				if test.absent {
					_, persistErr := cfg.PersistRemoveMCPConfigResult(scope, oldName)
					return config.MCPMutationResult{
						Operation: "replace",
						OldName:   oldName,
						NewName:   newName,
					}, persistErr
				}
				return cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			}

			err = replaceServerWithResultPersistenceAndPreparation(
				context.Background(), store, test.oldName, test.newName,
				candidateConfig, persist, prepare,
			)
			require.NoError(t, err)
			for _, want := range test.want {
				requireTransactionalEvent(t, events, want.typeValue, want.name, want.state)
			}
			select {
			case event := <-events:
				t.Fatalf("replacement published an extra event: %v", event)
			default:
			}
		})
	}
}

func TestStartFallbackDisabledPublishesBeforeLeaseUnlock(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "fallback-disabled-order"
	disabled := config.MCPConfig{Type: config.MCPStdio, Command: name, Disabled: true}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, disabled))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	seenBeforeUnlock := make(chan pubsub.Event[Event], 1)
	serverLeaseHooks.Lock()
	serverLeaseHooks.afterUnlockFn = func(lease *serverLease) {
		if lease.name != name {
			return
		}
		select {
		case event := <-events:
			seenBeforeUnlock <- event
		default:
		}
	}
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.afterLockFn = nil
		serverLeaseHooks.afterUnlockFn = nil
		serverLeaseHooks.Unlock()
	}()

	startFallback(context.Background(), store, name, disabled, owner)
	requireTransactionalEvent(t, seenBeforeUnlock, pubsub.UpdatedEvent, name, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("disabled fallback published a duplicate event: %v", event)
	default:
	}
}

func TestReplacePublishesBeforeBlockedCloseAndConcurrentRemove(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "replace-close-remove-order"
	oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: "old"}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)

	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	oldSession := &ClientSession{terminal: func() {
		close(closeStarted)
		<-releaseClose
	}}
	sessions.Set(name, oldSession)
	setState(name, StateConnected, nil, oldSession, Counts{})

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	releasePersistence := make(chan struct{})
	committed := make(chan struct{})
	removeAttempted := make(chan struct{})
	var removeAttemptOnce sync.Once
	serverLeaseHooks.Lock()
	serverLeaseHooks.beforeLockFn = func(lease *serverLease) {
		if lease.name == name {
			removeAttemptOnce.Do(func() { close(removeAttempted) })
		}
	}
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.afterLockFn = nil
		serverLeaseHooks.afterUnlockFn = nil
		serverLeaseHooks.Unlock()
		select {
		case <-releasePersistence:
		default:
			close(releasePersistence)
		}
		select {
		case <-releaseClose:
		default:
			close(releaseClose)
		}
		require.NoError(t, owner.Close(context.Background()))
	}()

	prepare := func(
		context.Context,
		*config.ConfigStore,
		string,
		config.MCPConfig,
		config.VariableResolver,
		*serverAdmission,
	) (*preparedClient, error) {
		return &preparedClient{session: &ClientSession{}}, nil
	}
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, name, name,
			config.MCPConfig{Type: config.MCPStdio, Command: "new"},
			func(
				cfg *config.ConfigStore,
				scope config.Scope,
				oldName, newName string,
				value config.MCPConfig,
			) (config.MCPMutationResult, error) {
				result, persistErr := cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
				close(committed)
				<-releasePersistence
				return result, persistErr
			}, prepare,
		)
	}()
	awaitMCPSignal(t, committed)

	removeDone := make(chan error, 1)
	go func() { removeDone <- RemoveServer(store, name) }()
	awaitMCPSignal(t, removeAttempted)
	close(releasePersistence)
	require.NoError(t, awaitMCPError(t, removeDone))

	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, name, StateConnected)
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, name, StateDisabled)
	close(releaseClose)
	require.NoError(t, awaitMCPError(t, replaceDone))
	select {
	case event := <-events:
		t.Fatalf("replacement/remove published an extra event: %v", event)
	default:
	}
}

func TestReplaceRevealedFallbackWaitsForBlockedCloseAndMutation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "replace-fallback-old"
	const newName = "replace-fallback-new"
	fallbackStarted := make(chan struct{})
	releaseFallback := make(chan struct{})
	var fallbackOnce sync.Once
	fallbackHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackOnce.Do(func() { close(fallbackStarted) })
		select {
		case <-r.Context().Done():
		case <-releaseFallback:
		}
	}))
	defer fallbackHTTP.Close()
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: fallbackHTTP.URL, Timeout: 60}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	require.NoError(t, store.PersistMCPConfigExact(config.ScopeWorkspace, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	closeStarted := make(chan struct{})
	var closeOnce sync.Once
	releaseClose := make(chan struct{})
	oldSession := &ClientSession{terminal: func() {
		closeOnce.Do(func() { close(closeStarted) })
		<-releaseClose
	}}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	defer func() {
		select {
		case <-releaseFallback:
		default:
			close(releaseFallback)
		}
		select {
		case <-releaseClose:
		default:
			close(releaseClose)
		}
		require.NoError(t, owner.Close(context.Background()))
	}()

	newConfig := config.MCPConfig{Type: config.MCPStdio, Command: newName}
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, newConfig,
			func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			}, func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()

	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, newName, StateConnected)
	awaitMCPSignal(t, closeStarted)
	select {
	case <-fallbackStarted:
		t.Fatal("fallback started before the detached session closed")
	default:
	}
	close(releaseClose)
	awaitMCPSignal(t, fallbackStarted)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, oldName, StateStarting)

	disableDone := make(chan error, 1)
	go func() {
		disableDone <- DisableServer(context.Background(), store, oldName)
	}()
	require.NoError(t, awaitMCPError(t, disableDone))
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, oldName, StateDisabled)

	close(releaseFallback)
	require.NoError(t, awaitMCPError(t, replaceDone))
	select {
	case event := <-events:
		if event.Payload.Name == oldName && event.Payload.State == StateConnected {
			t.Fatalf("fallback published after concurrent disable: %v", event)
		}
	default:
	}
}
