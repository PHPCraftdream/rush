package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
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
