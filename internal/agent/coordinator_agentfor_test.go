package agent

// Task #1054: Cancel/InjectMessage must route through agentFor, the same
// choke point wakeSession uses, instead of always hitting c.currentAgent --
// a delegated child session's live generation runs on its own registered
// driver (task #1049), and c.currentAgent has no mailbox record of it.

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent pins task #1054.
// Revert-check performed: reverted Cancel to call c.currentAgent.Cancel(sessionID)
// directly -- this test FAILED (driver.cancelled was empty, rootAgent.cancelled
// held "child-1" instead). Restored the agentFor routing; re-ran, passed.
func TestCoordinatorCancel_RoutesToChildDriverNotCurrentAgent(t *testing.T) {
	rootAgent := &mockSessionAgent{}
	driverAgent := &mockSessionAgent{}
	coord := &coordinator{currentAgent: rootAgent, subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: driverAgent})

	coord.Cancel("child-1")

	require.Empty(t, rootAgent.cancelled, "c.currentAgent must not be touched for a driven child session")
	require.Equal(t, []string{"child-1"}, driverAgent.cancelled)
}

// TestCoordinatorCancel_NoDriverFallsBackToCurrentAgent proves the
// non-delegated (no registered driver) path is byte-for-byte unchanged.
func TestCoordinatorCancel_NoDriverFallsBackToCurrentAgent(t *testing.T) {
	rootAgent := &mockSessionAgent{}
	coord := &coordinator{currentAgent: rootAgent, subAgentDrivers: newSubAgentDriverRegistry()}

	coord.Cancel("root-session")

	require.Equal(t, []string{"root-session"}, rootAgent.cancelled)
}

// TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent pins task
// #1054 for InjectMessage: a persisted message merge for a delegated child
// must land on the driver's mailbox, not silently query an idle
// c.currentAgent for busy state via injectIfBusy on the wrong SessionAgent.
// Revert-check performed: reverted InjectMessage to call
// c.currentAgent.InjectMessage(ctx, call) directly -- this test FAILED
// (driverAgent.queuedCalls was empty, rootAgent.queuedCalls held the call
// instead). Restored the agentFor routing; re-ran, passed.
func TestCoordinatorInjectMessage_RoutesToChildDriverNotCurrentAgent(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: providerID, Model: "test-model"}

	rootAgent := &mockSessionAgent{}
	driverAgent := &mockSessionAgent{}
	coord := &coordinator{
		cfg:             cfg,
		sessions:        env.sessions,
		messages:        env.messages,
		currentAgent:    rootAgent,
		modelCache:      csync.NewMap[string, cachedModelPair](),
		subAgentDrivers: newSubAgentDriverRegistry(),
	}

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "tool-call-1", parent.ID, "child")
	require.NoError(t, err)
	coord.subAgentDrivers.register(child.ID, subAgentDriver{agent: driverAgent})

	_, err = coord.InjectMessage(context.Background(), child.ID, "notice text")
	require.NoError(t, err)

	require.Empty(t, rootAgent.queuedCalls, "c.currentAgent must not receive the driven child's inject")
	require.Len(t, driverAgent.queuedCalls, 1)
	require.Equal(t, "notice text", driverAgent.queuedCalls[0].Prompt)
}
