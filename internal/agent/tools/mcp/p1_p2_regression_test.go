package mcp

import (
	"context"
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
