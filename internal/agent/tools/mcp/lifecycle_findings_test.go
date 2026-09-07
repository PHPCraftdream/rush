package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestReplaceRejectsStalePersisterResultWithoutPublishingCandidate(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "stale-result-old"
	const newName = "stale-result-new"
	oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: oldName}
	newConfig := config.MCPConfig{Type: config.MCPStdio, Command: newName}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession := &ClientSession{}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})
	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	candidate := &ClientSession{}

	err = replaceServerWithResultPersistenceAndPreparation(
		context.Background(), store, oldName, newName, newConfig,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			if persistErr != nil {
				return result, persistErr
			}
			_, persistErr = cfg.PersistMCPDisabledOverrideResult(scope, newName, true)
			return result, persistErr
		},
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: candidate}, nil
		},
	)
	require.NoError(t, err)
	current, ok := store.MCPConfig(newName)
	require.True(t, ok)
	require.True(t, current.Disabled)
	require.False(t, hasSession(oldName))
	require.False(t, hasSession(newName))
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, oldName, StateDisabled)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, newName, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("stale replacement published an extra event: %v", event)
	default:
	}
}

func TestReplaceRejectsCrossStoreMutationBeforePin(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const oldName = "cross-store-old"
	const newName = "cross-store-new"
	oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: oldName}
	newConfig := config.MCPConfig{Type: config.MCPStdio, Command: newName}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession := &ClientSession{}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})
	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	candidate := &ClientSession{}

	err = replaceServerWithResultPersistenceAndPreparation(
		context.Background(), store, oldName, newName, newConfig,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			if persistErr == nil {
				_, persistErr = contender.PersistMCPDisabledOverrideResult(scope, newName, true)
			}
			return result, persistErr
		},
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: candidate}, nil
		},
	)
	require.NoError(t, err)
	current, ok := contender.MCPConfig(newName)
	require.True(t, ok)
	require.True(t, current.Disabled)
	require.False(t, hasSession(oldName))
	require.False(t, hasSession(newName))
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, oldName, StateDisabled)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, newName, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("cross-store stale replacement published an extra event: %v", event)
	default:
	}
}

func TestMCPMutationPinHoldsSidecarsThroughLifecyclePublication(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const name = "cross-store-pinned"
	pinned := config.MCPConfig{Type: config.MCPStdio, Command: name}
	result, err := store.PersistMCPConfigResult(config.ScopeGlobal, name, pinned)
	require.NoError(t, err)

	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	candidate := &ClientSession{}
	t.Cleanup(func() {
		sessions.Del(name)
		states.Del(name)
	})

	lifecycleMu.Lock()
	callbackEntered := make(chan struct{})
	publicationDone := make(chan error, 1)
	go func() {
		publicationDone <- store.WithCurrentMCPMutation(result, func() error {
			close(callbackEntered)
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			sessions.Set(name, candidate)
			setState(name, StateConnected, nil, candidate, Counts{})
			broker.Publish(pubsub.UpdatedEvent, Event{Type: EventStateChanged, Name: name, State: StateConnected})
			return nil
		})
	}()
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		lifecycleMu.Unlock()
		t.Fatal("pinned publication did not reach the lifecycle boundary")
	}

	writerDone := make(chan error, 1)
	go func() {
		_, writerErr := contender.PersistMCPDisabledOverrideResult(config.ScopeGlobal, name, true)
		writerDone <- writerErr
	}()
	select {
	case writerErr := <-writerDone:
		lifecycleMu.Unlock()
		t.Fatalf("second store crossed the pinned sidecar boundary: %v", writerErr)
	case <-time.After(100 * time.Millisecond):
	}
	lifecycleMu.Unlock()

	require.NoError(t, <-publicationDone)
	require.NoError(t, <-writerDone)
	got, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, candidate, got)
	require.Equal(t, StateConnected, mustState(t, name).State)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, name, StateConnected)
	current, ok := contender.MCPConfig(name)
	require.True(t, ok)
	require.True(t, current.Disabled)
}

func TestReplaceKnownCommitFencesRuntimeAfterEpochInvalidation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "post-commit-epoch-old"
	const newName = "post-commit-epoch-new"
	oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: oldName}
	newConfig := config.MCPConfig{Type: config.MCPStdio, Command: newName}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession := &ClientSession{}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})
	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	candidate := &ClientSession{}
	err = replaceServerWithResultPersistenceAndPreparation(
		context.Background(), store, oldName, newName, newConfig,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			if persistErr == nil {
				owner.invalidateServer(oldName)
			}
			return result, persistErr
		},
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: candidate}, nil
		},
	)
	require.NoError(t, err)
	_, ok := store.MCPConfig(newName)
	require.True(t, ok)
	require.False(t, hasSession(oldName))
	require.False(t, hasSession(newName))
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, oldName, StateDisabled)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, newName, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("epoch-invalidated replacement published an extra event: %v", event)
	default:
	}
}

func TestDisableRechecksUncertaintyAfterWaitingForLease(t *testing.T) {
	testLeaseWaitUncertainty(t, false)
}

func TestRemoveRechecksUncertaintyAfterWaitingForLease(t *testing.T) {
	testLeaseWaitUncertainty(t, true)
}

func TestDisableLeaseWaitUncertaintyDuringClose(t *testing.T) {
	testLeaseWaitUncertaintyDuringClose(t, false)
}

func TestRemoveLeaseWaitUncertaintyDuringClose(t *testing.T) {
	testLeaseWaitUncertaintyDuringClose(t, true)
}

func TestMutationPublicationTakesServerLeaseBeforeConfigLocks(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "publication-lock-order"
	mcpConfig := config.MCPConfig{Type: config.MCPStdio, Command: name}
	result, err := store.PersistMCPConfigResult(config.ScopeGlobal, name, mcpConfig)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	releasePersist := make(chan struct{})
	defer func() {
		select {
		case <-releasePersist:
		default:
			close(releasePersist)
		}
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeTryLockFn = nil
		serverLeaseHooks.Unlock()
		require.NoError(t, owner.Close(context.Background()))
	}()

	persistEntered := make(chan struct{})
	disableDone := make(chan error, 1)
	go func() {
		disableDone <- disableServerWithResultPersistence(context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, _ *config.MCPConfig) (config.MCPMutationResult, error) {
				close(persistEntered)
				<-releasePersist
				return cfg.PersistMCPDisabledOverrideResult(scope, serverName, true)
			})
	}()
	awaitMCPSignal(t, persistEntered)

	admission, err := owner.admitServerForConfig(context.Background(), store, name, mcpConfig, true)
	require.NoError(t, err)
	defer admission.done()
	admission.mutationResult = &result
	candidate := &preparedClient{session: &ClientSession{}}

	leases.mu.Lock()
	lease := leases.entries[name]
	leases.mu.Unlock()
	require.NotNil(t, lease)
	helperAttempted := make(chan struct{})
	var helperAttemptOnce sync.Once
	var helperStarted atomic.Bool
	serverLeaseHooks.Lock()
	serverLeaseHooks.beforeTryLockFn = func(candidateLease *serverLease) {
		if helperStarted.Load() && candidateLease == lease {
			helperAttemptOnce.Do(func() { close(helperAttempted) })
		}
	}
	serverLeaseHooks.Unlock()

	helperDone := make(chan error, 1)
	helperStarted.Store(true)
	go func() {
		helperDone <- publishPreparedClientWithMutation(store, name, candidate, &admission)
	}()
	awaitMCPSignal(t, helperAttempted)
	close(releasePersist)
	require.NoError(t, awaitMCPError(t, disableDone))
	require.ErrorIs(t, awaitMCPError(t, helperDone), context.Canceled)
}

func testLeaseWaitUncertaintyDuringClose(t *testing.T, remove bool) {
	t.Helper()
	store := isolatedMCPStore(t)
	const name = "lease-wait-close-uncertain"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPStdio, Command: name,
	}))
	owner, err := Acquire()
	require.NoError(t, err)

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	aDone := make(chan error, 1)
	if remove {
		go func() {
			aDone <- removeServerWithResultPersistence(store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string) (config.MCPMutationResult, error) {
					close(persistStarted)
					<-releasePersist
					result, persistErr := cfg.PersistRemoveMCPConfigResult(scope, serverName)
					return result, errors.Join(persistErr, injectedMCPMaybeCommitted())
				})
		}()
	} else {
		go func() {
			aDone <- disableServerWithResultPersistence(context.Background(), store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string, _ *config.MCPConfig) (config.MCPMutationResult, error) {
					close(persistStarted)
					<-releasePersist
					result, persistErr := cfg.PersistMCPDisabledOverrideResult(scope, serverName, true)
					return result, errors.Join(persistErr, injectedMCPMaybeCommitted())
				})
		}()
	}
	awaitMCPSignal(t, persistStarted)

	leases.mu.Lock()
	lease := leases.entries[name]
	leases.mu.Unlock()
	require.NotNil(t, lease)
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	serverLeaseHooks.Lock()
	serverLeaseHooks.beforeLockFn = func(candidate *serverLease) {
		if candidate == lease {
			waitingOnce.Do(func() { close(waiting) })
		}
	}
	serverLeaseHooks.beforeTryLockFn = serverLeaseHooks.beforeLockFn
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.beforeTryLockFn = nil
		serverLeaseHooks.afterLockFn = nil
		serverLeaseHooks.afterUnlockFn = nil
		serverLeaseHooks.Unlock()
		select {
		case <-releasePersist:
		default:
			close(releasePersist)
		}
		require.NoError(t, owner.Close(context.Background()))
	}()

	bPersisted := atomic.Bool{}
	bDone := make(chan error, 1)
	if remove {
		go func() {
			bDone <- removeServerWithResultPersistence(store, name,
				func(*config.ConfigStore, config.Scope, string) (config.MCPMutationResult, error) {
					bPersisted.Store(true)
					return config.MCPMutationResult{}, errors.New("unexpected remove persistence")
				})
		}()
	} else {
		go func() {
			bDone <- disableServerWithResultPersistence(context.Background(), store, name,
				func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error) {
					bPersisted.Store(true)
					return config.MCPMutationResult{}, errors.New("unexpected disable persistence")
				})
		}()
	}
	awaitMCPSignal(t, waiting)
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	close(releasePersist)
	requireMCPMaybeCommitted(t, awaitMCPError(t, aDone))
	require.ErrorIs(t, awaitMCPError(t, bDone), ErrMCPConfigUncertain)
	require.False(t, bPersisted.Load())
	require.NoError(t, awaitMCPError(t, closeDone))
}

func testLeaseWaitUncertainty(t *testing.T, remove bool) {
	t.Helper()
	store := isolatedMCPStore(t)
	name := "lease-wait-uncertain"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPStdio, Command: name,
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	aDone := make(chan error, 1)
	if remove {
		go func() {
			aDone <- removeServerWithResultPersistence(store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string) (config.MCPMutationResult, error) {
					close(persistStarted)
					<-releasePersist
					result, persistErr := cfg.PersistRemoveMCPConfigResult(scope, serverName)
					return result, errors.Join(persistErr, injectedMCPMaybeCommitted())
				})
		}()
	} else {
		go func() {
			aDone <- disableServerWithResultPersistence(context.Background(), store, name,
				func(cfg *config.ConfigStore, scope config.Scope, serverName string, _ *config.MCPConfig) (config.MCPMutationResult, error) {
					close(persistStarted)
					<-releasePersist
					result, persistErr := cfg.PersistMCPDisabledOverrideResult(scope, serverName, true)
					return result, errors.Join(persistErr, injectedMCPMaybeCommitted())
				})
		}()
	}
	awaitMCPSignal(t, persistStarted)
	leases.mu.Lock()
	lease := leases.entries[name]
	leases.mu.Unlock()
	require.NotNil(t, lease)
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	serverLeaseHooks.Lock()
	waitingHook := func(candidate *serverLease) {
		if candidate == lease {
			waitingOnce.Do(func() { close(waiting) })
		}
	}
	serverLeaseHooks.beforeLockFn = waitingHook
	serverLeaseHooks.beforeTryLockFn = waitingHook
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.beforeTryLockFn = nil
		serverLeaseHooks.afterLockFn = nil
		serverLeaseHooks.afterUnlockFn = nil
		serverLeaseHooks.Unlock()
		select {
		case <-releasePersist:
		default:
			close(releasePersist)
		}
	}()

	bPersisted := atomic.Bool{}
	bDone := make(chan error, 1)
	if remove {
		go func() {
			bDone <- removeServerWithResultPersistence(store, name,
				func(*config.ConfigStore, config.Scope, string) (config.MCPMutationResult, error) {
					bPersisted.Store(true)
					return config.MCPMutationResult{}, errors.New("unexpected remove persistence")
				})
		}()
	} else {
		go func() {
			bDone <- disableServerWithResultPersistence(context.Background(), store, name,
				func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error) {
					bPersisted.Store(true)
					return config.MCPMutationResult{}, errors.New("unexpected disable persistence")
				})
		}()
	}
	awaitMCPSignal(t, waiting)
	close(releasePersist)
	requireMCPMaybeCommitted(t, awaitMCPError(t, aDone))
	require.ErrorIs(t, awaitMCPError(t, bDone), ErrMCPConfigUncertain)
	require.False(t, bPersisted.Load())
}

func TestEnableRejectsChangedConfigBeforePublication(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const name = "enable-stale-config"
	initial := config.MCPConfig{Type: config.MCPHttp, URL: "http://enable-a.example", Disabled: true}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, initial))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	persisted := make(chan struct{})
	releasePersist := make(chan struct{})
	enableDone := make(chan error, 1)
	initializerDone := make(chan error, 1)
	go func() {
		enableDone <- enableServerWithPersistenceAndInitializer(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := enableMCPConfig(cfg, scope, serverName, pending)
				close(persisted)
				<-releasePersist
				return result, persistErr
			},
			func(_ context.Context, cfg *config.ConfigStore, serverName string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) error {
				defer admission.done()
				candidate := &ClientSession{}
				err := publishPreparedClient(cfg, serverName, &preparedClient{session: candidate}, admission)
				initializerDone <- err
				return err
			},
		)
	}()
	awaitMCPSignal(t, persisted)
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://enable-b.example"}
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": replacement.URL,
	}))
	close(releasePersist)
	require.NoError(t, awaitMCPError(t, enableDone))
	require.ErrorIs(t, awaitMCPError(t, initializerDone), config.ErrMCPMutationStale)
	require.False(t, hasSession(name))
	current, ok := contender.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, replacement, current)
}

func TestAddRejectsStoreGenerationChangedDuringPreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "add-stale-generation"
	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithPreparationAndPersistence(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPHttp, URL: "http://generation.example"},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				close(prepareStarted)
				<-releasePrepare
				return &preparedClient{session: &ClientSession{}}, nil
			},
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistMCPConfigResult(scope, serverName, value)
			},
		)
	}()
	awaitMCPSignal(t, prepareStarted)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.debug", true))
	close(releasePrepare)
	require.ErrorIs(t, awaitMCPError(t, addDone), ErrOwnerBusy)
	require.False(t, hasSession(name))
	_, exists := store.MCPConfig(name)
	require.False(t, exists)
}

func TestEnableRejectsStoreGenerationChangedBeforePublication(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "enable-stale-generation"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://generation-enable.example", Disabled: true,
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	persisted := make(chan struct{})
	releasePersist := make(chan struct{})
	initializerStarted := make(chan struct{})
	releaseInitializer := make(chan struct{})
	enableDone := make(chan error, 1)
	initializerDone := make(chan error, 1)
	go func() {
		enableDone <- enableServerWithPersistenceAndInitializer(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := enableMCPConfig(cfg, scope, serverName, pending)
				close(persisted)
				<-releasePersist
				return result, persistErr
			},
			func(_ context.Context, cfg *config.ConfigStore, serverName string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) error {
				defer admission.done()
				close(initializerStarted)
				<-releaseInitializer
				err := publishPreparedClient(cfg, serverName, &preparedClient{session: &ClientSession{}}, admission)
				initializerDone <- err
				return err
			},
		)
	}()
	awaitMCPSignal(t, persisted)
	close(releasePersist)
	awaitMCPSignal(t, initializerStarted)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.debug", true))
	close(releaseInitializer)
	require.NoError(t, awaitMCPError(t, enableDone))
	require.ErrorIs(t, awaitMCPError(t, initializerDone), config.ErrMCPMutationStale)
	require.False(t, hasSession(name))
}

func TestMCPAdmissionSurvivesUnrelatedCOWUpdate(t *testing.T) {
	const name = "unrelated-cow"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: name},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	require.NoError(t, owner.rememberConfig(store))

	mcpConfig := config.MCPConfig{Type: config.MCPStdio, Command: name}
	admission, err := owner.admitServerForConfig(context.Background(), store, name, mcpConfig, true)
	require.NoError(t, err)
	defer admission.done()
	store.SetSkipPermissionRequests(true)
	require.True(t, admission.valid())
}

func TestAddCloseRollsBackCanceledPendingConfig(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "close-pending-rollback"
	owner, err := Acquire()
	require.NoError(t, err)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	addDone := make(chan error, 1)
	var persistCalled atomic.Bool
	go func() {
		addDone <- addServerWithPreparationAndPersistence(
			ctx, store, name,
			config.MCPConfig{Type: config.MCPStdio, Command: name},
			func(_ context.Context, _ *config.ConfigStore, _ string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) (*preparedClient, error) {
				close(started)
				<-admission.ctx.Done()
				return nil, admission.ctx.Err()
			},
			func(*config.ConfigStore, config.Scope, string, config.MCPConfig) (config.MCPMutationResult, error) {
				persistCalled.Store(true)
				return config.MCPMutationResult{}, errors.New("unexpected persistence")
			},
		)
	}()
	awaitMCPSignal(t, started)
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Error(t, awaitMCPError(t, addDone))
	require.False(t, persistCalled.Load())
	require.NoError(t, awaitMCPError(t, closeDone))

	_, exists := store.MCPConfig(name)
	require.False(t, exists)
	_, diskErr := os.ReadFile(config.GlobalConfigData())
	require.ErrorIs(t, diskErr, os.ErrNotExist)
	_, exists = sessions.Get(name)
	require.False(t, exists)
	_, exists = states.Get(name)
	require.False(t, exists)
	_, exists = allTools.Get(name)
	require.False(t, exists)
	_, exists = allPrompts.Get(name)
	require.False(t, exists)
	_, exists = allResources.Get(name)
	require.False(t, exists)
	select {
	case event := <-events:
		if event.Payload.Name == name {
			t.Fatalf("canceled Add published an event: %#v", event)
		}
	default:
	}

	next, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, next.Close(context.Background())) }()
	require.NoError(t, next.rememberConfig(store))
	next.Initialize(context.Background(), nil, store, false)
	require.False(t, hasSession(name))
}

func TestInitializeRejectsChangedConfigBeforePublication(t *testing.T) {
	const name = "initialize-stale-config"
	server := mcp.NewServer(&mcp.Implementation{Name: "stale-init"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "stale-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				startOnce.Do(func() { close(started) })
				<-release
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

	initDone := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(initDone)
	}()
	awaitMCPSignal(t, started)
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://initialize-b.example", Timeout: 60}
	require.NoError(t, store.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": replacement.URL,
	}))
	close(release)
	awaitMCPSignal(t, initDone)
	require.False(t, hasSession(name))
	current, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, replacement, current)
}

func TestAddRejectsChangedConfigBeforePublication(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const name = "add-stale-config"
	added := config.MCPConfig{Type: config.MCPHttp, URL: "http://add-a.example"}
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://add-b.example"}
	err = addServerWithPreparationAndPersistence(
		context.Background(), store, name, added,
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: &ClientSession{}}, nil
		},
		func(cfg *config.ConfigStore, scope config.Scope, serverName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistMCPConfigResult(scope, serverName, value)
			if persistErr != nil {
				return result, persistErr
			}
			return result, contender.PersistMCPFieldsExact(config.ScopeGlobal, serverName, map[string]any{
				"url": replacement.URL,
			})
		},
	)
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	require.False(t, hasSession(name))
	current, ok := contender.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, replacement, current)
}

func TestReplaceRejectsSourceEditDuringPreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "source-edit-old"
	const newName = "source-edit-new"
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://source-a.example"}
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://replacement.example"}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	prepared := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, replacement,
			func(cfg *config.ConfigStore, scope config.Scope, old, new string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistReplaceMCPResult(scope, old, new, value)
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				close(prepared)
				<-release
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()
	awaitMCPSignal(t, prepared)
	require.NoError(t, store.PersistMCPFieldsExact(config.ScopeGlobal, oldName, map[string]any{
		"url": "http://source-b.example",
	}))
	close(release)
	require.ErrorIs(t, awaitMCPError(t, done), ErrOwnerBusy)
	_, exists := store.MCPConfig(newName)
	require.False(t, exists)
}

func TestReplaceAllowsNoOpReloadDuringPreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "resolver-reload-old"
	const newName = "resolver-reload-new"
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://resolver-source.example"}
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://resolver-replacement.example"}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	prepared := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, replacement,
			func(cfg *config.ConfigStore, scope config.Scope, old, new string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistReplaceMCPResult(scope, old, new, value)
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				close(prepared)
				<-release
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()
	awaitMCPSignal(t, prepared)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(release)
	require.NoError(t, awaitMCPError(t, done))
	current, exists := store.MCPConfig(newName)
	require.True(t, exists)
	require.Equal(t, replacement, current)
}

func TestReplaceAllowsUnrelatedCOWDuringPreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "unrelated-cow-replace-old"
	const newName = "unrelated-cow-replace-new"
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://unrelated-cow-source.example"}
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://unrelated-cow-replacement.example"}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	prepared := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, replacement,
			func(cfg *config.ConfigStore, scope config.Scope, old, new string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistReplaceMCPResult(scope, old, new, value)
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				close(prepared)
				<-release
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()
	awaitMCPSignal(t, prepared)
	store.SetSkipPermissionRequests(true)
	close(release)
	require.NoError(t, awaitMCPError(t, done))
	current, exists := store.MCPConfig(newName)
	require.True(t, exists)
	require.Equal(t, replacement, current)
}

func TestReplaceRejectsSemanticResolverChangeDuringPreparation(t *testing.T) {
	t.Setenv("RUSH_MCP_REPLACE_RESOLVER_TEST", "old")
	store := isolatedMCPStore(t)
	const oldName = "semantic-resolver-replace-old"
	const newName = "semantic-resolver-replace-new"
	oldConfig := config.MCPConfig{Type: config.MCPHttp, URL: "http://semantic-resolver-source.example"}
	replacement := config.MCPConfig{Type: config.MCPHttp, URL: "http://semantic-resolver-replacement.example"}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	prepared := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, replacement,
			func(cfg *config.ConfigStore, scope config.Scope, old, new string, value config.MCPConfig) (config.MCPMutationResult, error) {
				return cfg.PersistReplaceMCPResult(scope, old, new, value)
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				close(prepared)
				<-release
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()
	awaitMCPSignal(t, prepared)
	t.Setenv("RUSH_MCP_REPLACE_RESOLVER_TEST", "new")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(release)
	require.ErrorIs(t, awaitMCPError(t, done), ErrOwnerBusy)
	_, exists := store.MCPConfig(newName)
	require.False(t, exists)
}

func TestReplaceRejectsPendingAddDestinationBeforePreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "replace-pending-old"
	const newName = "replace-pending-new"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
		Type: config.MCPStdio, Command: oldName,
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	addStarted := make(chan struct{})
	releaseAdd := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(), store, newName,
			config.MCPConfig{Type: config.MCPStdio, Command: newName},
			func(_ context.Context, cfg *config.ConfigStore, serverName string, _ config.MCPConfig, _ config.VariableResolver, admission *serverAdmission) error {
				defer admission.done()
				if err := publishPreparedClient(cfg, serverName, &preparedClient{session: &ClientSession{}}, admission); err != nil {
					return err
				}
				close(addStarted)
				<-releaseAdd
				return nil
			},
		)
	}()
	awaitMCPSignal(t, addStarted)

	var persistCalled atomic.Bool
	replaceErr := replaceServerWithResultPersistenceAndPreparation(
		context.Background(), store, oldName, newName,
		config.MCPConfig{Type: config.MCPStdio, Command: "replacement"},
		func(cfg *config.ConfigStore, scope config.Scope, old, new string, value config.MCPConfig) (config.MCPMutationResult, error) {
			persistCalled.Store(true)
			return cfg.PersistReplaceMCPResult(scope, old, new, value)
		},
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: &ClientSession{}}, nil
		},
	)
	require.ErrorIs(t, replaceErr, config.ErrMCPTargetExists)
	require.False(t, persistCalled.Load())
	close(releaseAdd)
	require.NoError(t, awaitMCPError(t, addDone))
	current, ok := store.MCPConfig(newName)
	require.True(t, ok)
	require.Equal(t, config.MCPConfig{Type: config.MCPStdio, Command: newName}, current)
}

func TestReplaceFallbackSelectionIgnoresAbsentOrDisabledReplacement(t *testing.T) {
	fallback := config.MCPConfig{Type: config.MCPHttp, URL: "http://fallback.example"}
	for _, replacement := range []config.MCPConfig{
		{},
		{Type: config.MCPHttp, URL: "http://replacement.example", Disabled: true},
	} {
		name, selected, exists := fallbackDefinition(config.MCPMutationResult{
			Operation:      "replace",
			OldName:        "old",
			NewName:        "new",
			NewExists:      replacement.URL != "",
			NewConfig:      replacement,
			FallbackExists: true,
			FallbackConfig: fallback,
		})
		require.Equal(t, "old", name)
		require.True(t, exists)
		require.Equal(t, fallback, selected)
	}
}

func TestMCPAdmissionRejectsSecondStoreChangeBeforePin(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const name = "admission-cross-store-stale"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://admission-a.example",
	}))
	snapshot := store.SnapshotMCPAdmission(name)
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": "http://admission-b.example",
	}))
	require.ErrorIs(t, store.WithCurrentMCPAdmission(snapshot, name, func(config.MCPAdmissionGuard) error { return nil }), config.ErrMCPMutationStale)
}

func TestMCPAdmissionHoldsSidecarsThroughSkippedTransition(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const name = "admission-cross-store-block"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://admission-block.example", Disabled: true,
	}))
	snapshot := store.SnapshotMCPAdmission(name)
	entered := make(chan struct{})
	release := make(chan struct{})
	publicationDone := make(chan error, 1)
	go func() {
		publicationDone <- store.WithCurrentMCPAdmission(snapshot, name, func(config.MCPAdmissionGuard) error {
			close(entered)
			<-release
			return nil
		})
	}()
	awaitMCPSignal(t, entered)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"url": "http://admission-blocked-writer.example",
		})
	}()
	select {
	case err := <-writerDone:
		t.Fatalf("second store crossed the admission sidecar lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-publicationDone)
	require.NoError(t, <-writerDone)
}

func TestMCPAdmissionFinalTurnRevalidatesAfterLifecycleContention(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "project"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			runMCPAdmissionFinalTurnContention(t, external)
		})
	}
}

func runMCPAdmissionFinalTurnContention(t *testing.T, external bool) {
	t.Helper()
	store := isolatedMCPStore(t)
	const name = "admission-final-turn-contention"
	path := filepath.Join(store.WorkingDir(), "rush.json")
	key := "mcp"
	if external {
		path = filepath.Join(store.WorkingDir(), ".mcp.json")
		key = "mcpServers"
	}
	write := func(url string) {
		data, err := json.Marshal(map[string]any{key: map[string]any{name: map[string]any{
			"type": "http", "url": url,
		}}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	write("http://initial.example")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	snapshot := store.SnapshotMCPAdmission(name)

	var validations atomic.Int32
	holderAcquired := make(chan struct{})
	releaseHolder := make(chan struct{})
	var firstValidation sync.Once
	mcpInitTestHooks.Lock()
	previousBefore := mcpInitTestHooks.beforeAdmissionValidate
	previousAfter := mcpInitTestHooks.afterAdmissionValidate
	previousFailure := mcpInitTestHooks.onAdmissionTryLockFail
	mcpInitTestHooks.beforeAdmissionValidate = func(hookName string) {
		if hookName == name {
			validations.Add(1)
		}
	}
	mcpInitTestHooks.afterAdmissionValidate = func(hookName string) {
		if hookName != name {
			return
		}
		if validations.Load() != 1 {
			return
		}
		firstValidation.Do(func() {
			go func() {
				lifecycleMu.Lock()
				close(holderAcquired)
				<-releaseHolder
				lifecycleMu.Unlock()
			}()
			<-holderAcquired
			write("http://changed-while-lifecycle-held.example")
		})
	}
	mcpInitTestHooks.onAdmissionTryLockFail = func(hookName string) {
		if hookName == name {
			close(releaseHolder)
		}
	}
	mcpInitTestHooks.Unlock()
	defer func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeAdmissionValidate = previousBefore
		mcpInitTestHooks.afterAdmissionValidate = previousAfter
		mcpInitTestHooks.onAdmissionTryLockFail = previousFailure
		mcpInitTestHooks.Unlock()
	}()

	published := false
	err := store.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
		return withMCPAdmissionFinalTurn(name, guard, func() error {
			published = true
			return nil
		})
	})
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	require.GreaterOrEqual(t, validations.Load(), int32(2))
	require.False(t, published)
	_, ok := GetState(name)
	require.False(t, ok)
}

func TestInitializeSingleDisabledDetachesPublishedRuntime(t *testing.T) {
	const name = "disabled-detaches-runtime"
	store := isolatedMCPStore(t)
	store.Config().MCP = config.MCPs{
		name: {Type: config.MCPHttp, URL: "http://disabled.example", Disabled: true},
	}
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	require.NoError(t, owner.rememberConfig(store))
	session := &ClientSession{}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{Tools: 1, Prompts: 1})
	allTools.Set(name, []*Tool{{Name: "stale-tool"}})
	allPrompts.Set(name, []*Prompt{{Name: "stale-prompt"}})
	allResources.Set(name, []*Resource{{Name: "stale-resource"}})

	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	require.NoError(t, InitializeSingle(context.Background(), name, store))
	require.False(t, hasSession(name))
	require.Empty(t, GetServerToolNames(name))
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, name, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("disabled transition published duplicate event: %v", event)
	default:
	}
}

func TestInitializeCLISkipDetachesPublishedRuntime(t *testing.T) {
	const name = "cli-skip-detaches-runtime"
	store := isolatedMCPStore(t)
	store.Config().MCP = config.MCPs{
		name: {Type: config.MCPHttp, URL: "http://cli-skip.example", EnabledInCLI: false},
	}
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	session := &ClientSession{}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{Tools: 1})
	allTools.Set(name, []*Tool{{Name: "stale-cli-tool"}})

	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	owner.Initialize(context.Background(), nil, store, true)
	require.False(t, hasSession(name))
	require.Empty(t, GetServerToolNames(name))
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, name, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("CLI skip published duplicate event: %v", event)
	default:
	}
}

func TestStaleAdmissionCleanupLeavesNoOwnerlessStarting(t *testing.T) {
	const name = "stale-admission-cleanup"
	store := isolatedMCPStore(t)
	result, err := store.PersistMCPConfigResult(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://stale-admission.example",
	})
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	require.NoError(t, owner.rememberConfig(store))
	admission, err := owner.admitServerForConfig(context.Background(), store, name, result.NewConfig, true)
	require.NoError(t, err)
	defer admission.done()
	admission.mutationResult = &result
	updateAdmissionState(&admission, StateStarting, nil, nil, Counts{})
	eventsCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := SubscribeEvents(eventsCtx)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.debug", true))
	session := &ClientSession{}
	err = publishPreparedClientWithMutation(store, name, &preparedClient{session: session}, &admission)
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	cleanupFailedAdmission(&admission, err)
	state, ok := GetState(name)
	require.True(t, ok)
	require.NotEqual(t, StateStarting, state.State)
	require.Equal(t, StateError, state.State)
	require.False(t, hasSession(name))
	requireTransactionalEvent(t, events, pubsub.UpdatedEvent, name, StateError)
	select {
	case event := <-events:
		t.Fatalf("stale cleanup published duplicate event: %v", event)
	default:
	}
}

func TestSkippedTransitionReleasesLeaseBeforeBlockingRetirement(t *testing.T) {
	const name = "skipped-blocking-retirement"
	store := isolatedMCPStore(t)
	store.Config().MCP = config.MCPs{
		name: {Type: config.MCPHttp, URL: "http://skipped-blocking.example", Disabled: true},
	}
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	terminalEntered := make(chan struct{})
	releaseTerminal := make(chan struct{})
	session := &ClientSession{terminal: func() {
		close(terminalEntered)
		<-releaseTerminal
	}}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{})

	initDone := make(chan error, 1)
	go func() { initDone <- InitializeSingle(context.Background(), name, store) }()
	awaitMCPSignal(t, terminalEntered)

	mutationDone := make(chan error, 1)
	go func() { mutationDone <- DisableSingle(store, name) }()
	select {
	case err := <-mutationDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("same-name mutation could not acquire lease while retirement was blocked")
	}
	close(releaseTerminal)
	require.NoError(t, <-initDone)
	require.False(t, hasSession(name))
}
