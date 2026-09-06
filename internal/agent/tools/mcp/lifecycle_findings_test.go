package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
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
