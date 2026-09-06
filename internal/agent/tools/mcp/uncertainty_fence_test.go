package mcp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUnreconciledCommitBlocksReadmissionUntilReloadReconciliation(t *testing.T) {
	store := isolatedMCPStore(t)
	name := "uncertain-fence"
	initial := config.MCPConfig{Type: config.MCPStdio, Command: name}
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, initial))
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	err = disableServerWithResultPersistence(context.Background(), store, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistMCPDisabledOverrideResult(scope, name, true)
			require.NoError(t, persistErr)
			return result, injectedMCPCommitOutcome(false)
		})
	requireMCPCommitUncertainty(t, err, false)

	admission, err := owner.admitServer(context.Background(), store, name, false)
	require.ErrorIs(t, err, ErrMCPConfigUncertain)
	require.Empty(t, admission)

	// Make the staleness signal clean without changing the config. The direct
	// lifecycle operation still has to reload before clearing this fence.
	require.NoError(t, store.RefreshStalenessSnapshot())
	require.False(t, store.ConfigStaleness().Dirty)
	require.NoError(t, InitializeSingle(context.Background(), name, store))

	// The successful reload cleared the fence, so a fresh admission is now
	// allowed even though the reconciled config remains disabled.
	admission, err = owner.admitServer(context.Background(), store, name, false)
	require.NoError(t, err)
	admission.done()

	owner.Initialize(context.Background(), nil, store, false)
	require.Equal(t, StateDisabled, mustState(t, name).State)

	// A failed on-demand reload leaves a newly raised fence in place.
	fenceMCPRuntime(owner, name)
	require.NoError(t, os.WriteFile(config.GlobalConfigData(), []byte("{"), 0o600))
	require.Error(t, InitializeSingle(context.Background(), name, store))
	_, err = owner.admitServer(context.Background(), store, name, false)
	require.ErrorIs(t, err, ErrMCPConfigUncertain)
}

func TestAddPrecommitFailureDoesNotPublishPreparedRuntime(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)

	name := "prepared-add"
	prepare := func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error) {
		return &preparedClient{session: &ClientSession{}}, nil
	}
	err = addServerWithPreparationAndPersistence(context.Background(), store, name,
		config.MCPConfig{Type: config.MCPStdio, Command: name}, prepare,
		func(*config.ConfigStore, config.Scope, string, config.MCPConfig) (config.MCPMutationResult, error) {
			return config.MCPMutationResult{}, &config.CommitOutcome{Cause: errors.New("precommit")}
		})
	require.Error(t, err)
	var outcome *config.CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	_, ok := sessions.Get(name)
	require.False(t, ok)
	_, ok = GetState(name)
	require.False(t, ok)
	_, ok = store.MCPConfig(name)
	require.False(t, ok)
}

func TestReloadAndReconcilePreservesFenceRaisedDuringFinalize(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	owner.rememberConfig(store)

	const name = "reload-finalize-race"
	const clearedName = "reload-finalize-cleared"
	oldVersion := store.MarkMCPUncertain(name)
	store.MarkMCPUncertain(clearedName)

	reloadReachedFinalize := make(chan struct{})
	releaseFinalize := make(chan struct{})
	mcpReloadAfterSuccessHook = func() {
		close(reloadReachedFinalize)
		<-releaseFinalize
	}
	defer func() { mcpReloadAfterSuccessHook = nil }()

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- ReloadAndReconcileMCPConfig(context.Background(), store) }()
	waitForRequest(t, reloadReachedFinalize)
	fenceMCPRuntime(owner, name)
	close(releaseFinalize)

	require.ErrorIs(t, <-reloadDone, ErrMCPConfigUncertain)
	currentVersion, stillFenced := store.MCPUncertaintyVersion(name)
	_, cleared := store.MCPUncertaintyVersion(clearedName)
	require.True(t, stillFenced)
	require.NotEqual(t, oldVersion, currentVersion)
	require.False(t, cleared)
}

func TestMaybeCommittedFenceSurvivesOwnerRollover(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, owner.rememberConfig(store))

	const name = "maybe-rollover"
	err = addServerWithInitializerAndPersistence(context.Background(), store, name,
		config.MCPConfig{Type: config.MCPStdio, Command: name}, fakeMCPInitializer,
		func(cfg *config.ConfigStore, scope config.Scope, name string, value config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := cfg.PersistMCPConfigResult(scope, name, value)
			require.NoError(t, persistErr)
			return result, injectedMCPMaybeCommitted()
		})
	requireMCPMaybeCommitted(t, err)
	require.NoError(t, owner.Close(context.Background()))

	next, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, next)
	require.NoError(t, next.rememberConfig(store))

	admission, err := next.admitServer(context.Background(), store, name, false)
	require.ErrorIs(t, err, ErrMCPConfigUncertain)
	require.Empty(t, admission)

	require.NoError(t, ReloadAndReconcileMCPConfig(context.Background(), store))
	admission, err = next.admitServer(context.Background(), store, name, false)
	require.NoError(t, err)
	admission.done()
}

func TestWrongStoreReloadDoesNotClearMCPFence(t *testing.T) {
	store := isolatedMCPStore(t)
	otherStore := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	require.NoError(t, owner.rememberConfig(store))

	const name = "wrong-store-reload"
	store.MarkMCPUncertain(name)
	require.NoError(t, ReloadAndReconcileMCPConfig(context.Background(), otherStore))

	_, err = owner.admitServer(context.Background(), store, name, false)
	require.ErrorIs(t, err, ErrMCPConfigUncertain)
}

func TestOwnerRejectsConfigStoreSwitch(t *testing.T) {
	store := isolatedMCPStore(t)
	otherStore := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer closeCommitOutcomeTestOwner(t, owner)
	require.NoError(t, owner.rememberConfig(store))

	require.ErrorIs(t, InitializeSingle(context.Background(), "switch", otherStore), ErrMCPConfigStoreBusy)
	_, err = owner.admitServer(context.Background(), otherStore, "switch", false)
	require.ErrorIs(t, err, ErrMCPConfigStoreBusy)
	admission, err := owner.admitServer(context.Background(), store, "switch", false)
	require.NoError(t, err)
	admission.done()
}
