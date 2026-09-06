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
	lifecycleMu.Lock()
	owner.nextUncertainty++
	oldVersion := owner.nextUncertainty
	owner.uncertainServers[name] = oldVersion
	owner.nextUncertainty++
	owner.uncertainServers[clearedName] = owner.nextUncertainty
	lifecycleMu.Unlock()

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
	lifecycleMu.Lock()
	currentVersion, stillFenced := owner.uncertainServers[name]
	_, cleared := owner.uncertainServers[clearedName]
	lifecycleMu.Unlock()
	require.True(t, stillFenced)
	require.NotEqual(t, oldVersion, currentVersion)
	require.False(t, cleared)
}
