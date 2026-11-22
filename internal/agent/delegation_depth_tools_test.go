package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDurableDepthMarkerStripsDelegationWithoutCallOptions(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "legacy exhausted depth marker")
	require.NoError(t, err)
	_, err = PersistSessionAgentDepth(t.Context(), env.sessions, sess.ID, 0)
	require.NoError(t, err)
	ctx, err := WithPersistedSessionAgentDepth(t.Context(), env.sessions, sess.ID)
	require.NoError(t, err)
	require.Nil(t, callOptionsFrom(ctx))
	original := config.Agent{AllowedTools: []string{AgentToolName, tools.AgenticFetchToolName, "view", "edit"}}
	coord := &coordinator{}
	cfg := &config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeWorker: {Model: "configured-worker"},
	}}
	require.True(t, coord.workerSubAgentActiveForCall(t.Context(), cfg), "control: a worker is configured")
	require.False(t, coord.workerSubAgentActiveForCall(ctx, cfg), "an exhausted legacy call cannot require worker delegation")
	built := coord.buildToolsAgentConfigForCall(ctx, cfg, original, false)
	require.Equal(t, []string{"view", "edit"}, built.AllowedTools)
	filtered := coord.applyCallDisableSubAgents(ctx, cfg, original, false)
	require.Equal(t, []string{"view", "edit"}, filtered.AllowedTools)
	require.Equal(t, []string{AgentToolName, tools.AgenticFetchToolName, "view", "edit"}, original.AllowedTools, "per-call filtering cannot change the shared snapshot")
}
