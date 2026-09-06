package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func injectedMCPCommitOutcome(reconciled bool) error {
	return &config.CommitOutcome{
		Committed:  true,
		Reconciled: reconciled,
		Path:       "injected-mcp-config",
		Cause:      errors.New("injected commit uncertainty"),
	}
}

func injectedMCPPrecommitOutcome() error {
	return &config.CommitOutcome{
		Committed:  false,
		Reconciled: false,
		Path:       "injected-mcp-config",
		Cause:      errors.New("injected pre-commit failure"),
	}
}

func requireMCPCommitUncertainty(t *testing.T, err error, reconciled bool) {
	t.Helper()
	var outcome *config.CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.Equal(t, reconciled, outcome.Reconciled)
}

func requireMCPPrecommitOutcome(t *testing.T, err error) {
	t.Helper()
	var outcome *config.CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	require.False(t, outcome.Reconciled)
}

func closeCommitOutcomeTestOwner(t *testing.T, owner *Owner) {
	t.Helper()
	for _, session := range sessions.Seq2() {
		_ = session.Close()
	}
	require.NoError(t, owner.Close(context.Background()))
}

func fakeMCPInitializer(
	_ context.Context,
	cfg *config.ConfigStore,
	name string,
	_ config.MCPConfig,
	_ config.VariableResolver,
	admission *serverAdmission,
) error {
	defer admission.done()
	return publishPreparedClient(cfg, name, &preparedClient{session: &ClientSession{}}, admission)
}

func countedAddInitializer(owner *Owner, closeCalls *atomic.Int32, cleanupBeforeReset *atomic.Bool) admittedClientInitializer {
	return func(
		_ context.Context,
		cfg *config.ConfigStore,
		name string,
		_ config.MCPConfig,
		_ config.VariableResolver,
		admission *serverAdmission,
	) error {
		candidate := &ClientSession{cancel: func() {
			closeCalls.Add(1)
			if currentOwner() == owner {
				cleanupBeforeReset.Store(true)
			}
		}}
		defer admission.done()
		return publishPreparedClient(cfg, name, &preparedClient{session: candidate}, admission)
	}
}

func awaitMCPError(t *testing.T, results <-chan error) error {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-results:
		return err
	case <-timer.C:
		t.Fatal("MCP operation did not complete")
		return nil
	}
}

func awaitMCPSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal("MCP test barrier was not reached")
	}
}

func TestAddServerReconciledCommitOutcomePublishesRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	name := "add-outcome"
	mcpConfig := config.MCPConfig{Type: config.MCPStdio, Command: "add-outcome"}
	err = addServerWithInitializerAndPersistence(context.Background(), store, name, mcpConfig, fakeMCPInitializer,
		func(cfg *config.ConfigStore, scope config.Scope, name string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistMCPConfigResult(scope, name, value)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(true)
		})

	requireMCPCommitUncertainty(t, err, true)
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.Equal(t, mcpConfig, configured)
	require.Equal(t, StateConnected, mustState(t, name).State)
	require.True(t, hasSession(name))
}

func TestEnableServerReconciledCommitOutcomeStartsRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	initial := config.MCPConfig{Type: config.MCPStdio, Command: "enable-outcome", Disabled: true}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, "enable-outcome", initial))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = enableServerWithPersistenceAndInitializer(context.Background(), store, "enable-outcome",
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := enableMCPConfig(cfg, scope, name, pending)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(true)
		}, fakeMCPInitializer)

	requireMCPCommitUncertainty(t, err, true)
	configured, ok := store.MCPConfig("enable-outcome")
	require.True(t, ok)
	require.False(t, configured.Disabled)
	require.Eventually(t, func() bool {
		return hasSession("enable-outcome") && mustState(t, "enable-outcome").State == StateConnected
	}, 5*time.Second, time.Millisecond)
}

func TestDisableServerReconciledCommitOutcomeAppliesRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, "disable-outcome", config.MCPConfig{Type: config.MCPStdio, Command: "disable-outcome"}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	session := &ClientSession{}
	sessions.Set("disable-outcome", session)
	setState("disable-outcome", StateConnected, nil, session, Counts{})

	err = disableServerWithResultPersistence(context.Background(), store, "disable-outcome",
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistMCPDisabledOverrideResult(scope, name, true)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(true)
		})

	requireMCPCommitUncertainty(t, err, true)
	configured, ok := store.MCPConfig("disable-outcome")
	require.True(t, ok)
	require.True(t, configured.Disabled)
	require.False(t, hasSession("disable-outcome"))
	require.Equal(t, StateDisabled, mustState(t, "disable-outcome").State)
}

func TestRemoveServerReconciledCommitOutcomeAppliesRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, "remove-outcome", config.MCPConfig{Type: config.MCPStdio, Command: "remove-outcome"}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	session := &ClientSession{}
	sessions.Set("remove-outcome", session)
	setState("remove-outcome", StateConnected, nil, session, Counts{})

	err = removeServerWithResultPersistence(store, "remove-outcome",
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistRemoveMCPConfigResult(scope, name)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(true)
		})

	requireMCPCommitUncertainty(t, err, true)
	_, ok := store.MCPConfig("remove-outcome")
	require.False(t, ok)
	require.False(t, hasSession("remove-outcome"))
	_, ok = GetState("remove-outcome")
	require.False(t, ok)
}

func TestReplaceServerReconciledCommitOutcomePublishesCandidate(t *testing.T) {
	store := isolatedMCPStore(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, "replace-outcome", config.MCPConfig{Type: config.MCPStdio, Command: "replace-old"}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	oldSession := &ClientSession{}
	sessions.Set("replace-outcome", oldSession)
	setState("replace-outcome", StateConnected, nil, oldSession, Counts{})

	newConfig := config.MCPConfig{Type: config.MCPStdio, Command: "replace-new"}
	err = replaceServerWithResultPersistenceAndPreparation(context.Background(), store, "replace-outcome", "replace-outcome", newConfig,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(true)
		}, func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: &ClientSession{}}, nil
		})

	requireMCPCommitUncertainty(t, err, true)
	configured, ok := store.MCPConfig("replace-outcome")
	require.True(t, ok)
	require.Equal(t, newConfig, configured)
	require.Eventually(t, func() bool {
		return hasSession("replace-outcome") && mustState(t, "replace-outcome").State == StateConnected
	}, 5*time.Second, time.Millisecond)
	require.Empty(t, GetServerToolNames("replace-outcome"))
}

func TestCommittedUnreconciledOutcomesFenceRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, "unreconciled", config.MCPConfig{Type: config.MCPStdio, Command: "unreconciled"}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = disableServerWithResultPersistence(context.Background(), store, "unreconciled",
		func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, injectedMCPCommitOutcome(false)
		})
	requireMCPCommitUncertainty(t, err, false)
	require.False(t, hasSession("unreconciled"))
	require.Equal(t, StateDisabled, mustState(t, "unreconciled").State)
}

func TestTypedPrecommitOutcomeDoesNotCommitAdd(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	name := "precommit-add"
	err = addServerWithInitializerAndPersistence(context.Background(), store, name,
		config.MCPConfig{Type: config.MCPStdio, Command: name}, fakeMCPInitializer,
		func(*config.ConfigStore, config.Scope, string, config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, injectedMCPPrecommitOutcome()
		})

	requireMCPPrecommitOutcome(t, err)
	_, ok := store.MCPConfig(name)
	require.False(t, ok)
	require.False(t, hasSession(name))
	_, ok = GetState(name)
	require.False(t, ok)
}

func TestAddDurableCommitFencedByCloseReturnsSuccessWithoutPublishingCandidate(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	releasePersistence := make(chan struct{})
	var releasePersistenceOnce sync.Once
	defer func() {
		releasePersistenceOnce.Do(func() { close(releasePersistence) })
		require.NoError(t, owner.Close(context.Background()))
	}()

	const name = "add-close-after-commit"
	var candidateCloseCalls atomic.Int32
	var cleanupBeforeReset atomic.Bool
	initialize := countedAddInitializer(owner, &candidateCloseCalls, &cleanupBeforeReset)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	committed := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializerAndPersistence(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPStdio, Command: name},
			initialize,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := cfg.PersistMCPConfigResult(scope, serverName, value)
				close(committed)
				<-releasePersistence
				return result, persistErr
			},
		)
	}()
	awaitMCPSignal(t, committed)

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	releasePersistenceOnce.Do(func() { close(releasePersistence) })

	require.NoError(t, awaitMCPError(t, addDone))
	require.NoError(t, awaitMCPError(t, closeDone))
	_, persisted := diskMCP(t)[name]
	require.True(t, persisted)
	require.Equal(t, int32(1), candidateCloseCalls.Load(), "the rejected Add candidate must be closed exactly once")
	require.True(t, cleanupBeforeReset.Load(), "candidate cleanup must finish before registry reset")
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.Payload.Name == name && (event.Payload.State == StateConnected ||
				event.Payload.Type == EventToolsListChanged || event.Payload.Type == EventPromptsListChanged ||
				event.Payload.Type == EventResourcesListChanged) {
				t.Fatalf("Close winner published an Add candidate event: %v", event)
			}
		default:
			return
		}
	}
}

func TestAddReconciledCommitOutcomeFencedByCloseReturnsOriginalOutcome(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	releasePersistence := make(chan struct{})
	var releasePersistenceOnce sync.Once
	defer func() {
		releasePersistenceOnce.Do(func() { close(releasePersistence) })
		require.NoError(t, owner.Close(context.Background()))
	}()

	const name = "add-uncertain-close-after-commit"
	var candidateCloseCalls atomic.Int32
	var cleanupBeforeReset atomic.Bool
	initialize := countedAddInitializer(owner, &candidateCloseCalls, &cleanupBeforeReset)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	committed := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializerAndPersistence(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPStdio, Command: name},
			initialize,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := cfg.PersistMCPConfigResult(scope, serverName, value)
				close(committed)
				<-releasePersistence
				return result, errors.Join(persistErr, injectedMCPCommitOutcome(true))
			},
		)
	}()
	awaitMCPSignal(t, committed)

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	releasePersistenceOnce.Do(func() { close(releasePersistence) })

	requireMCPCommitUncertainty(t, awaitMCPError(t, addDone), true)
	require.NoError(t, awaitMCPError(t, closeDone))
	_, persisted := diskMCP(t)[name]
	require.True(t, persisted)
	require.Equal(t, int32(1), candidateCloseCalls.Load(), "the rejected Add candidate must be closed exactly once")
	require.True(t, cleanupBeforeReset.Load(), "candidate cleanup must finish before registry reset")
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.Payload.Name == name && (event.Payload.State == StateConnected ||
				event.Payload.Type == EventToolsListChanged || event.Payload.Type == EventPromptsListChanged ||
				event.Payload.Type == EventResourcesListChanged) {
				t.Fatalf("Close winner published an Add candidate event: %v", event)
			}
		default:
			return
		}
	}
}

func TestAddLinearizesPublicationBeforeConcurrentRemoveAfterCommit(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	releasePersistence := make(chan struct{})
	releaseAddUnlock := make(chan struct{})
	releaseRemoveLock := make(chan struct{})
	var releasePersistenceOnce sync.Once
	var releaseAddUnlockOnce sync.Once
	var releaseRemoveLockOnce sync.Once
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeLockFn = nil
		serverLeaseHooks.afterLockFn = nil
		serverLeaseHooks.afterUnlockFn = nil
		serverLeaseHooks.Unlock()
		releasePersistenceOnce.Do(func() { close(releasePersistence) })
		releaseAddUnlockOnce.Do(func() { close(releaseAddUnlock) })
		releaseRemoveLockOnce.Do(func() { close(releaseRemoveLock) })
		require.NoError(t, owner.Close(context.Background()))
	}()

	const name = "add-remove-after-commit"
	committed := make(chan struct{})
	addUnlockReached := make(chan struct{})
	addDone := make(chan error, 1)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	go func() {
		addDone <- addServerWithInitializerAndPersistence(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPStdio, Command: name},
			fakeMCPInitializer,
			func(cfg *config.ConfigStore, scope config.Scope, serverName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := cfg.PersistMCPConfigResult(scope, serverName, value)
				close(committed)
				<-releasePersistence
				return result, persistErr
			},
		)
	}()
	awaitMCPSignal(t, committed)

	leases.mu.Lock()
	addLease := leases.entries[name]
	leases.mu.Unlock()
	require.NotNil(t, addLease)
	removeLockAttempted := make(chan *serverLease, 1)
	removeLockAcquired := make(chan *serverLease, 1)
	var removeAttemptOnce sync.Once
	var removeAcquiredOnce sync.Once
	var addUnlockOnce sync.Once
	serverLeaseHooks.Lock()
	serverLeaseHooks.beforeLockFn = func(lease *serverLease) {
		if lease == addLease {
			removeAttemptOnce.Do(func() { removeLockAttempted <- lease })
		}
	}
	serverLeaseHooks.afterLockFn = func(lease *serverLease) {
		if lease == addLease {
			removeAcquiredOnce.Do(func() {
				removeLockAcquired <- lease
				<-releaseRemoveLock
			})
		}
	}
	serverLeaseHooks.afterUnlockFn = func(lease *serverLease) {
		if lease == addLease {
			addUnlockOnce.Do(func() {
				close(addUnlockReached)
				<-releaseAddUnlock
			})
		}
	}
	serverLeaseHooks.Unlock()

	removeDone := make(chan error, 1)
	go func() { removeDone <- RemoveServer(store, name) }()
	var attemptedLease *serverLease
	select {
	case attemptedLease = <-removeLockAttempted:
	// This timeout is only an outer safety net; the ordering assertion uses hooks.
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveServer did not begin lock acquisition")
	}
	require.Same(t, addLease, attemptedLease)
	releasePersistenceOnce.Do(func() { close(releasePersistence) })
	awaitMCPSignal(t, addUnlockReached)

	var unlockedLease *serverLease
	select {
	case unlockedLease = <-removeLockAcquired:
	// This timeout is only an outer safety net; the ordering assertion uses hooks.
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveServer did not acquire the exact server lease")
	}
	require.Same(t, addLease, unlockedLease)
	var publication Event
	select {
	case event := <-events:
		publication = event.Payload
	default:
		t.Fatal("RemoveServer acquired the lease before Add publication")
	}
	require.Equal(t, EventStateChanged, publication.Type)
	require.Equal(t, name, publication.Name)
	require.Equal(t, StateConnected, publication.State)
	releaseRemoveLockOnce.Do(func() { close(releaseRemoveLock) })
	releaseAddUnlockOnce.Do(func() { close(releaseAddUnlock) })

	require.NoError(t, awaitMCPError(t, addDone))
	require.NoError(t, awaitMCPError(t, removeDone))
	_, persisted := diskMCP(t)[name]
	require.False(t, persisted)
	require.False(t, hasSession(name))
	_, hasState := GetState(name)
	require.False(t, hasState)
	requireTransactionalEvent(t, events, pubsub.DeletedEvent, name, StateDisabled)
	select {
	case event := <-events:
		t.Fatalf("Add/Remove published an extra event: %v", event)
	default:
	}
}

func TestTypedPrecommitOutcomeDoesNotDisableRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	name := "precommit-disable"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPStdio, Command: name,
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	session := &ClientSession{}
	sessions.Set(name, session)
	setState(name, StateConnected, nil, session, Counts{})

	err = disableServerWithResultPersistence(context.Background(), store, name,
		func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, injectedMCPPrecommitOutcome()
		})

	requireMCPPrecommitOutcome(t, err)
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.False(t, configured.Disabled)
	require.Same(t, session, mustSession(t, name))
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestTypedPrecommitOutcomeDoesNotReplaceRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	oldName := "precommit-replace"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, config.MCPConfig{
		Type: config.MCPStdio, Command: "old",
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	oldSession := &ClientSession{}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})

	err = replaceServerWithResultPersistenceAndPreparation(context.Background(), store, oldName, oldName,
		config.MCPConfig{Type: config.MCPStdio, Command: "new"},
		func(*config.ConfigStore, config.Scope, string, string, config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, injectedMCPPrecommitOutcome()
		}, func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
			return &preparedClient{session: &ClientSession{}}, nil
		})

	requireMCPPrecommitOutcome(t, err)
	configured, ok := store.MCPConfig(oldName)
	require.True(t, ok)
	require.Equal(t, "old", configured.Command)
	require.Same(t, oldSession, mustSession(t, oldName))
	require.Equal(t, StateConnected, mustState(t, oldName).State)
}
