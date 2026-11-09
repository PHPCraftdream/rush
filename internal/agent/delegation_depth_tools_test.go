package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDurableDepthMarkerStripsDelegationWithoutCallOptions(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(t.Context(), persistedAgentDepthContextKey{}, 0)
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
