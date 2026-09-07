package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestReplaceRejectsDestinationFenceRaisedDuringPreparation(t *testing.T) {
	store := isolatedMCPStore(t)
	const oldName = "replace-fence-old"
	const newName = "replace-fence-new"
	oldConfig := config.MCPConfig{Type: config.MCPStdio, Command: oldName}
	replacementConfig := config.MCPConfig{Type: config.MCPStdio, Command: newName}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, oldName, oldConfig))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	oldSession := &ClientSession{}
	sessions.Set(oldName, oldSession)
	setState(oldName, StateConnected, nil, oldSession, Counts{})
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)

	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	var prepareOnce sync.Once
	var persistCalled atomic.Bool
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- replaceServerWithResultPersistenceAndPreparation(
			context.Background(), store, oldName, newName, replacementConfig,
			func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, value config.MCPConfig) (config.MCPMutationResult, error) {
				persistCalled.Store(true)
				return cfg.PersistReplaceMCPResult(scope, oldName, newName, value)
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
				prepareOnce.Do(func() { close(prepareStarted) })
				<-releasePrepare
				return &preparedClient{session: &ClientSession{}}, nil
			},
		)
	}()
	awaitMCPSignal(t, prepareStarted)

	addErr := addServerWithInitializerAndPersistence(
		context.Background(), store, newName, replacementConfig, fakeMCPInitializer,
		func(cfg *config.ConfigStore, scope config.Scope, name string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, err := cfg.PersistMCPConfigResult(scope, name, value)
			if err != nil {
				return result, err
			}
			return result, injectedMCPMaybeCommitted()
		},
	)
	requireMCPMaybeCommitted(t, addErr)
	requireMCPUncertain(t, owner, newName)

	close(releasePrepare)
	replaceErr := awaitMCPError(t, replaceDone)
	require.ErrorIs(t, replaceErr, ErrOwnerBusy)
	require.False(t, persistCalled.Load(), "replacement must not persist after the destination fence")
	require.Same(t, oldSession, mustSession(t, oldName))
	require.False(t, hasSession(newName))
	for {
		select {
		case event := <-events:
			if event.Payload.State == StateConnected || event.Payload.Type != EventStateChanged {
				t.Fatalf("fenced replacement published a runtime event: %v", event)
			}
		default:
			return
		}
	}
}

func TestEnableRollbackUncertaintySurvivesOwnerRolloverUntilExactReload(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "enable-rollback-fence"
	initial := config.MCPConfig{Type: config.MCPStdio, Command: name, Disabled: true}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, initial))
	owner, err := Acquire()
	require.NoError(t, err)

	enabled := make(chan struct{})
	releaseEnable := make(chan struct{})
	var calls atomic.Int32
	enableDone := make(chan error, 1)
	go func() {
		enableDone <- enableServerWithPersistenceAndInitializerAndRollback(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
				calls.Add(1)
				result, err := enableMCPConfig(cfg, scope, name, pending)
				close(enabled)
				<-releaseEnable
				return result, err
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) error {
				return errors.New("initializer must not run after shutdown wins")
			},
			func(cfg *config.ConfigStore, scope config.Scope, name string, _ *config.MCPConfig) (config.MCPMutationResult, error) {
				result, err := cfg.PersistMCPDisabledOverrideResult(scope, name, true)
				if err != nil {
					return result, err
				}
				return result, injectedMCPMaybeCommitted()
			},
		)
	}()
	awaitMCPSignal(t, enabled)

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	close(releaseEnable)

	enableErr := awaitMCPError(t, enableDone)
	require.ErrorIs(t, enableErr, ErrOwnerBusy)
	require.ErrorIs(t, enableErr, ErrMCPConfigUncertain)
	requireMCPMaybeCommitted(t, enableErr)
	requireMCPUncertain(t, owner, name)
	require.NoError(t, awaitMCPError(t, closeDone))

	next, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, next.Close(context.Background())) }()
	require.NoError(t, next.rememberConfig(store))
	require.NoError(t, ReloadAndReconcileMCPConfig(context.Background(), store))
	configured, ok := store.MCPConfig(name)
	require.True(t, ok)
	require.True(t, configured.Disabled)
	_, uncertain := store.MCPUncertaintyVersion(name)
	require.False(t, uncertain)
	next.Initialize(context.Background(), nil, store, false)
	require.False(t, hasSession(name))
}

func TestEnablePendingGlobalAddRollbackRestoresDisabledDefinition(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)

	const name = "enable-pending-rollback"
	addStarted := make(chan struct{})
	releaseAdd := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		addDone <- addServerWithInitializer(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPHttp, URL: "http://pending-enable.example"},
			func(
				_ context.Context,
				_ *config.ConfigStore,
				_ string,
				_ config.MCPConfig,
				_ config.VariableResolver,
				admission *serverAdmission,
			) error {
				admission.done()
				close(addStarted)
				<-releaseAdd
				return nil
			},
		)
	}()
	awaitMCPSignal(t, addStarted)

	enablePersisted := make(chan struct{})
	releaseEnable := make(chan struct{})
	enableDone := make(chan error, 1)
	go func() {
		enableDone <- enableServerWithPersistenceAndInitializer(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := enableMCPConfig(cfg, scope, name, pending)
				close(enablePersisted)
				<-releaseEnable
				return result, persistErr
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) error {
				return errors.New("initializer must not run after shutdown wins")
			},
		)
	}()
	awaitMCPSignal(t, enablePersisted)

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	close(releaseEnable)
	require.ErrorIs(t, <-enableDone, ErrOwnerBusy)
	close(releaseAdd)
	require.ErrorIs(t, <-addDone, ErrOwnerBusy)
	require.NoError(t, <-closeDone)

	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)[name], &persisted))
	require.Equal(t, "http://pending-enable.example", persisted.URL)
	require.True(t, persisted.Disabled)
}

func TestEnableRollbackPreservesConcurrentDefinitionAndFencesStore(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "enable-concurrent-rollback"
	initial := config.MCPConfig{Type: config.MCPHttp, URL: "http://old-enable.example", Disabled: true}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, initial))
	contender, err := config.Init(store.WorkingDir(), store.Config().Options.DataDirectory, false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)

	enablePersisted := make(chan struct{})
	releaseEnable := make(chan struct{})
	enableDone := make(chan error, 1)
	go func() {
		enableDone <- enableServerWithPersistenceAndInitializer(
			context.Background(), store, name,
			func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
				result, persistErr := enableMCPConfig(cfg, scope, name, pending)
				close(enablePersisted)
				<-releaseEnable
				return result, persistErr
			},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) error {
				return errors.New("initializer must not run after shutdown wins")
			},
		)
	}()
	awaitMCPSignal(t, enablePersisted)
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": "http://new-enable.example",
	}))

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	require.Eventually(t, func() bool {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return owner.closing
	}, time.Second, time.Millisecond)
	close(releaseEnable)
	enableErr := <-enableDone
	require.ErrorIs(t, enableErr, ErrOwnerBusy)
	require.ErrorIs(t, enableErr, ErrMCPConfigUncertain)
	require.NoError(t, <-closeDone)

	var persisted config.MCPConfig
	require.NoError(t, json.Unmarshal(diskMCP(t)[name], &persisted))
	require.Equal(t, "http://new-enable.example", persisted.URL)
	require.False(t, persisted.Disabled)
	_, uncertain := store.MCPUncertaintyVersion(name)
	require.True(t, uncertain)
}

func TestMCPAdmissionFinalTurnCancellationReleasesConfigLocks(t *testing.T) {
	store := isolatedMCPStore(t)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	const name = "admission-final-turn-canceled"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://admission-cancel.example",
	}))
	snapshot := store.SnapshotMCPAdmission(name)

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var turnCalls atomic.Int32
	var published atomic.Bool
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.beforeAdmissionTurn
	mcpInitTestHooks.beforeAdmissionTurn = func(hookName string) {
		if hookName == name {
			turnCalls.Add(1)
			close(entered)
		}
	}
	mcpInitTestHooks.Unlock()
	defer func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeAdmissionTurn = previous
		mcpInitTestHooks.Unlock()
	}()

	done := make(chan error, 1)
	go func() {
		done <- store.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
			return withMCPAdmissionFinalTurn(ctx, name, guard, func() error {
				published.Store(true)
				return nil
			})
		})
	}()
	awaitMCPSignal(t, entered)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, int32(1), turnCalls.Load())
	require.False(t, published.Load())

	writerDone := make(chan error, 1)
	go func() {
		writerDone <- contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"url": "http://admission-cancel-writer.example",
		})
	}()
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("config sidecar lock remained held after admission cancellation")
	}
}
